package orchicon

import (
	"strings"
	"testing"
)

// Tag fragments for tests are derived from the runtime constants (sliced),
// never typed verbatim — raw markup gets rewritten in transit through
// some tooling paths.
var (
	tOpen  = thinkOpenTag
	tClose = thinkCloseTag
)

// collect flattens one splitter run into "T:text"/"R:reasoning" segments,
// merging adjacent same-kind deltas the way the renderer downstream does
// (sessionItems merges consecutive live text/reasoning chunks).
func collect(t *testing.T, chunks []string) string {
	t.Helper()
	sp := newThinkSplitter()
	var q []Event
	for _, c := range chunks {
		sp.feed(c, &q)
	}
	sp.drain(&q)
	var b strings.Builder
	kind := byte(0)
	for _, ev := range q {
		var text, k string
		switch e := ev.(type) {
		case TextDelta:
			text, k = e.Text, "T"
		case ReasoningDelta:
			text, k = e.Text, "R"
		default:
			t.Fatalf("unexpected event %T", ev)
		}
		if kind == 0 || kind != k[0] {
			if kind != 0 {
				b.WriteString("|")
			}
			b.WriteString(k + ":")
			kind = k[0]
		}
		b.WriteString(text)
	}
	return b.String()
}

func TestThinkSplitSingleChunk(t *testing.T) {
	got := collect(t, []string{"answer before" + tOpen + "thinking hard" + tClose + "after"})
	want := "T:answer before|R:thinking hard|T:after"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestThinkSplitAcrossChunks(t *testing.T) {
	// The open tag and the close tag both split across chunk boundaries:
	// "<th|ink>" and "</th|ink>".
	got := collect(t, []string{
		"ans" + tOpen[:4], tOpen[4:],
		"hidden rea", "soning" + tClose[:5], tClose[5:],
		" tail",
	})
	want := "T:ans|R:hidden reasoning|T: tail"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestThinkSplitPassthroughNoTags(t *testing.T) {
	got := collect(t, []string{"just ", "plain ", "text"})
	if got != "T:just plain text" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkSplitPartialTagNeverEmitted(t *testing.T) {
	// A chunk ending in an open-tag prefix that never completes must
	// still surface at drain (nothing is swallowed).
	got := collect(t, []string{"start " + tOpen[:5]})
	if got != "T:start "+tOpen[:5] {
		t.Fatalf("got %q", got)
	}
}

func TestThinkSplitUnclosedThinkDrains(t *testing.T) {
	got := collect(t, []string{"before " + tOpen + "never closed, stream died"})
	if got != "T:before |R:never closed, stream died" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkSplitMultipleBlocks(t *testing.T) {
	got := collect(t, []string{
		"before " + tOpen + "a" + tClose + " mid " + tOpen + "b" + tClose + " done",
	})
	want := "T:before |R:a|T: mid |R:b|T: done"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestThinkSplitDropsTagDelimiters(t *testing.T) {
	// A bare tag pair produces no text and no reasoning at all.
	got := collect(t, []string{tOpen + tClose})
	if got != "" {
		t.Fatalf("tag delimiter leaked: %q", got)
	}
}

func TestThinkSplitRealisticLlamaCppTurn(t *testing.T) {
	// A full llama-server-style turn: reasoning first, then the answer,
	// streamed in several deltas.
	chunks := []string{tOpen, "Let me check the", " repo structure…", tClose, "I found the", " bug in bridge.go."}
	got := collect(t, chunks)
	want := "R:Let me check the repo structure…|T:I found the bug in bridge.go."
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
