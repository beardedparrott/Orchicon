package chat

import "testing"

// Ported from frontend/src/components/executions/sessionItems.test.ts —
// the same cases, so terminal grouping cannot drift from the GUI's.

func text(key, txt string, at int64, live bool, phase string) ChatItem {
	return ChatItem{Kind: KindText, Text: txt, At: at, Key: key, Live: live, Phase: phase}
}
func reasoning(key, txt string, at int64, live bool, phase string) ChatItem {
	return ChatItem{Kind: KindReasoning, Text: txt, At: at, Key: key, Live: live, Phase: phase}
}
func tool(key, toolName string, at int64) ChatItem {
	return ChatItem{Kind: KindTool, Tool: &ParsedTool{ID: key, ToolName: toolName, Input: "", Output: "", At: at}, Key: key}
}

func texts(items []ChatItem) []string {
	var out []string
	for _, i := range items {
		if i.Kind == KindText {
			out = append(out, i.Text)
		}
	}
	return out
}

func TestMergeFullHistoryWhenLiveEmpty(t *testing.T) {
	history := []ChatItem{text("t1", "hello", 100, false, ""), tool("t2", "bash", 200), text("t3", "world", 300, false, "")}
	merged := MergeSessionItems(history, nil)
	if len(merged) != 3 {
		t.Fatalf("len = %d, want 3", len(merged))
	}
	if merged[0].Key != "t1" || merged[2].Key != "t3" {
		t.Fatalf("keys = %s..%s, want t1..t3", merged[0].Key, merged[2].Key)
	}
}

func TestMergeDropsCoveredLiveChunks(t *testing.T) {
	history := []ChatItem{text("t1", "already said", 500, false, "")}
	live := []ChatItem{
		text("l1", "old chunk ", 400, true, ""), // covered → dropped
		text("l2", "new chunk", 600, true, ""),  // newer → kept
	}
	merged := MergeSessionItems(history, live)
	if len(merged) != 2 {
		t.Fatalf("len = %d, want 2", len(merged))
	}
	if got := texts(merged); len(got) != 2 || got[1] != "new chunk" {
		t.Fatalf("texts = %v", got)
	}
}

func TestMergeGroupsConsecutiveLiveText(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		text("l1", "The quick ", 100, true, ""),
		text("l2", "brown fox ", 101, true, ""),
		text("l3", "jumps", 102, true, ""),
	})
	if len(merged) != 1 || merged[0].Kind != KindText {
		t.Fatalf("len=%d kind=%s", len(merged), merged[0].Kind)
	}
	if merged[0].Text != "The quick brown fox jumps" {
		t.Fatalf("text = %q", merged[0].Text)
	}
}

func TestMergeGroupsConsecutiveLiveReasoning(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "thinking ", 100, true, ""),
		reasoning("r2", "deeper", 101, true, ""),
	})
	if len(merged) != 1 || merged[0].Kind != KindReasoning || merged[0].Text != "thinking deeper" {
		t.Fatalf("got %+v", merged[0])
	}
}

func TestMergeDoesNotMergeAcrossKinds(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "think", 100, true, ""),
		text("l1", "answer", 200, true, ""),
	})
	if len(merged) != 2 || merged[0].Kind != KindReasoning || merged[1].Kind != KindText {
		t.Fatalf("got %d items", len(merged))
	}
}

func TestMergeInterleavesHistoryAndLive(t *testing.T) {
	history := []ChatItem{text("t1", "earlier", 100, false, ""), tool("t2", "bash", 200)}
	live := []ChatItem{text("l1", "streaming", 300, true, "")}
	merged := MergeSessionItems(history, live)
	if len(merged) != 3 {
		t.Fatalf("len = %d", len(merged))
	}
	if merged[0].Kind != KindText || merged[1].Kind != KindTool || merged[2].Kind != KindText {
		t.Fatalf("kinds: %s %s %s", merged[0].Kind, merged[1].Kind, merged[2].Kind)
	}
	if merged[2].Text != "streaming" {
		t.Fatalf("last = %q", merged[2].Text)
	}
}

func TestMergeInterleavedReasoningTextOneBubbleEach(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "think ", 100, true, ""),
		text("l1", "answer ", 101, true, ""),
		reasoning("r2", "harder", 102, true, ""),
	})
	if len(merged) != 2 {
		t.Fatalf("len = %d, want 2", len(merged))
	}
	// First-appearance order: reasoning arrived before text.
	if merged[0].Kind != KindReasoning || merged[0].Text != "think harder" {
		t.Fatalf("merged[0] = %+v", merged[0])
	}
	if merged[1].Kind != KindText || merged[1].Text != "answer " {
		t.Fatalf("merged[1] = %+v", merged[1])
	}
}

