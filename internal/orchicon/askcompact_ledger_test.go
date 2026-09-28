package orchicon

// askcompact_ledger_test.go — the identifier ledger, and the loss it exists to
// prevent.
//
// The collapse is lossy in two places. reduceAskHistory replaces tool arguments,
// tool results and images with one-line markers BEFORE the summarizer sees
// anything, and on the over-limit conversation that motivated the pressure gate
// that dropped material was 83% of the bytes (46% images, 37% tool results) against
// 3% of actual text. The identifiers a continuing conversation needs — file paths,
// work item ids, commit shas — live in exactly what gets dropped, so the summary
// prompt's instruction to "preserve identifiers verbatim" was asked of a model that
// had never been shown them.
//
// These tests pin both halves of the answer: the ledger names what the reduction
// dropped, and it survives into the compacted history even when the summary omits
// it entirely.

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// ledgerTestHistory builds a history whose ONLY copies of an entity id, a file path
// and a commit live inside tool arguments and a tool result — the material the
// reduce stage replaces with markers. The prose messages deliberately contain none
// of them, so anything the ledger names can only have come from the dropped side.
func ledgerTestHistory(n int) ([]Message, string, string, string) {
	const (
		entityID = "01M33R5RDSENCVRFVZ588N9NRG"
		path     = "/home/beardedparrott/projects/Orchicon/internal/askorchicon/chat.go"
		commit   = "c0a80e54"
	)
	hist := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		hist = append(hist, Message{Role: role, Content: []Content{{Text: compactTestStr("a message with no identifiers in it")}}})
	}
	// The tool call whose ARGUMENTS carry the path and the entity id.
	hist[1].Content = append(hist[1].Content, Content{ToolUse: &ContentToolUse{
		ToolCallID: "call_1",
		Name:       "bash",
		ArgsJSON:   `{"command":"git log --oneline -1","work_item":"` + entityID + `","path":"` + path + `"}`,
	}})
	// The tool RESULT whose output carries the commit.
	hist[2].Content = append(hist[2].Content, Content{ToolResult: &ContentToolResult{
		ToolCallID: "call_1",
		Content:    "commit " + commit + " fix(ask): never deref an unbound workflow_id on update\n",
	}})
	return hist, entityID, path, commit
}

// TestTheLedgerNamesWhatTheReductionDrops is the premise test. If reduceAskHistory
// already kept these tokens, the ledger would be redundant — so this asserts the
// gap exists before asserting the ledger closes it.
func TestTheLedgerNamesWhatTheReductionDrops(t *testing.T) {
	hist, entityID, path, commit := ledgerTestHistory(8)

	reduced, _, _ := reduceAskHistory(hist)
	for _, tok := range []string{entityID, path, commit} {
		if strings.Contains(reduced, tok) {
			t.Fatalf("the reduced transcript already contains %q — this test's premise (that the "+
				"reduction drops identifiers) no longer holds, so the ledger's value must be re-derived", tok)
		}
	}

	ledger := askHistoryLedger(hist)
	for _, tok := range []string{entityID, path, commit} {
		if !strings.Contains(ledger, tok) {
			t.Fatalf("the ledger does not name %q, which the reduction dropped — that identifier is "+
				"unrecoverable after the collapse.\nledger:\n%s", tok, ledger)
		}
	}
	if !strings.Contains(ledger, "bash (1)") {
		t.Errorf("the ledger should name the tools invoked with their counts, got:\n%s", ledger)
	}
}

