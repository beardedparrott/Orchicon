package diffs

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// vecByName indexes the shared fixtures by their vector name.
func vecByName(t *testing.T) map[string]testfixtures.FileEditVector {
	t.Helper()
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	m := map[string]testfixtures.FileEditVector{}
	for _, v := range vecs {
		m[v.Name] = v
	}
	return m
}

// ParseUnifiedDiff must faithfully reconstruct what the shared engine
// produced for every vector with a non-empty expected_unified_diff — the
// cross-language "byte-for-byte" contract (same rows, same classifications
// as the TS sideBySide.test.ts).
func TestParseUnifiedDiffRoundTripsFixtures(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	if len(vecs) < 10 {
		t.Fatalf("expected the full vector set (>=10), got %d", len(vecs))
	}
	for _, v := range vecs {
		v := v
		diff := ""
		if v.ExpectedUnifiedDiff != nil {
			diff = *v.ExpectedUnifiedDiff
		}
		if diff == "" {
			continue // no-op/binary/truncate vectors have no diff
		}
		t.Run(v.Name, func(t *testing.T) {
			rows := ParseUnifiedDiff(diff)
			if len(rows) == 0 {
				t.Fatalf("expected >0 rows for %s", v.Name)
			}
			// Reconstruct the diff text from the rows (the round-trip).
			var rebuilt strings.Builder
			for _, r := range rows {
				var line, sign string
				switch r.Kind {
				case KindAdd:
					line, sign = r.NewText, "+"
				case KindDel:
					line, sign = r.OldText, "-"
				default:
					line, sign = r.OldText, " "
				}
				rebuilt.WriteString(sign + line + "\n")
			}
			// Every non-hunk, non-header line of the original must appear.
			originalLines := strings.Split(diff, "\n")
			for _, l := range originalLines {
				if l == "" || strings.HasPrefix(l, "@@") ||
					strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++") ||
					strings.HasPrefix(l, "\\") {
					continue
				}
				if !strings.Contains(rebuilt.String(), l) {
					t.Errorf("vector %s: rebuilt diff lost line %q", v.Name, l)
				}
			}
		})
	}
}

func TestParseUnifiedDiffModifyTwoLines(t *testing.T) {
	vecs := vecByName(t)
	v, ok := vecs["modify-two-lines"]
	if !ok {
		t.Fatal("modify-two-lines fixture not found")
	}
	rows := ParseUnifiedDiff(deref(v.ExpectedUnifiedDiff))
	// @@ -1,3 +1,3 @@ → line 1,2,3 both sides. The changed middle line is -/+.
	ctx0 := rows[0]
	if ctx0.Kind != KindCtx {
		t.Errorf("row0 kind = %q, want ctx", ctx0.Kind)
	}
	if ctx0.LineNoOld != 1 || ctx0.LineNoNew != 1 {
		t.Errorf("ctx line numbers = %d/%d, want 1/1", ctx0.LineNoOld, ctx0.LineNoNew)
	}
	del := rows[1]
	if del.Kind != KindDel {
		t.Errorf("row1 kind = %q, want del", del.Kind)
	}
	if del.LineNoOld != 2 || del.HasNew {
		t.Errorf("del line numbers = %d (HasNew=%v), want old=2 / no new", del.LineNoOld, del.HasNew)
	}
	add := rows[2]
	if add.Kind != KindAdd {
		t.Errorf("row2 kind = %q, want add", add.Kind)
	}
	if add.LineNoNew != 2 || add.HasOld {
		t.Errorf("add line numbers = %d (HasOld=%v), want new=2 / no old", add.LineNoNew, add.HasOld)
	}
}

func TestParseUnifiedDiffCreateAndDelete(t *testing.T) {
	vecs := vecByName(t)
	create := ParseUnifiedDiff(deref(vecs["create-new-file"].ExpectedUnifiedDiff))
	if adds := countKind(create, KindAdd); adds != 3 {
		t.Errorf("create: adds = %d, want 3", adds)
	}
	if dels := countKind(create, KindDel); dels != 0 {
		t.Errorf("create: dels = %d, want 0", dels)
	}
	del := ParseUnifiedDiff(deref(vecs["delete-file"].ExpectedUnifiedDiff))
	if dels := countKind(del, KindDel); dels != 2 {
		t.Errorf("delete: dels = %d, want 2", dels)
	}
	if adds := countKind(del, KindAdd); adds != 0 {
		t.Errorf("delete: adds = %d, want 0", adds)
	}
}

func TestParseUnifiedDiffNoTrailingNewline(t *testing.T) {
	vecs := vecByName(t)
	rows := ParseUnifiedDiff(deref(vecs["no-trailing-newline"].ExpectedUnifiedDiff))
	// ctx(line1) del(line2, no-eol-marker) add(line2, no-eol-marker).
	if ctx := countKind(rows, KindCtx); ctx != 1 {
		t.Errorf("ctx rows = %d, want 1", ctx)
	}
	if dels := countKind(rows, KindDel); dels != 1 {
		t.Errorf("del rows = %d, want 1", dels)
	}
	if adds := countKind(rows, KindAdd); adds != 1 {
		t.Errorf("add rows = %d, want 1", adds)
	}
	// The marker is annotated onto the only del/add rows (⟪no newline⟫ = \u27EA).
	del := findKind(rows, KindDel)
	add := findKind(rows, KindAdd)
	if !strings.Contains(del.OldText, "\u27EA") {
		t.Errorf("del.oldText %q missing no-newline marker", del.OldText)
	}
	if !strings.Contains(add.NewText, "\u27EA") {
		t.Errorf("add.newText %q missing no-newline marker", add.NewText)
	}
	// Paired line numbers: the add/del share new/old line 2, not a phantom
	// ctx row consuming line 3.
	if del.LineNoOld != 2 {
		t.Errorf("del.lineNoOld = %d, want 2", del.LineNoOld)
	}
	if add.LineNoNew != 2 {
		t.Errorf("add.lineNoNew = %d, want 2", add.LineNoNew)
	}
}

// TestParseUnifiedDiffMultiHunk ensures two hunks in one diff both preserve
// line-number state (the multihunk vector has two @@ headers).
func TestParseUnifiedDiffMultiHunk(t *testing.T) {
	vecs := vecByName(t)
	rows := ParseUnifiedDiff(deref(vecs["multi-hunk-three-context"].ExpectedUnifiedDiff))
	if len(rows) == 0 {
		t.Fatal("multihunk produced no rows")
	}
	// Reconstructed diff must contain both changed regions.
	rebuilt := joined(rows)
	for _, want := range []string{"-a1", "+A1", "-a15", "+A15"} {
		if !strings.Contains(rebuilt, want) {
			t.Errorf("multihunk rebuilt diff missing %q", want)
		}
	}
}

func countKind(rows []Row, k Kind) int {
	n := 0
	for _, r := range rows {
		if r.Kind == k {
			n++
		}
	}
	return n
}

func findKind(rows []Row, k Kind) Row {
	for _, r := range rows {
		if r.Kind == k {
			return r
		}
	}
	return Row{}
}

func joined(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		var line, sign string
		switch r.Kind {
		case KindAdd:
			line, sign = r.NewText, "+"
		case KindDel:
			line, sign = r.OldText, "-"
		default:
			line, sign = r.OldText, " "
		}
		b.WriteString(sign + line + "\n")
	}
	return b.String()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
