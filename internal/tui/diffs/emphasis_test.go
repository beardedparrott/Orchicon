package diffs

import "testing"

// EmphasizeTokens must produce the same span ranges as sideBySide.ts
// emphasizeTokens for the cases the TS test asserts.
func TestEmphasizeTokens(t *testing.T) {
	const prefix = "The quick "
	const suffix = " fox"

	// Replace pair: "brown" → "red" in the middle.
	oldSpans, newSpans := EmphasizeTokens(prefix+"brown"+suffix, prefix+"red"+suffix)
	if len(oldSpans) != 1 {
		t.Fatalf("old spans = %d, want 1", len(oldSpans))
	}
	if oldSpans[0].Type != "del" {
		t.Errorf("old span type = %q, want del", oldSpans[0].Type)
	}
	if newSpans[0].Type != "add" {
		t.Errorf("new span type = %q, want add", newSpans[0].Type)
	}
	// The unchanged "The quick " prefix and " fox" suffix are NOT spanned.
	if oldSpans[0].Start != len(prefix) || oldSpans[0].End != len(prefix)+len("brown") {
		t.Errorf("old span = [%d,%d), want [%d,%d)", oldSpans[0].Start, oldSpans[0].End, len(prefix), len(prefix)+len("brown"))
	}
	if newSpans[0].Start != len(prefix) || newSpans[0].End != len(prefix)+len("red") {
		t.Errorf("new span = [%d,%d), want [%d,%d)", newSpans[0].Start, newSpans[0].End, len(prefix), len(prefix)+len("red"))
	}

	// A pure insertion marks only the new side.
	oldSpans, newSpans = EmphasizeTokens("line2", "line2x")
	if len(oldSpans) != 0 {
		t.Errorf("pure insertion: old spans = %d, want 0", len(oldSpans))
	}
	if len(newSpans) != 1 {
		t.Errorf("pure insertion: new spans = %d, want 1", len(newSpans))
	}

	// Identical lines → no spans on either side.
	oldSpans, newSpans = EmphasizeTokens("same", "same")
	if len(oldSpans) != 0 || len(newSpans) != 0 {
		t.Errorf("identical: got %d old / %d new spans, want 0/0", len(oldSpans), len(newSpans))
	}

	// Fully-different fallback beyond MAX_EMPHASIS → one span each side.
	oldSpans, newSpans = EmphasizeTokens(strRepeat("a", 300), strRepeat("b", 300))
	if len(oldSpans) == 0 {
		t.Errorf("fallback: expected >0 old spans")
	}
	if len(newSpans) == 0 {
		t.Errorf("fallback: expected >0 new spans")
	}
}

func strRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
