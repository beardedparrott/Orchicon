package fileedit

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// TestComputeUnifiedDiffVectors runs every shared test vector through the
// diff engine and asserts byte-equal diff text. The TS side
// (frontend/src/lib/fileedit/testvectors.test.ts) loads the SAME JSON files,
// so a green pair of runs proves GUI and TUI renderers consume identical
// ground truth.
func TestComputeUnifiedDiffVectors(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	if len(vecs) < 10 {
		t.Fatalf("expected the full vector set, got %d", len(vecs))
	}
	for _, v := range vecs {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			var before, after []byte
			if v.Before != nil {
				before = []byte(*v.Before)
			}
			if v.After != nil {
				after = []byte(*v.After)
			}
			got := ComputeUnifiedDiff(before, after, v.Path)

			if v.ExpectedSkipEntry {
				if got.UnifiedDiff != "" {
					t.Errorf("no-op vector must yield empty diff, got %q", got.UnifiedDiff)
				}
				return
			}
			if got.Kind != v.ExpectedKind {
				t.Errorf("kind = %q, want %q", got.Kind, v.ExpectedKind)
			}
			if got.Binary != v.ExpectedBinary {
				t.Errorf("binary = %v, want %v", got.Binary, v.ExpectedBinary)
			}
			if got.Truncated != v.ExpectedTruncated {
				t.Errorf("truncated = %v, want %v", got.Truncated, v.ExpectedTruncated)
			}
			if v.ExpectedUnifiedDiff != nil && got.UnifiedDiff != *v.ExpectedUnifiedDiff {
				t.Errorf("diff mismatch\n--- got (%d bytes) ---\n%q\n--- want (%d bytes) ---\n%q",
					len(got.UnifiedDiff), got.UnifiedDiff, len(*v.ExpectedUnifiedDiff), *v.ExpectedUnifiedDiff)
			}
			if v.ExpectedMaxDiffBytes > 0 && len(got.UnifiedDiff) > v.ExpectedMaxDiffBytes {
				t.Errorf("diff = %d bytes, exceeds cap %d", len(got.UnifiedDiff), v.ExpectedMaxDiffBytes)
			}
			// Byte-identical repeated edits: same pair → byte-identical diff.
			again := ComputeUnifiedDiff(before, after, v.Path)
			if again.UnifiedDiff != got.UnifiedDiff {
				t.Errorf("recomputation is not deterministic")
			}
		})
	}
}

// TestIdenticalVectorsShareDiffText pins the AC that byte-identical repeated
// edits produce identical diffs: the `modify` and `identical-repeat-edit`
// vectors carry the same before/after pair under different names.
func TestIdenticalVectorsShareDiffText(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	var modify, repeat *testfixtures.FileEditVector
	for i := range vecs {
		switch vecs[i].Name {
		case "modify-two-lines":
			modify = &vecs[i]
		case "identical-repeat-edit":
			repeat = &vecs[i]
		}
	}
	if modify == nil || repeat == nil {
		t.Fatalf("expected both vectors present")
	}
	r1 := ComputeUnifiedDiff([]byte(*modify.Before), []byte(*modify.After), modify.Path)
	r2 := ComputeUnifiedDiff([]byte(*repeat.Before), []byte(*repeat.After), repeat.Path)
	if r1.UnifiedDiff != r2.UnifiedDiff || r1.UnifiedDiff == "" {
		t.Fatalf("identical edits produced different/empty diffs:\n%q\n%q", r1.UnifiedDiff, r2.UnifiedDiff)
	}
}

// TestNoopYieldsEmptyDiff verifies the no-op guard: identical content on both
// sides → empty diff → callers append no ledger entry.
func TestNoopYieldsEmptyDiff(t *testing.T) {
	r := ComputeUnifiedDiff([]byte("same\n"), []byte("same\n"), "f.txt")
	if r.UnifiedDiff != "" || r.Kind != "" {
		t.Fatalf("no-op edit must yield empty diff + empty kind, got %+v", r)
	}
}

// TestLargeFileBounded checks a genuinely large (multi-MB) text file diffs
// fast and the result stays bounded + flagged.
func TestLargeFileBounded(t *testing.T) {
	before := strings.Repeat("stable line\n", 100_000)
	after := before + "one added line\n"
	r := ComputeUnifiedDiff([]byte(before), []byte(after), "big.txt")
	if r.Truncated {
		t.Fatalf("a one-line append to a large file must not truncate")
	}
	if !strings.Contains(r.UnifiedDiff, "+one added line\n") {
		t.Fatalf("append line missing from diff")
	}
	if strings.Count(r.UnifiedDiff, "\n") > 20 {
		t.Fatalf("expected a small hunk, got %d lines", strings.Count(r.UnifiedDiff, "\n"))
	}
}

// TestUnicodeSafety: CJK/emoji lines must diff line-level (never split a
// multi-byte rune mid-line).
func TestUnicodeSafety(t *testing.T) {
	r := ComputeUnifiedDiff([]byte("こんにちは\n"), []byte("こんばんは\n"), "u.md")
	want := "--- a/u.md\n+++ b/u.md\n@@ -1 +1 @@\n-こんにちは\n+こんばんは\n"
	if r.UnifiedDiff != want {
		t.Fatalf("unicode diff mismatch:\n got %q\nwant %q", r.UnifiedDiff, want)
	}
}

// TestTrailingNewlineToggle pins the AC that a change which ONLY toggles a
// trailing newline is a real, diff-visible edit (size/sha differ; git emits a
// hunk with the "\ No newline at end of file" marker). Comparing the dline
// text alone would digest both sides as a common keep and emit a header-only
// (empty) diff — this test guards the dlineEqual (text AND hasEOL) fix.
func TestTrailingNewlineToggle(t *testing.T) {
	// remove the trailing newline.
	rm := ComputeUnifiedDiff([]byte("foo\n"), []byte("foo"), "f.txt")
	wantRm := "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-foo\n+foo\n\\ No newline at end of file\n"
	if rm.UnifiedDiff != wantRm {
		t.Errorf("remove-NL diff:\n got %q\nwant %q", rm.UnifiedDiff, wantRm)
	}
	// add the trailing newline.
	add := ComputeUnifiedDiff([]byte("foo"), []byte("foo\n"), "f.txt")
	wantAdd := "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-foo\n\\ No newline at end of file\n+foo\n"
	if add.UnifiedDiff != wantAdd {
		t.Errorf("add-NL diff:\n got %q\nwant %q", add.UnifiedDiff, wantAdd)
	}
	// The two edits (and their inverses) must be byte-deterministic.
	if r := ComputeUnifiedDiff([]byte("foo\n"), []byte("foo"), "f.txt"); r.UnifiedDiff != wantRm {
		t.Errorf("remove-NL not deterministic")
	}
}

// TestBinaryEitherSide: NUL detection applies to both the before and the
// after side.
func TestBinaryEitherSide(t *testing.T) {
	if r := ComputeUnifiedDiff([]byte("text\x00here"), []byte("plain\n"), "b.bin"); !r.Binary || r.UnifiedDiff != "" {
		t.Fatalf("binary before side not detected: %+v", r)
	}
	if r := ComputeUnifiedDiff([]byte("plain\n"), []byte("text\x00here"), "b.bin"); !r.Binary || r.UnifiedDiff != "" {
		t.Fatalf("binary after side not detected: %+v", r)
	}
}
