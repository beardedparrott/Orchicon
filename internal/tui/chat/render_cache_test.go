package chat

// render_cache_test.go — THE CACHE MUST NEVER DRAW AN ITEM THAT IS NOT THERE.
//
// A remembered render is only safe if it is invalidated by EVERYTHING the render reads. Two
// things can go wrong, and they need different tests:
//
//  1. renderSig misses a field → an item's rendered form is remembered from a state it is no
//     longer in, and the transcript shows something stale. That is invisible in the rendered
//     output (a cache that always missed would draw the same text), so it is pinned DIRECTLY:
//     TestRenderSigCoversEveryFieldTheRenderReads mutates each field and requires the
//     fingerprint to move.
//  2. the key or the invalidation is wrong in a way the fingerprint cannot catch → the two
//     renders diverge. That is pinned by comparison: TestCachedRenderMatchesUncachedRender
//     drives every mutation through a WARM cache and requires the result to equal what the
//     cacheless renderer produces for the same items.
//
// The conversation is deliberately one of every kind, because the invalidation has to hold for
// the consent card's live state (the operator's highlight and their half-typed free text) and
// for the ask card's, and those are the items that change while the operator is looking at them.

import (
	"reflect"
	"testing"
)

// richConversation is one item of every kind the renderer draws, including both cards.
func richConversation() []ChatItem {
	return []ChatItem{
		{Kind: KindUser, Key: "u-1", Text: "Can you look at `foo.go`?", Source: "chat",
			Attachments: []string{"[image]"}, At: 1, Phase: "p1"},
		{Kind: KindSession, Key: "s-1", SessionID: "ses_1", ServeURL: "https://serve.example.com",
			AdapterKind: "opencode", At: 2, Phase: "p1"},
		{Kind: KindReasoning, Key: "r-1", Text: "first I check the binding\nand then the caller", At: 3, Phase: "p1"},
		{Kind: KindReasoning, Key: "r-2", Text: "a second block, never folded", At: 4, Phase: "p2"},
		{Kind: KindText, Key: "m-1", Text: "Here:\n\n```go\nf()\n```\n\n- one\n- two", At: 5, Phase: "p1"},
		{Kind: KindTool, Key: "t-1", Phase: "p1",
			Tool: &ParsedTool{ID: "c1", ToolName: "bash", Input: "ls", Output: "x.go", At: 6}},
		{Kind: KindError, Key: "e-1", Text: "it broke", At: 7, Phase: "p2"},
		{Kind: KindArtifact, Key: "a-1", Name: "report", Type: "md", Content: "line one\nline two", At: 8},
		{Kind: KindAsk, Key: "ask-1", AskID: "ask-1", At: 9, Phase: "p1",
			Ask: &ParsedAsk{Question: "which branch?", AllowOther: true,
				Options: []AskOption{{Label: "develop", Description: "the integration branch"}, {Label: "main"}}}},
		ConsentItem(PermissionAsk{
			ID: "c-1", Kind: AskQuestion, Question: "Which file?",
			Options: []string{"main.go", "util.go"}, AllowOther: true,
		}, 10),
	}
}

// renderFold folds the first reasoning block and nothing else, so the fold state varies within
// one conversation — which is what the cache keys the segment on.
func renderFold(key string) bool { return key == "r-1" }

// renderBoth renders the items twice: once through a cache that has ALREADY seen these items,
// and once through the cacheless renderer, which is the reference.
func renderBoth(t *testing.T, cache *RenderCache, items []ChatItem, width int, glyph string, why string) {
	t.Helper()
	wantBody, wantSpans := RenderItemsSpansWithCopy(items, width, glyph, renderFold)
	gotBody, gotSpans := cache.Render("conv-1", items, width, glyph, renderFold)
	if gotBody != wantBody {
		t.Fatalf("%s: the cached render differs from the uncached one.\n--- cached ---\n%s\n--- reference ---\n%s",
			why, gotBody, wantBody)
	}
	if !reflect.DeepEqual(gotSpans, wantSpans) {
		t.Fatalf("%s: the cached render's click geometry differs from the uncached one.\ncached:    %+v\nreference: %+v",
			why, gotSpans, wantSpans)
	}
}

// mutation is one field change, named so a failure says what was missed.
type mutation struct {
	name string
	do   func(items []ChatItem)
}

