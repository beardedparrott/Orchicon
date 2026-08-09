package opencode

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// fakeServe emulates the opencode serve HTTP+SSE surface used by a
// follow-up: /global/health (attach check), /event (SSE bus), and
// /session/{id}/prompt_async (fire-and-forget send). The SSE stream emits a
// session.idle for the session after a short delay so collectReply returns.
func fakeServe(t *testing.T, sessionID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/event":
			fl, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl.Flush()
			go func() {
				time.Sleep(30 * time.Millisecond)
				// A completed text part (time.end set — the mapping requires
				// it) so collectReply accumulates the reply.
				fmt.Fprintf(w, "data: {\"id\":\"1\",\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":%q,\"part\":{\"type\":\"text\",\"text\":\"All good — summary follows.\",\"time\":{\"start\":1,\"end\":2}}}}\n\n", sessionID)
				fl.Flush()
				fmt.Fprintf(w, "data: {\"id\":\"2\",\"type\":\"session.idle\",\"properties\":{\"sessionID\":%q}}\n\n", sessionID)
				fl.Flush()
			}()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestContinueSessionFireAndForget verifies the H5 follow-up semantics: the
// RPC returns IMMEDIATELY (the user message is recorded synchronously so the
// chat shows it right away) and the assistant reply is collected in the
// background and appended to the durable transcript — a long model turn can
// never block (and time out) the browser connection.
func TestContinueSessionFireAndForget(t *testing.T) {
	const (
		sessionID = "ses_followup"
		execID    = "exec_followup"
	)
	srv := fakeServe(t, sessionID)

	var mu sync.Mutex
	var stored []db.SessionPart
	a := &Adapter{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionStore: func(_ context.Context, _, _ string, parts []db.SessionPart) error {
			mu.Lock()
			defer mu.Unlock()
			stored = append(stored, parts...)
			return nil
		},
	}

	opts := ContinueSessionOpts{
		ExecutionID: execID,
		TenantID:    "tnt_dev",
		Message:     "Are you done?",
		StartSeq:    5,
		ServeURL:    srv.URL,
		SessionID:   sessionID,
		ProjectDir:  "/tmp",
	}

	started := time.Now()
	reply, err := a.ContinueSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("ContinueSession error: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("ContinueSession blocked for %v — follow-up must be fire-and-forget", time.Since(started))
	}
	// Fire-and-forget: the RPC does not return the (future) reply.
	if reply != "" {
		t.Fatalf("reply = %q, want empty (async)", reply)
	}

	// The user message must be persisted synchronously (seq = StartSeq).
	mu.Lock()
	parts := append([]db.SessionPart(nil), stored...)
	mu.Unlock()
	if len(parts) != 1 || parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 5 {
		t.Fatalf("synchronous parts = %+v, want exactly the user_message at seq 5", parts)
	}

	// The assistant reply lands asynchronously at the NEXT seq (6).
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		parts = append([]db.SessionPart(nil), stored...)
		mu.Unlock()
		if len(parts) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed in the transcript; parts = %+v", parts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	replyPart := parts[1]
	if replyPart.Kind != db.SessionPartText || replyPart.Seq != 6 {
		t.Fatalf("reply part = %+v, want a text part at seq 6", replyPart)
	}
}

// TestContinueSessionNoServeIsSynchronousError verifies that when no serve is
// reachable the RPC returns an immediate error (the UI shows a real error, not
// a hung connection).
func TestContinueSessionNoServeIsSynchronousError(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := a.ContinueSession(context.Background(), ContinueSessionOpts{
		ExecutionID: "exec_x",
		TenantID:    "tnt_dev",
		Message:     "hello",
	})
	if err == nil {
		t.Fatal("expected an error when no serve is available")
	}
	if !strings.Contains(err.Error(), "no opencode serve available") {
		t.Fatalf("unexpected error: %v", err)
	}
}
