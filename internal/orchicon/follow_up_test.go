package orchicon

// follow_up_test.go: regression tests for the real NativeBridge follow-up
// (NativeBridge.ContinueSession). Pins the H5 follow-up semantics:
//   - The fire-and-forget contract: the RPC returns "" immediately (the
//     reply flows via the durable transcript, never the RPC field) while
//     the model turn is collected asynchronously on a detached context.
//   - The question is recorded UP FRONT at seq=opts.StartSeq with
//     source:"follow_up"; the assistant reply lands at the NEXT seq as a
//     `text` part with part.text == the collected reply.
//   - Identity isolation: a cross-worker transcript is refused.
//   - A missing session id errors.
//   - A failed reply collection logs a warning and leaves the question
//     part — the RPC never fails after the question is recorded.

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
// question (source: follow_up) is recorded synchronously at opts.StartSeq
// and the assistant reply is collected asynchronously at the next seq.
func TestContinueSessionRecordsQuestionAndReply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
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
		SessionID:    "exec_prior",
		ModelRef:     "orchicon/mockprov/deepseek-v4-flash",
		Message:      "Are you done?",
		SystemPrompt: "You are QA.",
		StartSeq:     5,
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

	// The user message is persisted synchronously (seq = StartSeq, source
	// follow_up). It is ALWAYS parts[0] because the synchronous write
	// happens before the reply goroutine is even spawned; the count after
	// it is non-deterministic (the fire-and-forget reply may already have
	// landed), so only pin parts[0] here and let the poll below settle the
	// rest.
	parts := store.snapshot()
	if len(parts) < 1 || parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 5 {
		t.Fatalf("synchronous parts = %+v, want the user_message first at seq 5", parts)
	}
	var um map[string]any
	if err := jsonUnmarshal(parts[0].Payload, &um); err != nil {
		t.Fatalf("user_message payload: %v", err)
	}
	if um["text"] != "Are you done?" || um["source"] != "follow_up" {
		t.Fatalf("user_message payload = %v, want text/source follow_up", um)
	}

	// The assistant reply lands asynchronously at the NEXT seq (6) as a
	// text part carrying the collected text.
	deadline := time.Now().Add(3 * time.Second)
	for {
		parts = store.snapshot()
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
	var tp map[string]any
	if err := jsonUnmarshal(replyPart.Payload, &tp); err != nil {
		t.Fatalf("reply payload: %v", err)
	}
	inner, _ := tp["part"].(map[string]any)
	if inner == nil || inner["text"] != "Absolutely — here is the follow-up." {
		t.Fatalf("reply payload = %v, want part.text == collected reply", tp)
	}

	// The provider turn carried the follow-up system prompt + the question
	// as the user message.
	req := prov.lastRequest()
	if len(req.System) == 0 || req.System[0].Text != "You are QA." {
		t.Fatalf("follow-up system = %+v, want opts.SystemPrompt", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
		t.Fatalf("follow-up messages = %+v, want exactly one user message", req.Messages)
	}
	if req.Messages[0].Content[0].Text == nil || *req.Messages[0].Content[0].Text != "Are you done?" {
		t.Fatalf("follow-up user message = %v, want the question", req.Messages[0])
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

// TestContinueSessionMissingSessionIDErrors pins the missing-session-id
// guard.
func TestContinueSessionMissingSessionIDErrors(t *testing.T) {
	b := NewBridge(nil, t.TempDir(), nil)
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Are you done?",
	})
	if err == nil {
		t.Fatal("missing session id must error")
	}
	if !strings.Contains(err.Error(), "requires a prior session id") {
		t.Fatalf("missing session id error = %q", err.Error())
	}
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

// TestContinueSessionFailedReplyLeavesQuestion pins the failure contract:
// a failed reply collection logs a warning and leaves the question part —
// the RPC (already returned "") never fails after the question is
// recorded, and the transcript keeps the question.
func TestContinueSessionFailedReplyLeavesQuestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_test",
	})
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
		SessionID:   "exec_prior",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Are you done?",
		StartSeq:    3,
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession error after question recorded: %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty", reply)
	}
	// The question is persisted even though the reply collection failed.
	parts := store.snapshot()
	if len(parts) != 1 || parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 3 {
		t.Fatalf("parts after failed reply = %+v, want the question part at seq 3", parts)
	}
}
