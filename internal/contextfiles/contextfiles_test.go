package contextfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []string{"/a/b.go", "/c/dir", "/x/y/z.txt"}
	if err := Validate(ok); err != nil {
		t.Fatalf("expected valid list, got %v", err)
	}

	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"too many", makePaths(MaxContextFiles + 1), "exceeds max"},
		{"empty entry", []string{"/a", "  "}, "must not be empty"},
		{"too long", []string{"/a", strings.Repeat("x", MaxFilePathLen+1)}, "exceeds max length"},
		{"relative", []string{"a/b.go"}, "must be an absolute path"},
		{"traversal", []string{"/a/../b"}, "path-traversal"},
		{"traversal middle", []string{"/a/x..y/b"}, "path-traversal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.paths)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate(%v) = %v, want error containing %q", c.paths, err, c.want)
			}
		})
	}
}

func makePaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "/path/file"
	}
	return out
}

// TestValidateWithin verifies the project-directory confinement rule: a
// context path must be the project dir itself or a descendant, and an
// empty project dir (no directory configured yet) skips the confinement
// while still applying the base Validate rules.
func TestValidateWithin(t *testing.T) {
	root := "/home/user/projects/MyApp"

	ok := [][]string{
		{root},                               // the dir itself
		{root + "/file.go"},                  // direct file
		{root + "/src/lib"},                  // subdirectory
		{root + "/src/lib/util.go"},          // nested file
		{root + "/src", root + "/README.md"}, // mixed
	}
	for _, paths := range ok {
		if err := ValidateWithin(paths, root); err != nil {
			t.Errorf("ValidateWithin(%v, %q) unexpected error: %v", paths, root, err)
		}
	}

	rejected := [][]string{
		{"/home/user/projects/Other/file.go"}, // sibling project
		{"/home/user/projects"},               // parent dir
		{"/home/user/projects/MyApp-sibling"}, // name-prefix sibling (not a child)
		{root + "/file.go", "/etc/passwd"},    // one bad path among good
	}
	for _, paths := range rejected {
		err := ValidateWithin(paths, root)
		if err == nil || !strings.Contains(err.Error(), "inside the project directory") {
			t.Errorf("ValidateWithin(%v, %q) = %v, want 'inside the project directory'", paths, root, err)
		}
	}

	// Empty project dir: confinement skipped, base Validate still applies.
	if err := ValidateWithin([]string{"/etc/passwd"}, ""); err != nil {
		t.Errorf("ValidateWithin with empty project dir should skip confinement, got %v", err)
	}
	if err := ValidateWithin([]string{"relative/path"}, ""); err == nil {
		t.Error("ValidateWithin with empty project dir must still reject relative paths")
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name       string
		p          string
		projectDir string
		want       string
	}{
		{"absolute passes through", "/a/b.go", "/proj", "/a/b.go"},
		{"relative joined to project", "src/a.go", "/proj", "/proj/src/a.go"},
		{"relative with no project dropped", "src/a.go", "", ""},
		{"cleaned", "/a/b/../c.go", "/proj", "/a/c.go"},
		{"trimmed", "  /a/b.go  ", "/proj", "/a/b.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Resolve(c.p, c.projectDir); got != c.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", c.p, c.projectDir, got, c.want)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	paths := []string{"/a/b.go", "/c/dir"}
	data, err := ToJSON(paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != paths[0] || got[1] != paths[1] {
		t.Fatalf("round trip = %v", got)
	}
	// nil -> [] never null
	data, _ = ToJSON(nil)
	if string(data) != "[]" {
		t.Fatalf("ToJSON(nil) = %s, want []", data)
	}
	// empty input -> nil, no error
	got, err = FromJSON(nil)
	if err != nil || got != nil {
		t.Fatalf("FromJSON(nil) = %v, %v", got, err)
	}
}

func TestWalkDir(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "a")
	mustWrite(t, filepath.Join(root, "sub", "b.go"), "b")
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.txt"), "c")
	// noise dirs skipped
	mustWrite(t, filepath.Join(root, "node_modules", "junk.js"), "junk")
	mustWrite(t, filepath.Join(root, ".git", "config"), "cfg")
	mustWrite(t, filepath.Join(root, "dist", "out.js"), "out")

	got, err := WalkDir(root, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("WalkDir returned %d entries, want 3: %v", len(got), got)
	}
	for _, p := range got {
		if strings.Contains(p, "node_modules") || strings.Contains(p, ".git") || strings.Contains(p, "dist") {
			t.Fatalf("noise dir leaked into walk: %s", p)
		}
	}

	// bounded
	got, err = WalkDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("bounded walk returned %d, want 2", len(got))
	}
}

func TestRenderFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.go")
	mustWrite(t, f, "package main\n")

	out := Render("# Project context", []string{f}, root)
	if !strings.Contains(out, "## "+f) {
		t.Fatalf("missing file header:\n%s", out)
	}
	if !strings.Contains(out, "package main") {
		t.Fatalf("missing file contents:\n%s", out)
	}
}

func TestRenderDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ctxdir")
	mustWrite(t, filepath.Join(dir, "a.go"), "package a\n")
	mustWrite(t, filepath.Join(dir, "nested", "b.txt"), "hello\n")

	out := Render("# Work item context", []string{dir}, root)
	if !strings.Contains(out, "(directory)") {
		t.Fatalf("missing directory marker:\n%s", out)
	}
	if !strings.Contains(out, `Do NOT attempt to open the directory path itself as a file`) {
		t.Fatalf("missing not-a-file instruction:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.txt") {
		t.Fatalf("missing directory listing:\n%s", out)
	}
}

func TestRenderMissingAndEmpty(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope.go")
	out := Render("# Project context", []string{missing}, root)
	if !strings.Contains(out, "could not read") {
		t.Fatalf("missing error note:\n%s", out)
	}
	// empty paths -> empty section
	if out := Render("# Project context", nil, root); out != "" {
		t.Fatalf("expected empty section, got %q", out)
	}
	// relative path with no project dir -> dropped, no section
	if out := Render("# Project context", []string{"rel/path"}, ""); out != "" {
		t.Fatalf("expected empty section for unresolvable path, got %q", out)
	}
}

// mustWrite creates (or truncates) a file with the given content in a test.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRenderManifest pins the context-by-reference renderer: a small file is
// inlined (the never-blind floor), a large file becomes a manifest entry with
// a "read on demand" instruction, and a directory becomes a path+size listing
// instead of "read EVERY file".
func TestRenderManifest(t *testing.T) {
	root := t.TempDir()
	small := filepath.Join(root, "small.go")
	mustWrite(t, small, "package main\n")
	// A file larger than ManifestInlineMaxBytes.
	big := filepath.Join(root, "big.txt")
	mustWrite(t, big, strings.Repeat("z", ManifestInlineMaxBytes+1024))
	dir := filepath.Join(root, "ctxdir")
	mustWrite(t, filepath.Join(dir, "a.go"), "package a\n")
	mustWrite(t, filepath.Join(dir, "nested", "b.txt"), "hello\n")

	out := RenderManifest("# Project context", []string{small, big, dir}, root)
	// Small file inlined.
	if !strings.Contains(out, "## "+small) || !strings.Contains(out, "package main") {
		t.Fatalf("small file was not inlined into the manifest:\n%s", out)
	}
	// Large file is a manifest entry, not inlined.
	if strings.Contains(out, "## "+big+"\n\n```") {
		t.Fatalf("large file was inlined instead of manifested")
	}
	if !strings.Contains(out, "(read this file on demand") {
		t.Fatalf("large file missing 'read on demand' instruction:\n%s", out)
	}
	// Directory is a manifest listing, not "read EVERY file".
	if !strings.Contains(out, "(directory — read on demand)") {
		t.Fatalf("directory missing manifest marker:\n%s", out)
	}
	if strings.Contains(out, "Read EVERY file") {
		t.Fatalf("directory still says 'read EVERY file' instead of read-on-demand")
	}
	if !strings.Contains(out, "read the specific files you need") {
		t.Fatalf("missing directory read-on-demand guidance:\n%s", out)
	}
	// Manifest is present.
	if !strings.Contains(out, "big.txt (") {
		t.Fatalf("missing big.txt size entry:\n%s", out)
	}
}

