package orchicon

// Tests for native Ask conversation compaction (scheduler.ChatCompactor).

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// errSummarizeTestFailure is the pre-stream failure the summarize-turn tests
// inject to prove a failed summarize never rewrites the history.
var errSummarizeTestFailure = errors.New("summarize stream failed")

func compactTestStr(s string) *string { return &s }

// newCompactBridge builds a bridge whose provider returns summary and records
// the requests it received.
func newCompactBridge(t *testing.T, prov *chatTestProvider) *NativeBridge {
	t.Helper()
	resolver := ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	})
	return NewBridge(resolver, "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
}

// seedCompactHistory seeds n messages, including one image and one tool result,
// and returns the session id.
func seedCompactHistory(t *testing.T, b *NativeBridge, dir string, n int) string {
	t.Helper()
	if dir != "" {
		b.SetAskHistoryDir(dir)
	}
	const convID = "01COMPACTTESTCONV000000000"
	ctx := context.Background()
	sid, err := b.CreateConversationSession(ctx, convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	hist := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		hist = append(hist, Message{Role: role, Content: []Content{{Text: compactTestStr("message body")}}})
	}
	// One image and one oversized tool result — the two classes the reduce
	// stage exists to drop.
	hist[2].Content = append(hist[2].Content, Content{Image: compactTestStr(strings.Repeat("A", 4096))})
	hist[3].Content = append(hist[3].Content, Content{ToolResult: &ContentToolResult{
		ToolCallID: "c1", Content: strings.Repeat("tool output ", 512),
	}})
	b.mu.Lock()
	b.chatHistory[sid] = hist
	if dir != "" {
		b.persistAskHistoryLocked(sid)
	}
	b.mu.Unlock()
	return sid
}

func TestReduceAskHistoryDropsImagesAndToolPayloads(t *testing.T) {
	hist := []Message{
		{Role: RoleUser, Content: []Content{{Text: compactTestStr("keep this text")}}},
		{Role: RoleUser, Content: []Content{{Image: compactTestStr(strings.Repeat("A", 100000))}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{Content: strings.Repeat("x", 500000)}}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{Name: "grep", ArgsJSON: strings.Repeat("y", 50000)}}}},
	}
	transcript, before, after := reduceAskHistory(hist)

	// Text survives in full.
	if !strings.Contains(transcript, "keep this text") {
		t.Error("text content must be preserved verbatim")
	}
	// Non-text classes collapse to explicit markers (never silently dropped, so
	// the summarizer can say what is missing).
	for _, want := range []string{"image content dropped", "output dropped", "arguments dropped"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing marker %q", want)
		}
	}
	// The whole point: the reduction is orders of magnitude, and the payloads
	// must not appear.
	if strings.Contains(transcript, strings.Repeat("x", 100)) {
		t.Error("tool payload must not survive into the transcript")
	}
	if before <= after {
		t.Errorf("reduction must shrink the transcript: before=%d after=%d", before, after)
	}
	if after > 2000 {
		t.Errorf("reduced transcript is %d bytes; expected the payloads to be replaced by short markers", after)
	}
}

