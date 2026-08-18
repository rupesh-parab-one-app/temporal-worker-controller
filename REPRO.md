# Reproduction: `errors.Is(err, context.DeadlineExceeded)` does not match a Temporal gRPC deadline

This branch is evidence only. It changes no production code.

**Base commit:** `dfb8ce2`, the head of PR #329. It is not based on upstream `main`, so compare against `dfb8ce2` rather than `main` to see only the changes made here.

## What it shows

`shouldEvictClient` in `internal/controller/worker_controller.go` tests
`errors.Is(err, context.DeadlineExceeded)` at line 954. That test cannot match the
error a Temporal SDK call actually returns when it exceeds its deadline, so the
eviction it guards does not happen for the failure reported in #328.

The tests here demonstrate it two ways: by constructing the error shape directly,
and by driving a real SDK client through its real gRPC interceptor chain.

## Why

1. The SDK registers `errorInterceptor` as its outermost unary interceptor
   (`go.temporal.io/sdk@v1.41.1/internal/grpc_dialer.go:94` and `:134`) and rewrites
   every call error through `serviceerror.FromStatus(status.Convert(err))`
   (`:201-208`).
2. `codes.DeadlineExceeded` maps to `newDeadlineExceeded(st)`
   (`go.temporal.io/api@v1.62.8/serviceerror/convert.go:77`).
3. `*serviceerror.DeadlineExceeded`
   (`go.temporal.io/api@v1.62.8/serviceerror/deadline_exceeded.go`) implements only
   `Error()` and `Status()`. It has no `Unwrap()` and no `Is()`, so `errors.Is`
   cannot reach the `context.DeadlineExceeded` sentinel.
4. The message text survives via `st.Message()`, which is why logs read
   `context deadline exceeded` and the code looks correct.

`errors.As(err, &unavailable)` on line 958 is unaffected, because
`*serviceerror.Unavailable` is the concrete type the interceptor produces.

## Scope: one line, not the whole predicate

The other branches survive the conversion:

- `codes.PermissionDenied` maps to a concrete `*serviceerror.PermissionDenied`, so
  the `errors.As` check in `isAccessDeniedErr` still fires.
- `codes.Unauthenticated` deliberately falls through to `st.Err()`, which yields a
  `*status.Error`. That type implements `GRPCStatus()`, so the
  `grpcstatus.Code(err) == codes.Unauthenticated` fallback still fires.
- `codes.Unavailable` maps to `newUnavailable(st)`, so `errors.As` matches. One
  caveat: when the status carries a `NamespaceUnavailableFailure` detail,
  `convert.go` returns `*serviceerror.NamespaceUnavailable` instead, which
  `errors.As(err, &unavailable)` does not match either.

## Which call sites are affected

- `worker_controller.go:319`, the `GetWorkerDeploymentState` error path. A real
  Temporal deadline does not match. This is also the only Temporal call with no SDK
  retry config: it goes through `WorkflowService()` directly
  (`internal/temporal/worker_deployment.go:82`), so `NewRetryOptionsInterceptor`
  calls `grpc_retry.Disable()`.
- `worker_controller.go:365`, the `executePlan` error path. Mixed: `executePlan`
  makes both Kubernetes writes and Temporal calls (`SetManagerIdentity` at
  `execplan.go:229`, `SetCurrentVersion` at `:260`, `SetRampingVersion` at `:282`,
  `UpdateVersionMetadata` at `:303`). A Temporal-origin deadline does not match; a
  Kubernetes-origin one does, because `net/http` wraps the context error in a
  `*url.Error` whose `Unwrap` exposes it.
- `worker_controller.go:596`, the `handleDeletion` defer. A real Temporal deadline
  does not match. This is the path the reported incident took: the retries logged
  in #328 came from `worker_controller.go:158`, in the deletion branch.

Being on the deletion path does not mean SDK retries absorbed it first. All nine
deployment-handle methods do install the retry config
(`go.temporal.io/sdk@v1.41.1/internal/internal_worker_deployment_client.go`, e.g.
`:181`), but `codes.DeadlineExceeded` is deliberately excluded from the retryable
set, with the comment that context errors "are not retriable based on user
settings" (`internal/common/retry/interceptor.go:103-108`). So the SDK forwards a
deadline unretried, and the controller's check then fails to match it.

So the check misses every Temporal-origin deadline, and becomes true only for a
Kubernetes-origin deadline, where evicting the Temporal client achieves nothing.

Dial failures do not reach the predicate at all: in `handleDeletion` a dial error
returns before the defer is registered, and in `Reconcile` both checks sit on
post-dial calls.

## Changes on this branch

All in `internal/controller`, tests only.