// itemMutations changes every field the render (or the spans' text) reads. ONE OF THESE PER
// SIGNATURE FIELD: an entry missing from renderSig fails its case in both tests below.
func itemMutations() []mutation {
	ms := []mutation{
		{"user text", func(i []ChatItem) { i[0].Text += "!" }},
		{"user source", func(i []ChatItem) { i[0].Source = "goal" }},
		{"user attachment", func(i []ChatItem) { i[0].Attachments = append(i[0].Attachments, "[file: a.md]") }},
		{"user live", func(i []ChatItem) { i[0].Live = true }},
		{"user timestamp", func(i []ChatItem) { i[0].At++ }},
		{"user phase", func(i []ChatItem) { i[0].Phase = "p9" }},
		{"item kind", func(i []ChatItem) { i[4].Kind = KindError }},
		{"session id", func(i []ChatItem) { i[1].SessionID = "ses_2" }},
		{"session serve url", func(i []ChatItem) { i[1].ServeURL = "https://other.example.com" }},
		{"session adapter", func(i []ChatItem) { i[1].AdapterKind = "orchicon" }},
		{"reasoning text", func(i []ChatItem) { i[2].Text += " more" }},
		{"reasoning live", func(i []ChatItem) { i[2].Live = true }},
		{"message text", func(i []ChatItem) { i[4].Text += "\n- three" }},
		{"tool name", func(i []ChatItem) { i[5].Tool.ToolName = "write" }},
		{"tool input", func(i []ChatItem) { i[5].Tool.Input = "ls -la" }},
		{"tool output", func(i []ChatItem) { i[5].Tool.Output = "y.go" }},
		{"tool id", func(i []ChatItem) { i[5].Tool.ID = "c2" }},
		{"tool timestamp", func(i []ChatItem) { i[5].Tool.At++ }},
		{"error text", func(i []ChatItem) { i[6].Text = "it broke differently" }},
		{"artifact name", func(i []ChatItem) { i[7].Name = "report-2" }},
		{"artifact type", func(i []ChatItem) { i[7].Type = "txt" }},
		{"artifact content", func(i []ChatItem) { i[7].Content = "another first line" }},
		{"item key", func(i []ChatItem) { i[8].Key = "ask-2" }},
		{"ask id", func(i []ChatItem) { i[8].AskID = "ask-9" }},
		{"ask question", func(i []ChatItem) { i[8].Ask.Question = "which release?" }},
		{"ask allow other", func(i []ChatItem) { i[8].Ask.AllowOther = false }},
		{"ask answered", func(i []ChatItem) { i[8].Ask.Answered = true }},
		{"ask answer text", func(i []ChatItem) { i[8].Ask.AnswerText = "develop" }},
		{"ask option label", func(i []ChatItem) { i[8].Ask.Options[0].Label = "main" }},
		{"ask option description", func(i []ChatItem) { i[8].Ask.Options[0].Description = "other" }},
		// The free-text row this card gained: an open input row, and the text in it. Both are drawn,
		// so a signature that ignored either would serve the operator a card with their own typing
		// missing from it.
		{"ask drafting", func(i []ChatItem) { i[8].Ask.Drafting = true }},
		{"ask draft text", func(i []ChatItem) { i[8].Ask.Draft = "seed.sql" }},
		{"consent ask id", func(i []ChatItem) { i[9].Consent.Ask.ID = "c-2" }},
		{"consent conversation", func(i []ChatItem) { i[9].Consent.Ask.ConvID = "conv-2" }},
		{"consent tool", func(i []ChatItem) { i[9].Consent.Ask.Tool = "write" }},
		{"consent target", func(i []ChatItem) { i[9].Consent.Ask.Target = "/p/x.go" }},
		{"consent directory", func(i []ChatItem) { i[9].Consent.Ask.Directory = "/p" }},
		{"consent ask kind", func(i []ChatItem) { i[9].Consent.Ask.Kind = AskTool }},
		{"consent question", func(i []ChatItem) { i[9].Consent.Ask.Question = "Which directory?" }},
		{"consent summary", func(i []ChatItem) { i[9].Consent.Ask.Summary = "write two files" }},
		{"consent denied by", func(i []ChatItem) { i[9].Consent.Ask.DeniedBy = "/p/**" }},
		{"consent allow other", func(i []ChatItem) { i[9].Consent.Ask.AllowOther = false }},
		{"consent option", func(i []ChatItem) { i[9].Consent.Ask.Options = []string{"main.go"} }},
		{"consent selection", func(i []ChatItem) { i[9].Consent.Sel = 1 }},
		{"consent decision", func(i []ChatItem) { i[9].Consent.Decision = DecisionAnswer }},
		{"consent choice", func(i []ChatItem) { i[9].Consent.Choice = "util.go" }},
		{"consent note", func(i []ChatItem) { i[9].Consent.Note = "session · /p" }},
		{"consent free-text mode", func(i []ChatItem) { i[9].Consent.OtherMode = true }},
		{"consent free text", func(i []ChatItem) { i[9].Consent.OtherInput = "seed.sql" }},
	}
	return ms
}

