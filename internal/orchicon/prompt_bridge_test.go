package orchicon

// Cache metrics through the bridge boundary: recordUsage must fold the
// session's per-turn cache stats into the UsageRecord (ADR-0009 D6) —
// replacing the old all-zero placeholder. The native-memory note tool is
// session-scoped and must never reach the MCP registry (loop-answered).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

type captureRecorder struct {
	rec []scheduler.UsageRecord
}

func (c *captureRecorder) fn(ctx context.Context, in scheduler.UsageRecord) error {
	c.rec = append(c.rec, in)
	return nil
}

func TestQABridgeRecordUsageCarriesCacheTokens(t *testing.T) {
	dir := t.TempDir()
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "ok"}}, finish: StopStop, usage: Usage{InputTokens: 40, OutputTokens: 6, CacheReadTokens: 1000, CacheWriteTokens: 250}},
	}}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, nil)
	cap := &captureRecorder{}
	b.SetUsageRecorder(cap.fn)

	exec := testExecRow("exec_usage")
	mf := testManifest("orchicon/mockprov/deepseek-v4-flash")
	mf.ProjectDir = dir
	if err := b.Start(context.Background(), exec, mf, &recordedCallback{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(cap.rec) != 1 {
		t.Fatalf("usage records = %d, want 1", len(cap.rec))
	}
	r := cap.rec[0]
	if r.PromptTokens != 40 || r.CompletionTokens != 6 {
		t.Errorf("tokens = %d/%d, want 40/6", r.PromptTokens, r.CompletionTokens)
	}
	if r.CacheReadTokens != 1000 || r.CacheWriteTokens != 250 {
		t.Errorf("cache tokens = %d/%d, want 1000/250 (the old placeholder emitted zeros)", r.CacheReadTokens, r.CacheWriteTokens)
	}
}

func TestQANativeMemoryToolNeverHitsRegistry(t *testing.T) {
	tools := newMockTools()
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "m1", Name: "orchicon_memory_note"}, ToolCallDelta{Index: 0, ArgsJSONDelta: `{"text":"fact"}`}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 5, OutputTokens: 1}},
		{events: []Event{TextDelta{Text: "end"}}, finish: StopStop, usage: Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_memtool"),
		Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
		ProjectDir: t.TempDir(),
		Provider:   prov,
		Tools:      tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(s.dir); statErr != nil {
		t.Skip("transcript dir created lazily")
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tools.mu.Lock()
	executed := strings.Join(tools.executed, "\n")
	tools.mu.Unlock()
	if strings.Contains(executed, "orchicon_memory_note") {
		t.Error("session-scoped memory tool must not be routed to the tool registry")
	}
	joined := strings.Join(cb.toolCalls, "\n")
	if !strings.Contains(joined, "orchicon_memory_note") {
		t.Error("OnToolCall must still surface the session-scoped call for the live pane")
	}
}
