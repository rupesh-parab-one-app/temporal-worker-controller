// Unless explicitly stated otherwise all files in this repository are licensed under the MIT License.
//
// This product includes software developed at Datadog (https://www.datadoghq.com/). Copyright 2024 Datadog, Inc.

package controller

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// newHangingTemporalClient returns an SDK client whose every RPC reaches a gRPC
// server that never answers.
//
// No service is registered, so all methods fall through to the unknown-service
// handler, which blocks until the caller gives up. bufconn keeps the transport
// in memory, so there is no OS socket timing to make results flaky, and
// NewLazyClient is required because Dial eagerly fetches server capabilities
// that this server never returns.
func newHangingTemporalClient(t *testing.T) sdkclient.Client {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(
		func(_ any, stream grpc.ServerStream) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	c, err := sdkclient.NewLazyClient(sdkclient.Options{
		// HostPort is handed to grpc.NewClient verbatim, so the passthrough
		// scheme is what routes the target to the bufconn dialer instead of
		// through DNS resolution, which would find no addresses for a fake name.
		HostPort:  "passthrough:///bufnet",
		Namespace: "default",
		ConnectionOptions: sdkclient.ConnectionOptions{
			TLSDisabled: true,
			DialOptions: []grpc.DialOption{
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					return lis.DialContext(ctx)
				}),
			},
		},
	})
	require.NoError(t, err, "constructing a lazy client must not require a reachable server")
	t.Cleanup(c.Close)

	return c
}

// newUnreachableTemporalClient returns an SDK client whose dialer always fails,
// so an RPC cannot connect and fails fast instead of running out its deadline.
func newUnreachableTemporalClient(t *testing.T) sdkclient.Client {
	t.Helper()

	c, err := sdkclient.NewLazyClient(sdkclient.Options{
		HostPort:  "passthrough:///unreachable",
		Namespace: "default",
		ConnectionOptions: sdkclient.ConnectionOptions{
			TLSDisabled: true,
			DialOptions: []grpc.DialOption{
				grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
					return nil, errors.New("connection refused by test dialer")
				}),
			},
		},
	})
	require.NoError(t, err, "constructing a lazy client must not require a reachable server")
	t.Cleanup(c.Close)

	return c
}

func describeAnyDeployment(ctx context.Context, c sdkclient.Client, opts ...grpc.CallOption) error {
	_, err := c.WorkflowService().DescribeWorkerDeployment(ctx, &workflowservice.DescribeWorkerDeploymentRequest{
		Namespace:      "default",
		DeploymentName: "any-deployment",
	}, opts...)
	return err
}

// TestSDKErrorShapesFromRealCalls drives a real Temporal SDK client through its
// real gRPC interceptor chain and records the concrete error type a caller
// receives for the two transport failures the eviction predicate cares about.
//
// The table tests in reconciler_events_test.go construct those errors directly,
// which shows what the predicate does with a given value but not that the
// controller ever receives that value from a live call. This test closes that
// gap without needing a Temporal server.
func TestSDKErrorShapesFromRealCalls(t *testing.T) {
	// A connection that cannot be established fails fast rather than running
	// out the deadline, and arrives as *serviceerror.Unavailable. errors.As
	// matches that type, so the eviction predicate does fire for this class.
	t.Run("Unreachable_YieldsUnavailable", func(t *testing.T) {
		c := newUnreachableTemporalClient(t)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := describeAnyDeployment(ctx, c)
		require.Error(t, err, "the call must fail because the connection cannot be established")
		t.Logf("concrete type: %T", err)
		t.Logf("message: %v", err)

		var unavailable *serviceerror.Unavailable
		assert.True(t, errors.As(err, &unavailable),
			"a fail-fast connection error arrives as *serviceerror.Unavailable")
		assert.True(t, shouldEvictClient(err),
			"the predicate fires for this class, via errors.As")
	})

	// The connection establishes and the handler never answers, so the call runs
	// out its deadline. That is the shape the reported incident produced.
	// WaitForReady only removes a race where the first RPC is issued before the
	// channel reaches READY, which would fail fast instead.
	t.Run("HungCall_YieldsDeadlineExceeded", func(t *testing.T) {
		c := newHangingTemporalClient(t)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := describeAnyDeployment(ctx, c, grpc.WaitForReady(true))
		require.Error(t, err, "the call must fail once the deadline fires")
		t.Logf("concrete type: %T", err)
		t.Logf("message: %v", err)

		var deadlineExceeded *serviceerror.DeadlineExceeded
		assert.True(t, errors.As(err, &deadlineExceeded),
			"the SDK converts a gRPC deadline into *serviceerror.DeadlineExceeded")

		assert.False(t, errors.Is(err, context.DeadlineExceeded),
			"that type implements neither Unwrap nor Is, so the context sentinel does not match")

		assert.False(t, shouldEvictClient(err),
			"so the eviction predicate does not fire for a deadline produced by a real call")
	})
}
