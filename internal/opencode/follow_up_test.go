package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
)

// freshSessionID is what the fake serve's POST /session returns — the id of
// the session a follow-up FRESH-SEEDS on the adapter's transport.
const freshSessionID = "ses_fresh"

// fakeServe emulates the opencode serve HTTP+SSE surface used by a follow-up: /global/health
// (attach check), /event (SSE bus), and /session/{id}/prompt_async (fire-and-forget send).
func fakeServe(t *testing.T, sessionID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		case r.URL.Path == "/session/"+sessionID:
			// Re-attach probe: THIS transport still holds the session.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q}`, sessionID)
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			// Fresh-seed creation on the adapter's persistent transport.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q}`, freshSessionID)
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
			// Emit the SSE events synchronously (guarded by the request context) rather than from a detached
			// goroutine.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			// A completed text part (time.end set — the mapping requires
			// it) so collectReply accumulates the reply.
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":%q,\"part\":{\"type\":\"text\",\"text\":\"All good — summary follows.\",\"time\":{\"start\":1,\"end\":2}}}}\n\n", sessionID)
			fl.Flush()
			fmt.Fprintf(w, "data: {\"id\":\"2\",\"type\":\"session.idle\",\"properties\":{\"sessionID\":%q}}\n\n", sessionID)
			fl.Flush()
			// Block until the client cancels so the handler never returns
			// while the writes above are still being flushed.
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestContinueSessionFireAndForget verifies the H5 follow-up semantics: the RPC returns IMMEDIATELY
// (the user message is recorded synchronously so the chat shows it right away) and the assistant.
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

// TestContinueSessionTriggersLazyHostServeAndFailsLoud pins AC 2 + AC 4 for the follow-up
// continuation: a follow-up that cannot reuse the original serve is opencode DEMAND.
func TestContinueSessionTriggersLazyHostServeAndFailsLoud(t *testing.T) {
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Adapter{log: log, host: NewHostServe(log, t.TempDir(), "")}

	_, err := a.ContinueSession(context.Background(), ContinueSessionOpts{
		ExecutionID: "exec_lazy",
		TenantID:    "tnt_dev",
		Message:     "hello",
	})
	if err == nil {
		t.Fatal("ContinueSession with the host serve kill-switched returned nil, want the loud reason")
	}
	if !strings.Contains(err.Error(), "ORCHICON_OPCODE_SESSION_TRANSPORT=0") {
		t.Fatalf("error %q must surface the EnsureStarted kill-switch reason (the follow-up must TRIGGER the lazy start, not skip it)", err)
	}
}

// TestContinueSessionNoServeIsSynchronousError verifies that when no serve is reachable the RPC
// returns an immediate error (the UI shows a real error, not a hung connection).
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

// hostAdapter builds an Adapter whose ONLY transport is the given serve — the adapter's PERSISTENT
// host transport, which is what a follow-up must resolve to (a follow-up belongs.
func hostAdapter(srv *httptest.Server, mu *sync.Mutex, stored *[]db.SessionPart) *Adapter {
	return &Adapter{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		host: &HostServe{client: NewSessionClient(srv.URL, "", "/tmp")},
		sessionStore: func(_ context.Context, _, _ string, parts []db.SessionPart) error {
			mu.Lock()
			defer mu.Unlock()
			*stored = append(*stored, parts...)
			return nil
		},
	}
}

func storedParts(mu *sync.Mutex, stored *[]db.SessionPart) []db.SessionPart {
	mu.Lock()
	defer mu.Unlock()
	return append([]db.SessionPart(nil), *stored...)
}

