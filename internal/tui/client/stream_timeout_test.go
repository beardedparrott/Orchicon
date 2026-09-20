package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// streamDripper writes an event every 100ms until the client goes away.
type streamDripper struct {
	apiv1connect.UnimplementedExecutionServiceHandler
}

func (streamDripper) StreamExecutionEvents(ctx context.Context, req *connect.Request[v1.StreamExecutionEventsRequest], stream *connect.ServerStream[v1.StreamExecutionEventsResponse]) error {
	seq := int64(0)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		seq++
		if err := stream.Send(&v1.StreamExecutionEventsResponse{Sequence: seq}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// hangingUnary returns only when the caller's request context is canceled.
type hangingUnary struct {
	apiv1connect.UnimplementedExecutionServiceHandler
}

func (hangingUnary) ListExecutions(ctx context.Context, req *connect.Request[v1.ListExecutionsRequest]) (*connect.Response[v1.ListExecutionsResponse], error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStreamsNotCappedByUnaryTimeout is the regression test for the 30s
// live-stream kill: Options.Timeout bounds UNARY RPCs only, never
// server-streams. The client is configured with a 200ms unary deadline;
// a stream that outlives it and delivers an event after t=250ms proves
// the deadline does not cap streaming reads. Under the pre-fix placement
// (whole-request http.Client.Timeout) the body read aborts at 200ms and
// no event can ever arrive past it.
func TestStreamsNotCappedByUnaryTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewExecutionServiceHandler(streamDripper{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Token: "t", Timeout: 200 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	sub, err := c.Executions.StreamExecutionEvents(ctx, connect.NewRequest(&v1.StreamExecutionEventsRequest{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Now()
	var lastAt time.Duration
	n := 0
	for sub.Receive() {
		n++
		lastAt = time.Since(start)
	}
	if n < 3 {
		t.Fatalf("stream delivered only %d events", n)
	}
	if lastAt <= 250*time.Millisecond {
		t.Fatalf("last event arrived at %v — stream appears capped by the %v unary deadline (want events past 250ms)", lastAt, 200*time.Millisecond)
	}
}

// TestUnaryDeadlineApplies checks the positive half: a unary RPC with no
// caller deadline is bounded by Options.Timeout (30ms here) instead of
// hanging forever.
func TestUnaryDeadlineApplies(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewExecutionServiceHandler(hangingUnary{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Timeout: 30 * time.Millisecond})
	start := time.Now()
	_, err := c.Executions.ListExecutions(context.Background(), connect.NewRequest(&v1.ListExecutionsRequest{}))
	if err == nil {
		t.Fatal("expected deadline error on hanging unary RPC")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("unary took %v — deadline interceptor not applied", d)
	}
}