func TestCompactConversationSessionReplacesHistory(t *testing.T) {
	dir := t.TempDir()
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "SUMMARY OF EVERYTHING"}}}
	b := newCompactBridge(t, prov)
	sid := seedCompactHistory(t, b, dir, 20)

	ctx := tenant.WithID(context.Background(), "tnt")
	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: "01COMPACTTESTCONV000000000",
		SessionID:      sid,
		ModelRef:       "orchicon/deepseek/deepseek-flash",
		Reason:         "manual",
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected Compacted=true, detail=%q", res.Detail)
	}
	if !strings.Contains(res.Summary, "SUMMARY OF EVERYTHING") {
		t.Errorf("summary = %q, want the model's text", res.Summary)
	}
	// Measured token fields stay 0: the native path has no real measurement and
	// must never report an estimate as measured.
	if res.TokensBefore != 0 || res.TokensAfter != 0 {
		t.Errorf("token fields = (%d,%d), want (0,0) — never estimated", res.TokensBefore, res.TokensAfter)
	}

	// The history is now [summary marker] + the most recent 6 messages.
	b.mu.Lock()
	got := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if len(got) != askCompactTailMessages+1 {
		t.Fatalf("history len = %d, want %d (summary + tail)", len(got), askCompactTailMessages+1)
	}
	if got[0].Role != RoleAssistant || got[0].Content[0].Text == nil ||
		!strings.Contains(*got[0].Content[0].Text, "SUMMARY OF EVERYTHING") {
		t.Error("first message must be the summary marker as an assistant message")
	}
	if !strings.Contains(*got[0].Content[0].Text, "Earlier conversation compacted") {
		t.Error("summary marker must be framed as established context, not a fresh instruction")
	}

	// Exactly one summarize call, with no tools advertised (a summarize turn
	// must not start doing work).
	if prov.requestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", prov.requestCount())
	}
	if len(prov.lastRequest().Tools) != 0 {
		t.Error("the summarize turn must advertise no tools")
	}

	// The replaced history is persisted, and the pre-collapse file is archived
	// (the summary is lossy by design, so the detail stays recoverable).
	live := filepath.Join(dir, askHistoryFilename(sid)+".json")
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("live history file must exist after compaction: %v", err)
	}
	if !strings.Contains(string(raw), "SUMMARY OF EVERYTHING") {
		t.Error("persisted history must carry the summary")
	}
	if matches, _ := filepath.Glob(live + ".compacted-*.bak"); len(matches) != 1 {
		t.Errorf("archived pre-collapse history = %v, want exactly 1", matches)
	}
}

func TestCompactConversationSessionDeclinesShortHistory(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "unused"}}}
	b := newCompactBridge(t, prov)
	sid := seedCompactHistory(t, b, "", 4)

	ctx := tenant.WithID(context.Background(), "tnt")
	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: "01COMPACTTESTCONV000000000", SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.Compacted {
		t.Error("a short conversation must not be compacted")
	}
	if prov.requestCount() != 0 {
		t.Error("declining must not spend a model call")
	}
}

func TestCompactConversationSessionEmptySummaryLeavesHistoryUntouched(t *testing.T) {
	prov := &chatTestProvider{events: nil} // the model streams nothing
	b := newCompactBridge(t, prov)
	sid := seedCompactHistory(t, b, "", 20)

	b.mu.Lock()
	before := len(b.chatHistory[sid])
	b.mu.Unlock()

	ctx := tenant.WithID(context.Background(), "tnt")
	if _, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: "01COMPACTTESTCONV000000000", SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	}); err == nil {
		t.Fatal("an empty summary must be an error, not a silent destructive success")
	}
	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after != before {
		t.Errorf("history was modified (%d -> %d) despite the empty summary", before, after)
	}
}

func TestCompactConversationSessionProviderErrorLeavesHistoryUntouched(t *testing.T) {
	prov := &chatTestProvider{preErr: errSummarizeTestFailure}
	b := newCompactBridge(t, prov)
	sid := seedCompactHistory(t, b, "", 20)

	b.mu.Lock()
	before := len(b.chatHistory[sid])
	b.mu.Unlock()

	ctx := tenant.WithID(context.Background(), "tnt")
	if _, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: "01COMPACTTESTCONV000000000", SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	}); err == nil {
		t.Fatal("a summarize failure must surface (the conversation is still wedged, and must not be silently rewritten)")
	}
	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after != before {
		t.Errorf("history was modified (%d -> %d) despite the summarize failure", before, after)
	}
}

func TestCompactConversationSessionWithoutTenantFails(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "x"}}}
	b := newCompactBridge(t, prov)
	sid := seedCompactHistory(t, b, "", 20)

	// No tenant in context: the provider cannot be resolved, and the call must
	// fail rather than summarize against the wrong credential scope.
	if _, err := b.CompactConversationSession(context.Background(), scheduler.CompactConversationOpts{
		ConversationID: "01COMPACTTESTCONV000000000", SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	}); err == nil {
		t.Fatal("compaction without a tenant in context must fail")
	}
}
