package orchicon

// follow_up_test.go: regression tests for the real NativeBridge follow-up
// (NativeBridge.ContinueSession). Pins the follow-up semantics:
//   - The fire-and-forget contract: the RPC returns "" immediately (the
//     reply flows via the durable transcript, never the RPC field) while
//     the session runs asynchronously on a detached context.
//   - The follow-up runs as a FULL worker session: the question (source
//     "follow_up") and the reply are mirrored into the DB session parts by
//     the session's transcript recorder.
//   - Identity isolation: a cross-worker transcript is refused.
//   - A missing session id / transcript falls back to a fresh session.
//   - A failed session writes an error part — the RPC never fails after the
//     session is built.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// storesSessionParts is a concurrency-safe sessionStore capture.
type storesSessionParts struct {
	mu    sync.Mutex
	parts []db.SessionPart
}

func (s *storesSessionParts) record(_ context.Context, _ string, _ string, parts []db.SessionPart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parts = append(s.parts, parts...)
	return nil
}

func (s *storesSessionParts) snapshot() []db.SessionPart {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]db.SessionPart(nil), s.parts...)
}

// TestContinueSessionRecordsQuestionAndReplyPins the real follow-up: the
// question (source: follow_up) and the assistant reply are mirrored into
// the DB session parts by the session's transcript recorder.
func TestContinueSessionRecordsQuestionAndReply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_now.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_now",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_test",
	})
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Absolutely — here is the follow-up."}}, finish: StopStop, usage: Usage{InputTokens: 3, OutputTokens: 8}, bare: true},
	}}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	started := time.Now()
	reply, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID:  "exec_now",
		TenantID:     "tnt_test",
		WorkerID:     "worker_test",
		SessionID:    "exec_now",
		ModelRef:     "orchicon/mockprov/deepseek-v4-flash",
		Message:      "Are you done?",
		SystemPrompt: "You are QA.",
		ProjectDir:   dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession error: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("ContinueSession blocked for %v — follow-up must be fire-and-forget", time.Since(started))
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty (async — reply flows via transcript)", reply)
	}

	// The follow-up question is recorded as a user_message part (source
	// follow_up) and the reply as a text part, via the recorder.
	deadline := time.Now().Add(3 * time.Second)
	var umText, replyText string
	for {
		parts := store.snapshot()
		for _, p := range parts {
			if p.Kind == db.SessionPartUserMessage {
				var um map[string]any
				if jsonUnmarshal(p.Payload, &um) == nil {
					if s, _ := um["source"].(string); s == "follow_up" {
						umText, _ = um["text"].(string)
					}
				}
			}
			if p.Kind == db.SessionPartText {
				var tp map[string]any
				if jsonUnmarshal(p.Payload, &tp) == nil {
					if inner, ok := tp["part"].(map[string]any); ok {
						replyText, _ = inner["text"].(string)
					}
				}
			}
		}
		if umText != "" && replyText != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("question/reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if umText != "Are you done?" {
		t.Fatalf("follow-up question = %q, want the user's message verbatim (no injected frame)", umText)
	}
	if replyText != "Absolutely — here is the follow-up." {
		t.Fatalf("follow-up reply = %q, want the collected reply", replyText)
	}

	// The provider turn carried the follow-up question as a user message.
	req := prov.lastRequest()
	if len(req.Messages) == 0 || req.Messages[0].Role != RoleUser {
		t.Fatalf("follow-up messages = %+v, want a leading user message", req.Messages)
	}
	if req.Messages[0].Content[0].Text == nil || !strings.Contains(*req.Messages[0].Content[0].Text, "Are you done?") {
		t.Fatalf("follow-up user message = %v, want the (wrapped) question", req.Messages[0])
	}
}

// TestContinueSessionRefusesCrossWorker pins identity isolation: a
// follow-up against a transcript whose identity belongs to a DIFFERENT
// worker is refused.
func TestContinueSessionRefusesCrossWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
		WorkerID:    "worker_A",
		WorkerName:  "worker-a",
		TenantID:    "tnt_test",
	})
	b := NewBridge(nil, dir, nil)
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_B", // different worker → must refuse
		SessionID:   "exec_prior",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Are you done?",
		ProjectDir:  dir,
	})
	if err == nil {
		t.Fatal("cross-worker continuation must be refused")
	}
	if !strings.Contains(err.Error(), "identity isolation") {
		t.Fatalf("cross-worker error = %q, want identity isolation", err.Error())
	}
}

// TestContinueSessionMissingSessionIDFallsBack pins the context fallback:
// with no prior session id there is no transcript file to verify against,
// so the follow-up proceeds as a context one-shot instead of refusing —
// old runs without a session_info part stay answerable.
func TestContinueSessionMissingSessionIDFallsBack(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Context-only reply."}}, finish: StopStop, bare: true},
	}}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	reply, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Are you done?",
		Context:     "Prior work summary.",
		StartSeq:    1,
	})
	if err != nil {
		t.Fatalf("ContinueSession without session id must fall back, got: %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty (async)", reply)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if hasKind(store.snapshot(), db.SessionPartText) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasKind(store.snapshot(), db.SessionPartUserMessage) {
		t.Fatalf("parts = %+v, want a user_message + text", store.snapshot())
	}
}