// TestACompactedHistoryCarriesTheLedgerEvenWhenTheSummaryOmitsIt is the regression
// that matters. The stub model returns a summary that mentions NOTHING from the
// transcript, which is the worst case: a lazy or lossy summary. The identifiers must
// still be in the history afterwards, because the ledger rides verbatim beside it.
func TestACompactedHistoryCarriesTheLedgerEvenWhenTheSummaryOmitsIt(t *testing.T) {
	hist, entityID, path, commit := ledgerTestHistory(20)

	// The summary deliberately contains no identifier at all.
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "We discussed some work and made progress."}}}
	b := newCompactBridge(t, prov)

	const convID = "01LEDGERCOMPACTTEST0000000"
	ctx := tenant.WithID(context.Background(), "tnt")
	sid, err := b.CreateConversationSession(ctx, convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = hist
	b.mu.Unlock()

	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: convID,
		SessionID:      sid,
		ModelRef:       "orchicon/deepseek/deepseek-flash",
		Reason:         "pressure",
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected a compaction, detail=%q", res.Detail)
	}
	if strings.Contains(res.Summary, entityID) {
		t.Fatal("the test's stub summary was supposed to omit every identifier")
	}

	b.mu.Lock()
	after := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if len(after) == 0 || after[0].Content[0].Text == nil {
		t.Fatal("no compacted history to inspect")
	}
	marker := *after[0].Content[0].Text
	for _, tok := range []string{entityID, path, commit} {
		if !strings.Contains(marker, tok) {
			t.Fatalf("identifier %q was lost by the collapse even though the summary did not mention it "+
				"and the ledger should have carried it verbatim.\nmarker:\n%s", tok, marker)
		}
	}
	// And the summarize turn was TOLD about them, so it could attribute rather than
	// merely fail to mention them.
	if req := prov.lastRequest(); len(req.Messages) == 0 ||
		!strings.Contains(mustText(req.Messages[len(req.Messages)-1]), entityID) {
		t.Error("the summarize request did not carry the ledger — the model cannot attribute an identifier it was never shown")
	}
}

func mustText(m Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Text != nil {
			b.WriteString(*c.Text)
		}
	}
	return b.String()
}

// TestLedgerGitObjectSeparatesCommitsFromCounts pins the filter that keeps the ledger
// from filling with numbers. A naive hex match turns every 7+ digit count in tool
// output into a "commit", which buries the real ones.
func TestLedgerGitObjectSeparatesCommitsFromCounts(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		{"c0a80e54", true},                          // short sha
		{"596b30e35bbf67b2075701ae127be88db", true}, // full sha
		{"1000000", false},                          // a token count, not a commit
		{"1234567", false},                          // digits only
		{"deadbeef", false},                         // hex, but a word — no digit
		{"abcdefg", false},                          // not hex
		{"abc123", false},                           // too short
	}
	for _, tc := range cases {
		if got := ledgerGitObject(tc.tok); got != tc.want {
			t.Errorf("ledgerGitObject(%q) = %v, want %v", tc.tok, got, tc.want)
		}
	}
}

// TestTheLedgerIsCappedAndReportsWhatItLeftOut pins the bound. The ledger must not
// inherit the unbounded growth the collapse exists to fix, and a capped list must
// say how much it left out rather than looking complete.
func TestTheLedgerIsCappedAndReportsWhatItLeftOut(t *testing.T) {
	// More distinct entity ids than the cap, spread across tool results. The prefix is
	// 24 Crockford characters and two decimal digits make the 26 an id must be.
	hist := make([]Message, 0, 1)
	var sb strings.Builder
	total := askLedgerMaxPerKind + 5
	for i := 0; i < total; i++ {
		sb.WriteString("01M33R5RDSENCVRFVZ588N9N" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + " ")
	}
	hist = append(hist, Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
		ToolCallID: "c", Content: sb.String(),
	}}}})

	ledger := askHistoryLedger(hist)
	if !strings.Contains(ledger, "(+5 more, not listed)") {
		t.Fatalf("a capped ledger must report what it left out, got:\n%s", ledger)
	}
	if n := strings.Count(ledger, "01M33R"); n != askLedgerMaxPerKind {
		t.Fatalf("ledger listed %d ids, want the cap of %d", n, askLedgerMaxPerKind)
	}
}

// TestAnEmptyLedgerAddsNothingToTheMarker pins that a history with no identifiers
// produces no ledger section at all — the marker must not grow a heading for an
// empty list.
func TestAnEmptyLedgerAddsNothingToTheMarker(t *testing.T) {
	hist := []Message{{Role: RoleUser, Content: []Content{{Text: compactTestStr("nothing identifiable here")}}}}
	if ledger := askHistoryLedger(hist); ledger != "" {
		t.Fatalf("expected no ledger, got:\n%s", ledger)
	}
	marker := compactedHistoryMarker("a summary", "")
	if strings.Contains(*marker, askCompactLedgerMarkerFrame) {
		t.Fatal("an empty ledger must not add its frame to the marker")
	}
}