func TestMergeSealsReasoningOnToolBoundary(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "think about input ", 100, true, ""),
		tool("t2", "bash", 200),
		reasoning("r3", "think about result", 300, true, ""),
	})
	if len(merged) != 3 {
		t.Fatalf("len = %d, want 3", len(merged))
	}
	if merged[0].Kind != KindReasoning || merged[0].Text != "think about input " {
		t.Fatalf("merged[0] = %+v", merged[0])
	}
	if merged[1].Kind != KindTool {
		t.Fatalf("merged[1] = %s", merged[1].Kind)
	}
	if merged[2].Kind != KindReasoning || merged[2].Text != "think about result" {
		t.Fatalf("merged[2] = %+v", merged[2])
	}
}

func TestGroupingIgnoresLiveFlag(t *testing.T) {
	// Grouping keys off phase/live-fallback, not a live flag on reasoning.
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "think ", 100, false, ""),
		reasoning("r2", "deeper", 101, false, ""),
	})
	if len(merged) != 1 || merged[0].Text != "think deeper" {
		t.Fatalf("got %+v", merged[0])
	}
}

func TestMergeHistoryPhaseGroupsToOneBubble(t *testing.T) {
	merged := MergeSessionItems(
		[]ChatItem{reasoning("r1", "think ", 100, false, "step-1"), reasoning("r2", "deeper", 101, false, "step-1")},
		nil,
	)
	if len(merged) != 1 || merged[0].Kind != KindReasoning || merged[0].Text != "think deeper" {
		t.Fatalf("got %+v", merged[0])
	}
}

func TestMergeKeepsHistoryAndLivePhasesDistinct(t *testing.T) {
	history := []ChatItem{text("t1", "already said", 500, false, "step-1")}
	live := []ChatItem{
		text("l1", "covered ", 400, true, "live-1"), // covered → dropped
		text("l2", "new chunk", 600, true, "live-1"),
	}
	merged := MergeSessionItems(history, live)
	if len(merged) != 2 {
		t.Fatalf("len = %d, want 2", len(merged))
	}
	if got := texts(merged); len(got) != 2 || got[1] != "new chunk" {
		t.Fatalf("texts = %v", got)
	}
}

func TestMergeDoesNotMergeAcrossLivePhases(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "first think ", 100, true, "live-0"),
		tool("t2", "bash", 200),
		reasoning("r3", "second think", 300, true, "live-1"),
	})
	if len(merged) != 3 {
		t.Fatalf("len = %d", len(merged))
	}
	if merged[0].Text != "first think " || merged[2].Text != "second think" {
		t.Fatalf("texts: %q / %q", merged[0].Text, merged[2].Text)
	}
}

func TestMergeStillGroupsTextInterleavedWithReasoning(t *testing.T) {
	merged := MergeSessionItems(nil, []ChatItem{
		text("l1", "The quick ", 100, true, ""),
		reasoning("r1", "hmm ", 101, true, ""),
		text("l2", "brown fox", 102, true, ""),
	})
	if got := texts(merged); len(got) != 1 || got[0] != "The quick brown fox" {
		t.Fatalf("texts = %v", got)
	}
	for _, i := range merged {
		if i.Kind == KindReasoning && i.Text != "hmm " {
			t.Fatalf("reasoning = %q", i.Text)
		}
	}
}

func TestGroupPhaseGroupsFirstAppearanceOrder(t *testing.T) {
	groups := GroupPhaseGroups([]ChatItem{
		{Kind: KindReasoning, Text: "a", Phase: "live-1"},
		{Kind: KindText, Text: "b", Phase: "live-1"},
		{Kind: KindText, Text: "c", Phase: "live-1"},
		{Kind: KindReasoning, Text: "d", Phase: "live-2"},
	}, nil)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	want := [][]string{{"reasoning"}, {"text", "text"}, {"reasoning"}}
	for i, g := range groups {
		for j, it := range g {
			if string(it.Kind) != want[i][j] {
				t.Fatalf("group %d item %d = %s, want %s", i, j, it.Kind, want[i][j])
			}
		}
	}
}

func TestGroupPhaseGroupsAbsorbsArtifacts(t *testing.T) {
	groups := GroupPhaseGroups([]ChatItem{
		{Kind: KindText, Text: "a", Phase: "live-1"},
		{Kind: KindArtifact, Name: "f", Type: "text", Content: "x", At: 1, Key: "a1", Phase: "live-1"},
		{Kind: KindText, Text: "b", Phase: "live-1"},
	}, map[string]bool{string(KindArtifact): true})
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if len(groups[0]) != 3 {
		t.Fatalf("group len = %d, want 3", len(groups[0]))
	}
}

func TestGroupByPhaseIdempotent(t *testing.T) {
	items := MergeSessionItems(nil, []ChatItem{
		reasoning("r1", "think ", 100, true, "live-0"),
		reasoning("r2", "deeper", 101, true, "live-0"),
	})
	if len(items) != 1 {
		t.Fatalf("len = %d", len(items))
	}
	again := GroupByPhase(items)
	if len(again) != 1 || again[0].Text != "think deeper" {
		t.Fatalf("again = %+v", again)
	}
}
