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
		if len(store.snapshot()) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	parts := store.snapshot()
	if parts[0].Kind != db.SessionPartUserMessage || parts[1].Kind != db.SessionPartText {
		t.Fatalf("parts = %+v, want user_message + text", parts)
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

// TestContinueSessionFailedReplyWritesError pins the failure contract:
// a failed reply collection is written back as an `error` part at the next
// seq (so the UI never hangs on "responding") — the RPC (already returned
// "") never fails after the question is recorded.
func TestContinueSessionFailedReplyWritesError(t *testing.T) {
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
	// The question is persisted synchronously; the collection failure is
	// written back asynchronously as an error part at the next seq.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(store.snapshot()) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("error part never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	parts := store.snapshot()
	if len(parts) != 2 || parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 3 {
		t.Fatalf("parts after failed reply = %+v, want the question part at seq 3", parts)
	}
	if parts[1].Kind != db.SessionPartError || parts[1].Seq != 4 {
		t.Fatalf("parts[1] = %+v, want an error part at seq 4", parts[1])
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
		StartSeq:    7,
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("missing transcript must fall back, got: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(store.snapshot()) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	parts := store.snapshot()
	if parts[0].Kind != db.SessionPartUserMessage || parts[0].Seq != 7 {
		t.Fatalf("parts[0] = %+v, want user_message at seq 7", parts[0])
	}
	if parts[1].Kind != db.SessionPartText || parts[1].Seq != 8 {
		t.Fatalf("parts[1] = %+v, want text at seq 8", parts[1])
	}
	// The context seed rides the user message (file unavailable).
	req := prov.lastRequest()
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text == nil ||
		!strings.Contains(*req.Messages[0].Content[0].Text, "Prior work summary.") {
		t.Fatalf("follow-up messages = %+v, want the context seed", req.Messages)
	}
}

// TestContinueSessionToolLoop pins the follow-up agentic loop: a follow-up
// whose model emits a tool call is executed via the injected Ask tools and
// the turn continues to a final answer — a text-only follow-up would hang
// on the un-executed tool call.
func TestContinueSessionToolLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_test",
	})
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{
			TextDelta{Text: "Let me look."},
			ToolCall{Index: 0, ToolCallID: "fu_1", Name: "list_projects", ArgsJSON: `{}`},
		}, finish: StopToolUse, bare: true},
		{events: []Event{TextDelta{Text: "Three projects found."}}, finish: StopStop, bare: true},
	}}
	store := &storesSessionParts{}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.SetSessionStore(store.record)
	b.SetAskTools(&fakeAskTools{
		defs:    []ToolDef{{Name: "list_projects", ParamsJSON: `{"type":"object"}`}},
		results: map[string]string{"list_projects": `["a","b","c"]`},
	})

	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec_now",
		TenantID:    "tnt_test",
		WorkerID:    "worker_test",
		SessionID:   "exec_prior",
		ModelRef:    "orchicon/mockprov/deepseek-v4-flash",
		Message:     "Which projects?",
		Context:     "Prior work summary.",
		StartSeq:    5,
		ProjectDir:  dir,
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	// The reply lands as a text part (tool round + final consolidated). Wait
	// for it so the async goroutine has completed before asserting the
	// provider was driven twice.
	deadline := time.Now().Add(3 * time.Second)
	var text string
	for {
		var parts []string
		for _, p := range store.snapshot() {
			if p.Kind == db.SessionPartText {
				parts = append(parts, string(p.Payload))
			}
		}
		if len(parts) > 0 {
			text = strings.Join(parts, "\n")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never landed; parts = %+v", store.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(text, "Let me look.") || !strings.Contains(text, "Three projects found.") {
		t.Fatalf("reply = %q, want both rounds' text", text)
	}
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2 (tool round + final)", prov.requestCount())
	}
	// The follow-up request carried the tool defs.
	first := prov.requests[0]
	if len(first.Tools) != 1 || first.Tools[0].Name != "list_projects" {
		t.Fatalf("follow-up tools = %+v, want list_projects", first.Tools)
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