// hasKind reports whether the parts slice contains a part of the given kind.
func hasKind(parts []db.SessionPart, kind string) bool {
	for _, p := range parts {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

// TestContinueSessionNoModelRefErrors pins the missing-model-ref guard.
func TestContinueSessionNoModelRefErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_test",
	})
	b := NewBridge(nil, dir, nil)
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_prior",
		ProjectDir:  dir,
		Message:     "Are you done?",
	})
	if err == nil {
		t.Fatal("missing model ref must error")
	}
	if !strings.Contains(err.Error(), "no provider/model") {
		t.Fatalf("missing model ref error = %q", err.Error())
	}
}

// TestContinueSessionFailedReplyWritesError pins the failure contract:
// a session whose provider stream fails is written back as an `error` part
// (via the recorder) so the UI never hangs on "responding" — the RPC
// (already returned "") never fails after the session is built.
func TestContinueSessionFailedReplyWritesError(t *testing.T) {
	dir := t.TempDir()
	prov := &mockProvider{preStreamErrOnCall: 1}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	reply, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_now",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Are you done?",
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession error after question recorded: %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty", reply)
	}
	// The session's loop writes an error part on provider failure.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if hasKind(store.snapshot(), db.SessionPartError) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("error part never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestContinueSessionMissingTranscriptFallsBack pins the container-run
// case: the JSONL is not on the host project dir, so identity cannot be
// verified from the file — the follow-up still proceeds from the durable
// DB context instead of refusing.
func TestContinueSessionMissingTranscriptFallsBack(t *testing.T) {
	dir := t.TempDir() // no transcript file written
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Fallback reply."}}, finish: StopStop, bare: true},
	}}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_missing",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Status?",
		Context:     "Prior work summary.",
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("missing transcript must fall back, got: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if hasKind(store.snapshot(), db.SessionPartText) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasKind(store.snapshot(), db.SessionPartUserMessage) {
		t.Fatalf("parts = %+v, want a user_message + text", store.snapshot())
	}
}

// TestContinueSessionToolLoop pins the follow-up as a FULL worker session:
// the model's tool call is executed through the session's tool registry
// (the worker's host tools) and the turn continues to a final answer — a
// text-only one-shot would hang on the un-executed tool call.
func TestContinueSessionToolLoop(t *testing.T) {
	dir := t.TempDir()
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{
			TextDelta{Text: "Let me look."},
			ToolCall{Index: 0, ToolCallID: "fu_1", Name: "bash", ArgsJSON: `{"command":"echo hi"}`},
		}, finish: StopToolUse, bare: true},
		{events: []Event{TextDelta{Text: "Three projects found."}}, finish: StopStop, bare: true},
	}}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_now",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Which projects?",
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	// The reply lands as a text part. Wait for it so the async goroutine has
	// completed before asserting the provider was driven twice.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if hasKind(store.snapshot(), db.SessionPartText) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2 (tool round + final)", prov.requestCount())
	}
	// The follow-up request carried the WORKER's tools (host suite), not the
	// Ask product tools — a true live session with the same abilities.
	first := prov.requests[0]
	if len(first.Tools) == 0 {
		t.Fatalf("follow-up request carried no tools; want the worker's host tools")
	}
}

// blockingTurnStream blocks on Next until ctx is done, then returns the ctx
// error — the shape of a provider stream that never emits a terminal event.
type blockingTurnStream struct{}

func (s *blockingTurnStream) Next(ctx context.Context) (Event, bool, error) {
	<-ctx.Done()
	return nil, false, ctx.Err()
}
func (s *blockingTurnStream) Close() error { return nil }

// TestContinueSessionHangingStreamIsTornDown pins the ctx-aware teardown: a
// provider stream that never emits a terminal event must not hang the
// collection — when ctx is done the stream is closed and the turn surfaces
// an error part instead of waiting forever.
func TestContinueSessionHangingStreamIsTornDown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_test",
	})
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return &blockingProvider{}, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)

	// Shrink the reply window so the test is fast.
	t.Setenv("ORCHICON_FOLLOWUP_REPLY_WINDOW", "200ms")
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_prior",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Status?",
		Context:     "Prior work.",
		StartSeq:    1,
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	// The hanging stream must be torn down on the reply-window timeout and
	// an error part written (the UI never hangs forever).
	deadline := time.Now().Add(2 * time.Second)
	for {
		var sawErr bool
		for _, p := range store.snapshot() {
			if p.Kind == db.SessionPartError {
				sawErr = true
			}
		}
		if sawErr {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("error part never written for a hanging stream; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockingProvider is a Provider whose StreamTurn returns a stream that
// never emits a terminal event (blocks until ctx is done).
type blockingProvider struct{}

func (b *blockingProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	return &blockingTurnStream{}, nil
}
func (b *blockingProvider) ListModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }
func (b *blockingProvider) Capabilities() Capabilities                          { return Capabilities{Streaming: true} }