// TestResolveWithin verifies the shared path resolver used by the
// project-directory tools: it resolves a caller-supplied path against a
// project root, accepting the root itself and descendants while rejecting
// `..` escapes, absolute out-of-root paths, and name-prefix siblings.
// These always-run tests are what the acceptance criteria
// "unit tests cover traversal attempts" gates on.
func TestResolveWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	mustWrite(t, filepath.Join(root, "a.txt"), "a")
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), "b")
	mustWrite(t, filepath.Join(root, "sub", "c.txt"), "c")

	for _, tc := range []struct {
		name string
		p    string
		want string
	}{
		{"root itself", root, root},
		{"dot", ".", root},
		{"relative file", "a.txt", filepath.Join(root, "a.txt")},
		{"relative subdir", "sub", filepath.Join(root, "sub")},
		{"nested relative", filepath.Join("sub", "b.txt"), filepath.Join(root, "sub", "b.txt")},
		{"absolute inside", filepath.Join(root, "sub", "c.txt"), filepath.Join(root, "sub", "c.txt")},
		{"cleaned dot segment", filepath.Join(root, "sub", ".", "b.txt"), filepath.Join(root, "sub", "b.txt")},
		{"cleaned doubled slash", root + string(filepath.Separator) + string(filepath.Separator) + "a.txt", filepath.Join(root, "a.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveWithin(root, tc.p)
			if err != nil {
				t.Fatalf("ResolveWithin(%q) unexpected error: %v", tc.p, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveWithin(%q) = %q, want %q", tc.p, got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		p    string
		want string
	}{
		{"parent traversal", filepath.Join("..", "x"), "escapes"},
		{"double traversal", filepath.Join("..", "..", "x"), "escapes"},
		{"traversal through root", filepath.Join("sub", "..", "..", "x"), "escapes"},
		{"absolute out of root", filepath.Join(root, "..", "other", "passwd"), "escapes"},
		{"absolute sibling project", filepath.Join(filepath.Dir(root), "other", "f"), "escapes"},
		{"name-prefix sibling", root + "-sibling", "escapes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveWithin(root, tc.p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveWithin(%q) = %v, want error containing %q", tc.p, err, tc.want)
			}
		})
	}

	// Empty and overlong paths are rejected up front.
	if _, err := ResolveWithin(root, "   "); err == nil {
		t.Error("empty path should be rejected")
	}
	if _, err := ResolveWithin(root, strings.Repeat("x", MaxFilePathLen+1)); err == nil {
		t.Error("overlong path should be rejected")
	}
	if _, err := ResolveWithin("", "a.txt"); err == nil {
		t.Error("empty root should be rejected")
	}
	if _, err := ResolveWithin(root, strings.Repeat("/x", MaxFilePathLen)); err == nil {
		t.Error("overlong absolute path should be rejected")
	}

	// A nonexistent target keeps its clean lexical form — the downstream
	// operation reports the not-found.
	got, err := ResolveWithin(root, filepath.Join("sub", "nope.txt"))
	if err != nil {
		t.Fatalf("nonexistent target should resolve lexically, got: %v", err)
	}
	if want := filepath.Join(root, "sub", "nope.txt"); got != want {
		t.Fatalf("nonexistent target resolved to %q, want %q", got, want)
	}
}

