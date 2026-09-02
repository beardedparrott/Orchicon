package orchicon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/agentmemory"
)

// ctxModelProvider is a mockProvider that ALSO reports a true context
// window for the session model (live hint) — used to exercise the window
// trigger. ctxTokens <= 0 means no live hint (window trigger inert).
type ctxModelProvider struct {
	mockProvider
	ctxTokens int64
}

func (p *ctxModelProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if p.ctxTokens <= 0 {
		return nil, nil
	}
	return []ModelInfo{{ID: "deepseek-v4-flash", Context: p.ctxTokens}}, nil
}

func compactTestSession(t *testing.T, budgets string, window int64) (*Session, *ctxModelProvider, *agentmemory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	ms, err := agentmemory.Open(dir)
	if err != nil {
		t.Fatalf("agentmemory.Open: %v", err)
	}
	t.Cleanup(func() { ms.Close() })
	prov := &ctxModelProvider{ctxTokens: window}
	prov.turns = []scriptedTurn{}
	m := testManifest("orchicon/mockprov/deepseek-v4-flash")
	if budgets != "" {
		m.Budgets = []byte(budgets)
	}
	s, err := NewSession(SessionConfig{
		ExecRow:     testExecRow("exec_compact"),
		Manifest:    m,
		ProjectDir:  dir,
		Provider:    prov,
		MemoryStore: ms,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s, prov, ms, dir
}

func seedHistory(s *Session) {
	goal := "Goal: implement guarded compaction."
	s.history = []Message{
		{Role: RoleUser, Content: []Content{{Text: &goal}}},
		assistantToolUseMsg("call_1", "bash", `{"cmd":"ls"}`),
		toolResultMsg("call_1", "big old tool output line one"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("Continue")}}},
		assistantToolUseMsg("call_2", "glob", `{"pattern":"**/*.go"}`),
		toolResultMsg("call_2", "second big output that should also be evicted"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("Keep going")}}},
		assistantTextMsg("Final recent assistant message."),
	}
}

func ptrStr(s string) *string { return &s }

func assistantToolUseMsg(id, name, args string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: id, Name: name, ArgsJSON: args}}}}
}

func toolResultMsg(id, out string) Message {
	return Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: id, Content: out}}}}
}

func assistantTextMsg(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Text: &text}}}
}

// AC (no-hint): with NO live context hint and budget gates disabled, the
// session never compacts — token volume alone never fires compaction.
func TestCompactionNoHintNoFireOnTokensAlone(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"compact_tiers":[true,true,true]}`, 0)
	seedHistory(s)
	for step := 2; step < 12; step++ {
		s.recordTurnUsage(Usage{InputTokens: 100_000, OutputTokens: 10_000})
		if fired := s.maybeCompact(context.Background(), step, Usage{InputTokens: 100_000, OutputTokens: 10_000}); fired {
			t.Fatalf("compaction fired at step %d with no window hint and no enabled gate", step)
		}
	}
	if s.cs.compactions != 0 {
		t.Errorf("compactions = %d, want 0", s.cs.compactions)
	}
}

// AC: budget-breach (escalate tier) fires compaction exactly once per tier
// and the session continues afterwards.
func TestCompactionBudgetGateFiresOnce(t *testing.T) {
	s, _, _, dir := compactTestSession(t, `{"tokens":200,"compact_tiers":[false,true,true],"context_compaction":{"recent_turns":2}}`, 0)
	seedHistory(s)
	ctx := context.Background()

	// Turn at 60/200 = 30% (warn) → no compaction.
	s.recordTurnUsage(Usage{InputTokens: 60})
	if s.maybeCompact(ctx, 3, Usage{InputTokens: 60}) {
		t.Fatalf("compaction fired at warn tier")
	}
	// 60 more → 120/200 = 60% (escalate, compact_tiers[1]=true) → fires.
	s.recordTurnUsage(Usage{InputTokens: 60})
	if !s.maybeCompact(ctx, 4, Usage{InputTokens: 60}) {
		t.Fatalf("compaction did not fire at escalate tier")
	}
	// Same-tier re-evaluation (session continues) → no re-fire.
	s.recordTurnUsage(Usage{})
	if s.maybeCompact(ctx, 5, Usage{}) {
		t.Fatalf("compaction re-fired on the same escalate tier")
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1", s.cs.compactions)
	}
	// Evicted originals offloaded to the project dir.
	path := filepath.Join(dir, ".orchicon", "offload", s.id+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Errorf("offload file %s missing or empty (err=%v)", path, err)
	}
	// Session continues: history has a digest marker + recent tail.
	h := s.History()
	if len(h) == 0 {
		t.Fatalf("history empty after compaction — session cannot continue")
	}
	found := false
	for _, m := range h {
		for _, c := range m.Content {
			if c.Text != nil && strings.Contains(*c.Text, "Compacted old tool results") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no compaction digest marker in history")
	}
}

// AC: true-window pressure (live hint) fires at pressure_frac of the real
// window; below it nothing fires.
func TestCompactionWindowPressureFires(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000)
	seedHistory(s)
	ctx := context.Background()
	s.recordTurnUsage(Usage{InputTokens: 750})
	if s.maybeCompact(ctx, 3, Usage{InputTokens: 750}) {
		t.Fatalf("compaction fired below pressure_frac")
	}
	s.recordTurnUsage(Usage{InputTokens: 150})
	if !s.maybeCompact(ctx, 4, Usage{InputTokens: 150}) {
		t.Fatalf("compaction did not fire at window pressure")
	}
}

// AC (prefix stability): two sessions with DIFFERENT memory contents
// produce byte-identical block-0; the memory digest appears only in block 1
// (after the cache breakpoint).
func TestPrefixStabilityAcrossMemoryContents(t *testing.T) {
	s1, _, ms1, _ := compactTestSession(t, "", 0)
	s2, _, ms2, _ := compactTestSession(t, "", 0)
	writeMem(t, s1, ms1, "Alpha fact about caching")
	writeMem(t, s2, ms2, "Beta unrelated decision record")

	b1 := s1.AssembleSystem()
	b2 := s2.AssembleSystem()
	if len(b1) == 0 || len(b2) == 0 {
		t.Fatalf("empty system blocks")
	}
	if b1[0].Text != b2[0].Text {
		t.Errorf("block-0 (static prefix) differs across memory contents:\n%q\nvs\n%q", b1[0].Text, b2[0].Text)
	}
	if !b1[0].Cache {
		t.Errorf("block 0 must carry the cache breakpoint")
	}
	zone1, zone2 := "", ""
	for _, blk := range b1[1:] {
		zone1 += blk.Text
	}
	for _, blk := range b2[1:] {
		zone2 += blk.Text
	}
	if !strings.Contains(zone1, "Alpha fact about caching") {
		t.Errorf("session 1 digest missing from mutable zone")
	}
	if !strings.Contains(zone2, "Beta unrelated decision record") {
		t.Errorf("session 2 digest missing from mutable zone")
	}
}

func writeMem(t *testing.T, s *Session, ms *agentmemory.Store, title string) {
	t.Helper()
	_, err := ms.Write(context.Background(), agentmemory.WriteInput{
		TenantID:    s.identity.TenantID,
		ProjectDir:  s.projectDir,
		ExecutionID: s.identity.ExecutionID,
		WorkerID:    s.identity.WorkerID,
		Title:       title,
		Body:        "durable body content",
		Tags:        []string{"tag1"},
	})
	if err != nil {
		t.Fatalf("writeMem: %v", err)
	}
}