// TestRenderSigCoversEveryFieldTheRenderReads is the direct one: every mutation must move the
// fingerprint, whether or not the field happens to affect the drawn text.
func TestRenderSigCoversEveryFieldTheRenderReads(t *testing.T) {
	for _, m := range itemMutations() {
		base := richConversation()
		before := make([]uint64, len(base))
		for i, it := range base {
			before[i] = renderSig(it)
		}
		m.do(base)
		moved := false
		for i, it := range base {
			if renderSig(it) != before[i] {
				moved = true
				break
			}
		}
		if !moved {
			t.Errorf("changing the %s did not move any item's fingerprint — renderSig is missing that field, "+
				"so a cached render would survive a change to it", m.name)
		}
	}
}

// TestCachedRenderMatchesUncachedRender is the comparison: a WARM cache, then a mutation, then
// the two renders must agree.
func TestCachedRenderMatchesUncachedRender(t *testing.T) {
	for _, m := range itemMutations() {
		cache := NewRenderCache()
		items := richConversation()
		// Warm the cache with the pre-mutation items, at the width and glyph the app uses.
		cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
		renderBoth(t, cache, items, 100, CopyGlyph, "warm "+m.name)

		m.do(items)
		renderBoth(t, cache, items, 100, CopyGlyph, "after changing the "+m.name)
	}
}

// AND THE CACHE IS PER (CONVERSATION, WIDTH, GLYPH) — the same items drawn for another surface
// must not be served the other surface's render. The strip and the Ask pane draw one
// conversation at two widths with and without the copy glyph, which is exactly this case.
func TestACachedSegmentIsNotSharedAcrossSurfaces(t *testing.T) {
	cache := NewRenderCache()
	items := richConversation()
	cache.Render("conv-1", items, 100, CopyGlyph, renderFold) // the pane
	renderBoth(t, cache, items, 40, CopyGlyph, "a narrower pane")
	renderBoth(t, cache, items, 100, "", "the strip (no copy glyph)")
	body, _ := cache.Render("conv-2", items, 100, CopyGlyph, renderFold)
	want, _ := RenderItemsSpansWithCopy(items, 100, CopyGlyph, renderFold)
	if body != want {
		t.Fatal("another conversation's items were served this conversation's cached render")
	}
}

// TestAnUnchangedFrameRendersNothing is the CLAIM OF THE CACHE, asserted rather than assumed:
// the second and third repaints of identical items must lay out no items at all, and changing
// exactly one item must lay out exactly one.
func TestAnUnchangedFrameRendersNothing(t *testing.T) {
	cache := NewRenderCache()
	items := richConversation()

	cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
	first := cache.layoutsForTest()
	if first != len(items) {
		t.Fatalf("the first frame laid out %d of %d items", first, len(items))
	}

	cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
	cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
	if got := cache.layoutsForTest(); got != first {
		t.Fatalf("repainting an UNCHANGED conversation laid out %d more items — the cache is not being hit", got-first)
	}

	// One delta into the newest message: one item was re-rendered, not all of them.
	items[len(items)-2].Ask.AnswerText = "develop"
	cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
	if got := cache.layoutsForTest(); got != first+1 {
		t.Fatalf("changing one item laid out %d items, want exactly 1", got-first)
	}
}

// A NIL CACHE IS NOT A SPECIAL CASE FOR THE CALLER: a shell without one (an embedder, a test)
// renders as it always did. This is what lets the two App call sites read the cache
// unconditionally.
func TestANilCacheRendersWithoutRemembering(t *testing.T) {
	var cache *RenderCache
	items := richConversation()
	body, spans := cache.Render("conv-1", items, 100, CopyGlyph, renderFold)
	wantBody, wantSpans := RenderItemsSpansWithCopy(items, 100, CopyGlyph, renderFold)
	if body != wantBody || !reflect.DeepEqual(spans, wantSpans) {
		t.Fatal("a nil cache must render exactly what the cacheless renderer does")
	}
}
