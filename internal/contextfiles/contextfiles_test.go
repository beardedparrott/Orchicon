package contextfiles

import (
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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