- `reconciler_events_test.go`
  - `wireDeadlineExceeded()` helper building the error the way the interceptor does.
  - `TestShouldEvictClient/DeadlineExceededFromGRPC`, asserting `want: false`.
  - `TestShouldEvictClient/WireDeadlineIsNotTheContextSentinel`, asserting the
    mechanism: `errors.Is` false, `errors.As` true, message text preserved.
  - `TestReconcile_DoesNotEvictOnWireDeadline`, the wire-shape sibling of
    `TestReconcile_EvictsCachedClientOnTransportFailure`.
  - `TestHandleDeletion_EvictsCachedClientOnTemporalFailure/DescribeWireDeadline_RetainsClient`,
    the wire-shape sibling of `DescribeError_EvictsClient`.
- `wire_deadline_repro_test.go`
  - `TestSDKErrorShapesFromRealCalls`, driving a real SDK client over an in-memory
    `bufconn` transport with no Temporal server required.

The two pre-existing tests are left untouched and still pass. They inject the raw
`context.DeadlineExceeded` sentinel, which is why they report that eviction works.
The new siblings inject the shape a real call produces and assert the opposite
outcome, so the contrast is visible side by side.

**The new expectations are a characterization of current behaviour, not a
statement of intent.** A fix to the eviction predicate is expected to flip
`DeadlineExceededFromGRPC` to `want: true`, and to flip both `RetainsClient`
siblings to assert eviction.

## Reproduce

```
go test ./internal/controller \
  -run 'Test(ShouldEvictClient|Reconcile_EvictsCachedClientOnTransportFailure|Reconcile_DoesNotEvictOnWireDeadline|HandleDeletion_EvictsCachedClientOnTemporalFailure|SDKErrorShapesFromRealCalls)' \
  -count=1 -v
```

`go.mod` requires Go 1.26.4.

## Recorded output

```
=== RUN   TestShouldEvictClient
=== RUN   TestShouldEvictClient/DeadlineExceeded
=== RUN   TestShouldEvictClient/DeadlineExceededFromGRPC
=== RUN   TestShouldEvictClient/WireDeadlineIsNotTheContextSentinel
    reconciler_events_test.go:473: concrete type reaching the controller: *serviceerror.DeadlineExceeded
--- PASS: TestShouldEvictClient (0.00s)
    --- PASS: TestShouldEvictClient/DeadlineExceeded (0.00s)
    --- PASS: TestShouldEvictClient/DeadlineExceededFromGRPC (0.00s)
    --- PASS: TestShouldEvictClient/WireDeadlineIsNotTheContextSentinel (0.00s)
=== RUN   TestReconcile_EvictsCachedClientOnTransportFailure
--- PASS: TestReconcile_EvictsCachedClientOnTransportFailure (0.06s)
=== RUN   TestReconcile_DoesNotEvictOnWireDeadline
--- PASS: TestReconcile_DoesNotEvictOnWireDeadline (0.00s)
=== RUN   TestHandleDeletion_EvictsCachedClientOnTemporalFailure
--- PASS: TestHandleDeletion_EvictsCachedClientOnTemporalFailure (0.00s)
    --- PASS: TestHandleDeletion_EvictsCachedClientOnTemporalFailure/DescribeError_EvictsClient (0.00s)
    --- PASS: TestHandleDeletion_EvictsCachedClientOnTemporalFailure/DescribeWireDeadline_RetainsClient (0.00s)
    --- PASS: TestHandleDeletion_EvictsCachedClientOnTemporalFailure/DescribeNotFound_RetainsClient (0.00s)
=== RUN   TestSDKErrorShapesFromRealCalls
=== RUN   TestSDKErrorShapesFromRealCalls/Unreachable_YieldsUnavailable
    wire_deadline_repro_test.go:119: concrete type: *serviceerror.Unavailable
    wire_deadline_repro_test.go:120: message: connection error: desc = "transport: Error while dialing: connection refused by test dialer"
=== RUN   TestSDKErrorShapesFromRealCalls/HungCall_YieldsDeadlineExceeded
    wire_deadline_repro_test.go:141: concrete type: *serviceerror.DeadlineExceeded
    wire_deadline_repro_test.go:142: message: context deadline exceeded
--- PASS: TestSDKErrorShapesFromRealCalls (2.00s)
    --- PASS: TestSDKErrorShapesFromRealCalls/Unreachable_YieldsUnavailable (0.00s)
    --- PASS: TestSDKErrorShapesFromRealCalls/HungCall_YieldsDeadlineExceeded (2.00s)
PASS
ok  	github.com/temporalio/temporal-worker-controller/internal/controller	3.236s
```

The last two lines are the ones that matter. A real SDK call whose deadline fires
returns `*serviceerror.DeadlineExceeded` with the message `context deadline
exceeded` — identical text to the sentinel, different type. An unreachable
endpoint returns `*serviceerror.Unavailable`, which the predicate does match.

## What this branch does not propose

It takes no position on the fix. Widening the predicate to match
`*serviceerror.DeadlineExceeded`, removing `Unavailable`, or something else are all
open, and are better decided in #329.
