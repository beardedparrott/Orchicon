package opencode

import "testing"

// Tag fragments are derived from the runtime constants (sliced), never
// typed verbatim — raw markup gets rewritten in transit through some
// tooling paths.
var (
	dOpen  = completedThinkOpen
	dClose = completedThinkClose
)

func TestCompletedThinkDemuxSingleBlock(t *testing.T) {
	d := newCompletedThinkDemux()
	clean, bodies := d.segment("answer before" + dOpen + "thinking hard" + dClose + "after")
	if clean != "answer beforeafter" {
		t.Fatalf("clean = %q, want %q", clean, "answer beforeafter")
	}
	if len(bodies) != 1 || bodies[0] != "thinking hard" {
		t.Fatalf("bodies = %q, want [thinking hard]", bodies)
	}
}

func TestCompletedThinkDemuxPassthroughNoTags(t *testing.T) {
	d := newCompletedThinkDemux()
	clean, bodies := d.segment("just plain text")
	if clean != "just plain text" || len(bodies) != 0 {
		t.Fatalf("clean = %q bodies = %q", clean, bodies)
	}
}

func TestCompletedThinkDemuxBarePairYieldsNothing(t *testing.T) {
	d := newCompletedThinkDemux()
	clean, bodies := d.segment(dOpen + dClose)
	if clean != "" || len(bodies) != 0 {
		t.Fatalf("tag delimiter leaked: clean = %q bodies = %q", clean, bodies)
	}
}

func TestCompletedThinkDemuxMultipleBlocks(t *testing.T) {
	d := newCompletedThinkDemux()
	clean, bodies := d.segment("before " + dOpen + "a" + dClose + " mid " + dOpen + "b" + dClose + " done")
	if clean != "before  mid  done" {
		t.Fatalf("clean = %q", clean)
	}
	if len(bodies) != 2 || bodies[0] != "a" || bodies[1] != "b" {
		t.Fatalf("bodies = %q, want [a b]", bodies)
	}
}

func TestCompletedThinkDemuxBlockSpansParts(t *testing.T) {
	// A block opened in one completed part and closed in a later part
	// still routes: the opening part contributes its tail as a body, the
	// closing part contributes its head, and clean text stays clean.
	d := newCompletedThinkDemux()
	clean1, bodies1 := d.segment("answer " + dOpen + "thinking part one")
	if clean1 != "answer " {
		t.Fatalf("clean1 = %q", clean1)
	}
	if len(bodies1) != 1 || bodies1[0] != "thinking part one" {
		t.Fatalf("bodies1 = %q", bodies1)
	}
	clean2, bodies2 := d.segment("thinking part two" + dClose + " tail")
	if clean2 != " tail" {
		t.Fatalf("clean2 = %q", clean2)
	}
	if len(bodies2) != 1 || bodies2[0] != "thinking part two" {
		t.Fatalf("bodies2 = %q", bodies2)
	}
}

func TestCompletedThinkDemuxUnterminatedStaysOutOfText(t *testing.T) {
	// A block never closed (provider truncation) contributes its runs as
	// reasoning and never leaks into clean text — no end-of-run flush
	// needed because bodies emit eagerly per part.
	d := newCompletedThinkDemux()
	clean, bodies := d.segment("before " + dOpen + "never closed")
	if clean != "before " {
		t.Fatalf("clean = %q", clean)
	}
	if len(bodies) != 1 || bodies[0] != "never closed" {
		t.Fatalf("bodies = %q", bodies)
	}
	// A following part is still in-think until a close arrives.
	clean2, bodies2 := d.segment("still thinking" + dClose + " done")
	if clean2 != " done" {
		t.Fatalf("clean2 = %q", clean2)
	}
	if len(bodies2) != 1 || bodies2[0] != "still thinking" {
		t.Fatalf("bodies2 = %q", bodies2)
	}
}

func TestCompletedThinkDemuxNoDedupeAcrossCalls(t *testing.T) {
	// Each block is its own body even with identical content — the caller
	// persists native reasoning parts and demuxed bodies side by side.
	d := newCompletedThinkDemux()
	_, b1 := d.segment(dOpen + "same" + dClose)
	_, b2 := d.segment(dOpen + "same" + dClose)
	if len(b1) != 1 || len(b2) != 1 || b1[0] != "same" || b2[0] != "same" {
		t.Fatalf("b1 = %q b2 = %q", b1, b2)
	}
}
