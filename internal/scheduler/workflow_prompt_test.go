package scheduler

import (
	"strings"
	"testing"
)

// TestWriteCappedText_Under exercises the small-input path: the body
// fits under the cap and is emitted verbatim, with a single trailing
// newline. Worker prompts rely on this guarantee for short
// descriptions — if the trailing newline disappears, downstream
// sections can smush together.
func TestWriteCappedText_Under(t *testing.T) {
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", "hello world", 1024)
	got := sb.String()
	if !strings.Contains(got, "Output:\nhello world") {
		t.Errorf("expected verbatim body, got: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got: %q", got)
	}
}

// TestWriteCappedText_Over exercises the truncation path. The body
// exceeds the cap and must be cut at a sensible boundary (last
// newline before cap, never mid-word for the chosen cut strategy)
// with a clear marker pointing the worker to the canonical summary.
func TestWriteCappedText_Over(t *testing.T) {
	body := strings.Repeat("lorem ipsum dolor sit amet\n", 500) // ~13K
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", body, 1024)
	got := sb.String()
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker, got: %q", got)
	}
	if !strings.Contains(got, "ORCHICON WORKER SUMMARY") {
		t.Errorf("truncation marker should reference the summary, got: %q", got)
	}
	// The body must end on a complete line, never mid-line, even
	// after the cut. The last data line in the output is the one
	// immediately before the marker (\n…[truncated — ...]).
	cut := strings.Index(got, "…[truncated")
	if cut < 0 {
		t.Fatal("missing truncation marker")
	}
	// Everything before the marker is body + the trailing newline
	// of the cut line. The character just before the marker is
	// therefore a newline (the cut landed on a line boundary).
	pre := got[:cut]
	if !strings.HasSuffix(pre, "\n") {
		t.Errorf("expected cut to land on a newline, got tail: %q", pre[len(pre)-40:])
	}
}

// TestWriteCappedText_NewlineBoundary verifies the cut strategy
// prefers the last newline over a hard byte-cut so we never slice
// a word in half. The input is a single line of 5000 chars (no
// newlines) and the cap is 1024: the cap must trigger, the body
// emits up to 1024 chars (no newline available), and the marker
// follows.
func TestWriteCappedText_NoNewlines(t *testing.T) {
	body := strings.Repeat("a", 5000)
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", body, 1024)
	got := sb.String()
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker for huge single-line body, got: %q", got[:200])
	}
	// No-newline fallback: the cap is the hard cut. The body chunk
	// should be at most cap bytes (plus a trailing newline before
	// the marker).
	pre := strings.SplitN(got, "\n…", 2)[0]
	if len(pre) > 2000 {
		t.Errorf("body chunk unexpectedly long: %d", len(pre))
	}
}

// TestStepKindLabel covers the human-readable mapping for every kind
// the workflow editor exposes. A regression here would change every
// timeline header in every worker prompt.
func TestStepKindLabel(t *testing.T) {
	cases := map[string]string{
		"task":      "task",
		"decision":  "decision",
		"approval":  "approval",
		"parallel":  "parallel",
		"recover":   "recovery",
		"work_item": "work item",
		"project":   "project",
		"unknown":   "unknown",
	}
	for in, want := range cases {
		if got := stepKindLabel(in); got != want {
			t.Errorf("stepKindLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