// TestResolveWithinSymlinkEscape verifies symlink escapes are rejected and
// in-root symlinks are allowed. The EvalSymlinks pass resolves the target
// and re-checks containment against the evaluated root, so a link that
// points out of the root — file, directory, or nested — never resolves.
func TestResolveWithinSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "secret")
	mustWrite(t, filepath.Join(outside, "in.txt"), "in")

	root := filepath.Join(t.TempDir(), "proj")
	mustWrite(t, filepath.Join(root, "ok.txt"), "ok")

	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("create escape symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "escape"), filepath.Join(root, "nested")); err != nil {
		t.Fatalf("create nested symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "ok.txt"), filepath.Join(root, "inlink")); err != nil {
		t.Fatalf("create in-root symlink: %v", err)
	}

	for _, tc := range []struct {
		name string
		p    string
	}{
		{"symlink to outside file", filepath.Join(root, "leak")},
		{"symlinked directory escape", filepath.Join(root, "escape")},
		{"nested symlink escape", filepath.Join(root, "nested")},
		{"through escaped dir", filepath.Join(root, "escape", "secret.txt")},
		{"through nested escaped dir", filepath.Join(root, "nested", "secret.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveWithin(root, tc.p); err == nil {
				t.Errorf("ResolveWithin(%q) should reject the symlink escape", tc.p)
			}
		})
	}

	// An in-root symlink resolves and keeps its lexical form.
	got, err := ResolveWithin(root, "inlink")
	if err != nil {
		t.Fatalf("in-root symlink unexpectedly rejected: %v", err)
	}
	if want := filepath.Join(root, "inlink"); got != want {
		t.Fatalf("in-root symlink resolved to %q, want %q", got, want)
	}
}

// TestResolveWithinSymlinkedRoot verifies a project_dir that itself lives
// under a symlinked path (e.g. a symlinked home) resolves correctly: the
// root is evaluated once, targets inside it pass, and an escape through
// the symlinked root's real path is still rejected.
func TestResolveWithinSymlinkedRoot(t *testing.T) {
	realRoot := filepath.Join(t.TempDir(), "real")
	mustWrite(t, filepath.Join(realRoot, "f.txt"), "f")
	linkRoot := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := ResolveWithin(linkRoot, "f.txt")
	if err != nil {
		t.Fatalf("resolve through symlinked root: %v", err)
	}
	if want := filepath.Join(linkRoot, "f.txt"); got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}

	// An escape via the symlinked root's real path is still rejected.
	if _, err := ResolveWithin(linkRoot, filepath.Join(realRoot, "..", "evil")); err == nil {
		t.Error("escape through the root's real path should be rejected")
	}
}

// TestIsNoiseDir verifies the listing skip set matches WalkDir's.
func TestIsNoiseDir(t *testing.T) {
	for _, name := range []string{".git", "node_modules", "vendor", "dist", "build", ".venv", "__pycache__", ".orchicon", ".cache"} {
		if !IsNoiseDir(name) {
			t.Errorf("IsNoiseDir(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"src", "README.md", "go.mod", ".gitignore", "somegit"} {
		if IsNoiseDir(name) {
			t.Errorf("IsNoiseDir(%q) = true, want false", name)
		}
	}
}

// TestRenderCumulativeBudget verifies the cumulative inline budget: when
// many per-file capped inlines exceed MaxInlineContextBytes together, later
// files degrade to a "read from disk" note instead of being fully inlined —
// so a large context selection can't re-inflate every turn's model context.
func TestRenderCumulativeBudget(t *testing.T) {
	root := t.TempDir()
	// A file at the per-file cap: many of them sum past the cumulative
	// budget (8 × 64KiB > 384KiB).
	chunk := strings.Repeat("y", MaxInlineFileBytes)
	files := make([]string, 8)
	paths := make([]string, 0, 8)
	for i := range files {
		p := filepath.Join(root, fmt.Sprintf("f%d.txt", i))
		mustWrite(t, p, chunk)
		files[i] = p
		paths = append(paths, p)
	}

	out := Render("# Project context", paths, root)
	if !strings.Contains(out, "## "+files[0]) {
		t.Fatalf("missing first file header")
	}
	if !strings.Contains(out, "context budget reached") {
		t.Fatalf("cumulative budget did not kick in")
	}
	last := files[len(files)-1]
	if strings.Contains(out, "## "+last+"\n\n```") {
		t.Fatalf("final file was fully inlined past the cumulative budget")
	}
}
