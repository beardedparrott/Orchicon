package contextfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnion pins the union helper's contract: SORTED, deduped, trimmed, and
// safe on nil/empty input. The sort is the load-bearing part (see Union's doc
// comment): the skills/context section lands inside the prompt's cached static
// prefix, so the union's order must be a property of its CONTENTS, never of how
// the two lists arrived.
func TestUnion(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"nil nil", nil, nil, []string{}},
		{"empty empty", []string{}, []string{}, []string{}},
		{
			"interleaved and sorted",
			[]string{"/z/skill.md", "/a/skill.md"},
			[]string{"/m/dir"},
			[]string{"/a/skill.md", "/m/dir", "/z/skill.md"},
		},
		{
			"deduped across both sides",
			[]string{"/a", "/b"},
			[]string{"/b", "/c"},
			[]string{"/a", "/b", "/c"},
		},
		{
			"deduped within one side",
			[]string{"/a", "/a"},
			[]string{"/a"},
			[]string{"/a"},
		},
		{
			"trimmed and blanks dropped",
			[]string{"  /b  ", "   ", ""},
			[]string{"/a"},
			[]string{"/a", "/b"},
		},
		{"b only", nil, []string{"/only"}, []string{"/only"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Union(c.a, c.b)
			if len(got) != len(c.want) {
				t.Fatalf("Union(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Union(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
				}
			}
		})
	}
}

// TestUnionIsSortedRegardlessOfInputOrder is the determinism assertion on input
// order alone: two callers that pass the SAME selection in different orders must
// get the same union (and therefore the same rendered bytes, and therefore the
// same cached prefix).
func TestUnionIsSortedRegardlessOfInputOrder(t *testing.T) {
	a1 := []string{"/z", "/a"}
	b1 := []string{"/m"}
	a2 := []string{"/a", "/z"}
	b2 := []string{"/m"}
	if got, want := Union(a1, b1), Union(a2, b2); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("union depends on input order: %v vs %v", got, want)
	}
}

// TestRenderManifestByteStable is the AC4 assertion: two renders of the SAME
// selection are BYTE-IDENTICAL, including a directory-backed selection (whose
// entry order comes from a filesystem walk). This is what keeps the skills /
// context section from invalidating the prompt's cached static prefix
// (ADR-0009 D5) on every turn.
func TestRenderManifestByteStable(t *testing.T) {
	root := t.TempDir()
	// A directory with enough entries that a filesystem-dependent walk order
	// would be visibly different across runs.
	dir := filepath.Join(root, "skills")
	for _, name := range []string{"zeta.md", "alpha.md", "mid.md", "beta.md", "gamma.md", "delta.md"} {
		mustWrite(t, filepath.Join(dir, name), "skill body\n")
	}
	small := filepath.Join(root, "small.go")
	mustWrite(t, small, "package main\n")
	big := filepath.Join(root, "big.txt")
	mustWrite(t, big, strings.Repeat("z", ManifestInlineMaxBytes+512))

	// The SAME selection, deliberately passed in two different orders.
	sel1 := []string{dir, small, big}
	sel2 := []string{big, small, dir}

	first := RenderManifest("# Skills", sel1, root)
	second := RenderManifest("# Skills", sel1, root)
	if first != second {
		t.Fatalf("two renders of one selection differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	// Different input ORDER, same selection ⇒ same bytes.
	if third := RenderManifest("# Skills", sel2, root); third != first {
		t.Fatalf("render depends on input order:\n--- sel1 ---\n%s\n--- sel2 ---\n%s", first, third)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("expected a non-empty manifest")
	}
}

// TestRenderManifestDirectorySorted is the other half of AC4: a directory-backed
// skill renders a SORTED `path (N bytes)` manifest rather than inlining every
// file, so the section stays context-by-reference AND byte-stable.
func TestRenderManifestDirectorySorted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	// Written in non-lexical order on purpose.
	for _, name := range []string{"zebra.md", "apple.md", "mango.md"} {
		mustWrite(t, filepath.Join(dir, name), "body\n")
	}

	out := RenderManifest("# Skills", []string{dir}, root)
	if !strings.Contains(out, "(directory — read on demand)") {
		t.Fatalf("missing directory manifest marker:\n%s", out)
	}
	// The directory is NOT inlined file-by-file (`## <path>` headers are the
	// inline form).
	if strings.Contains(out, "## "+filepath.Join(dir, "apple.md")) {
		t.Fatalf("directory file was inlined instead of manifested:\n%s", out)
	}
	ai := strings.Index(out, "apple.md")
	mi := strings.Index(out, "mango.md")
	zi := strings.Index(out, "zebra.md")
	if ai < 0 || mi < 0 || zi < 0 {
		t.Fatalf("directory listing incomplete:\n%s", out)
	}
	if !(ai < mi && mi < zi) {
		t.Fatalf("directory manifest is not sorted (apple=%d mango=%d zebra=%d):\n%s", ai, mi, zi, out)
	}
	if !strings.Contains(out, "apple.md` (") {
		t.Fatalf("manifest entry missing its size:\n%s", out)
	}
}

// TestValidateWithinNamesProjectDir is the AC5 assertion: a skill path outside
// the project directory is rejected with the project dir NAMED in the error, so
// an operator is told which boundary they crossed rather than getting a bare
// "invalid path".
func TestValidateWithinNamesProjectDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := "/etc/passwd"
	err := ValidateWithin([]string{outside}, root)
	if err == nil {
		t.Fatal("expected an out-of-project skill path to be rejected")
	}
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("error must NAME the project dir %q, got: %v", root, err)
	}
	if !strings.Contains(err.Error(), "inside the project directory") {
		t.Fatalf("error must say 'inside the project directory', got: %v", err)
	}
}
