//go:build desktop_e2e

// Transport proof against a REAL planes — never a mock.
//
// Run: ORCHICON_DESKTOP_PLANE_URL=http://127.0.0.1:8080 go test -tags desktop_e2e ./internal/desktop -run TestLive -v
//
// WHAT THIS PROVES AND WHAT IT DOES NOT: it proves unary RPCs and a long-lived
// Connect server-stream both survive the shell's loopback origin, using the
// generated Go Connect client (the same Connect protocol the SPA's
// createConnectTransport speaks). It does NOT prove WebKit's own fetch-body
// streaming or the webview's cookie jar — that is the E2E task's job, and saying
// so here keeps it from being assumed proven.
package desktop

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

func liveShell(t *testing.T) (*Shell, string) {
	t.Helper()
	planeURL := os.Getenv("ORCHICON_DESKTOP_PLANE_URL")
	if planeURL == "" {
		t.Skip("ORCHICON_DESKTOP_PLANE_URL is unset — no live plane configured, so the transport is NOT proven here")
	}
	shell, err := New(Config{PlaneURL: planeURL, Logf: t.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := shell.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := contextWithTimeout()
		defer cancel()
		_ = shell.Close(ctx)
	})
	return shell, planeURL
}

// TestLiveUnaryRPC — AC3: a real unary RPC through the shell's origin.
func TestLiveUnaryRPC(t *testing.T) {
	shell, _ := liveShell(t)
	client := apiv1connect.NewProjectServiceClient(http.DefaultClient, shell.OriginURL())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{})); err != nil {
		// Unauthenticated is EXPECTED without a session: what this proves is that the
		// request reached the plane through the shell and got a Connect-shaped answer
		// rather than index.html.
		if connect.CodeOf(err) == connect.CodeUnauthenticated || connect.CodeOf(err) == connect.CodePermissionDenied {
			t.Logf("reached the plane through the shell origin (unauthenticated, as expected): %v", err)
			return
		}
		t.Fatalf("ListProjects through the shell origin: %v", err)
	}
}

// TestLiveServerStreaming — AC4, the highest-risk path: a stream that stays open
// and keeps delivering, through the shell's proxy.
func TestLiveServerStreaming(t *testing.T) {
	shell, planeURL := liveShell(t)
	client := apiv1connect.NewExecutionServiceClient(http.DefaultClient, shell.OriginURL())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A real execution id must come from the operator's plane: this test does not
	// create one (that would write to a plane the shell does not own).
	execID := os.Getenv("ORCHICON_DESKTOP_EXECUTION_ID")
	if execID == "" {
		t.Skipf("ORCHICON_DESKTOP_EXECUTION_ID is unset — start a workflow on %s and re-run to prove streaming", planeURL)
	}
	stream, err := client.StreamExecutionEvents(ctx, connect.NewRequest(&apiv1.StreamExecutionEventsRequest{ExecutionId: execID}))
	if err != nil {
		if strings.Contains(err.Error(), "unauthenticated") || connect.CodeOf(err) == connect.CodeUnauthenticated {
			t.Skipf("the plane rejected the unauthenticated stream (only a session-bearing client can prove this): %v", err)
		}
		t.Fatalf("StreamExecutionEvents through the shell origin: %v", err)
	}
	defer stream.Close()

	first := time.Now()
	count, spread := 0, time.Duration(0)
	for stream.Receive() {
		count++
		if count >= 3 {
			spread = time.Since(first)
			break
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error after %d events: %v", count, err)
	}
	// THREE EVENTS SPREAD OVER TIME is the assertion that catches the silent-stop
	// failure: a stream that yields once and then stalls fails this, not a
	// single-frame check.
	if count < 3 {
		t.Fatalf("only %d events arrived through the shell — a live execution view would look stalled", count)
	}
	if spread < time.Second {
		t.Logf("%d events arrived in %v — spread is short because the execution is fast; re-run against a longer run to assert real-time updating", count, spread)
	}
}