// TestContinueSessionOpencodeReattachesOnHostTransport pins AC 2's continuity half: an
// opencode-tagged transcript re-attaches the recorded session when the OPENCODE ADAPTER'S OWN
// persistent.
func TestContinueSessionOpencodeReattachesOnHostTransport(t *testing.T) {
	const sessionID = "ses_reattach"
	srv := fakeServe(t, sessionID)
	var mu sync.Mutex
	var stored []db.SessionPart
	a := hostAdapter(srv, &mu, &stored)

	if _, err := a.ContinueSession(context.Background(), ContinueSessionOpts{
		ExecutionID: "exec_reattach",
		TenantID:    "tnt_dev",
		Message:     "are you done?",
		StartSeq:    5,
		SessionID:   sessionID,
		AdapterKind: adapter.KindOpencode,
		ProjectDir:  "/tmp",
	}); err != nil {
		t.Fatalf("ContinueSession error: %v", err)
	}

	parts := storedParts(&mu, &stored)
	if len(parts) != 1 || parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 5 {
		t.Fatalf("synchronous parts = %+v, want ONLY the user_message at seq 5 (re-attached session)", parts)
	}
}

// TestContinueSessionOpencodeSeedsFreshWhenTransportLostSession pins AC 2's fallback half: when the
// recorded session is no longer in the adapter's store (a 404 on the re-attach probe.
func TestContinueSessionOpencodeSeedsFreshWhenTransportLostSession(t *testing.T) {
	// The fake serve holds a DIFFERENT session: the recorded id probes 404.
	srv := fakeServe(t, "ses_somewhere_else")
	var mu sync.Mutex
	var stored []db.SessionPart
	a := hostAdapter(srv, &mu, &stored)

	if _, err := a.ContinueSession(context.Background(), ContinueSessionOpts{
		ExecutionID: "exec_seed",
		TenantID:    "tnt_dev",
		Message:     "are you done?",
		StartSeq:    4,
		SessionID:   "ses_gone",
		ServeURL:    srv.URL,
		AdapterKind: adapter.KindOpencode,
		ProjectDir:  "/tmp",
	}); err != nil {
		t.Fatalf("ContinueSession error: %v", err)
	}

	parts := storedParts(&mu, &stored)
	if len(parts) != 2 {
		t.Fatalf("synchronous parts = %+v, want user_message + fresh session_info", parts)
	}
	if parts[1].Kind != db.SessionPartSessionInfo {
		t.Fatalf("part 2 = %+v, want the fresh session_info part", parts[1])
	}
	var pl map[string]any
	if err := json.Unmarshal(parts[1].Payload, &pl); err != nil {
		t.Fatalf("session_info payload: %v", err)
	}
	if pl["session_id"] != freshSessionID {
		t.Errorf("fresh session_id = %v, want %q", pl["session_id"], freshSessionID)
	}
	if pl["adapter_kind"] != adapter.KindOpencode {
		t.Errorf("fresh session adapter_kind = %v, want %q", pl["adapter_kind"], adapter.KindOpencode)
	}
}

// TestContinueSessionForeignAdapterNeverReattaches pins the adapter isolation rule: a transcript
// recorded by ANOTHER adapter (native "orchicon") must never re-attach its session here.
func TestContinueSessionForeignAdapterNeverReattaches(t *testing.T) {
	const sessionID = "ses_native"
	srv := fakeServe(t, sessionID) // healthy, and DOES hold sessionID
	var mu sync.Mutex
	var stored []db.SessionPart
	a := hostAdapter(srv, &mu, &stored)

	if _, err := a.ContinueSession(context.Background(), ContinueSessionOpts{
		ExecutionID: "exec_foreign",
		TenantID:    "tnt_dev",
		Message:     "are you done?",
		StartSeq:    3,
		SessionID:   sessionID,
		ServeURL:    srv.URL,
		AdapterKind: adapter.KindOrchicon,
		ProjectDir:  "/tmp",
	}); err != nil {
		t.Fatalf("ContinueSession error: %v", err)
	}

	parts := storedParts(&mu, &stored)
	if len(parts) != 2 {
		t.Fatalf("synchronous parts = %+v, want a FRESH seed (user_message + session_info)", parts)
	}
	var pl map[string]any
	if err := json.Unmarshal(parts[1].Payload, &pl); err != nil {
		t.Fatalf("session_info payload: %v", err)
	}
	if pl["session_id"] != freshSessionID {
		t.Errorf("session_id = %v, want a fresh %q (never the other adapter's session)", pl["session_id"], freshSessionID)
	}
}
