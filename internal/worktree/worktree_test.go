package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, base, rel, content string) string {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBatchReadMultipleAndDedupe(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "a.go", "package a\n")
	writeFile(t, base, "b.go", "package b\n")
	out, err := BatchRead(base, ReadArgs{Paths: []string{"a.go", "b.go", "a.go"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "2 file(s)") {
		t.Fatalf("expected 2 files (dedupe), got: %s", out)
	}
	if !strings.Contains(out, "package a") || !strings.Contains(out, "package b") {
		t.Fatalf("missing content:\n%s", out)
	}
}

func TestBatchReadDirectoryExpansion(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "docs/one.md", "one\n")
	writeFile(t, base, "docs/two.md", "two\n")
	writeFile(t, base, "docs/ignore.txt", "ignore\n")
	out, err := BatchRead(base, ReadArgs{Paths: []string{"docs"}, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") || !strings.Contains(out, "ignore") {
		t.Fatalf("directory expansion should include all immediate files, got:\n%s", out)
	}
}

func TestBatchReadTruncationMarker(t *testing.T) {
	base := t.TempDir()
	big := strings.Repeat("x", 5000)
	writeFile(t, base, "big.txt", big)
	out, err := BatchRead(base, ReadArgs{Paths: []string{"big.txt"}, PerFile: 100})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got:\n%s", out)
	}
}

func TestBatchReadRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := BatchRead(base, ReadArgs{Paths: []string{"../secret"}}); err == nil {
		t.Fatal("expected a path-traversal error for ../")
	}
	if _, err := BatchRead(base, ReadArgs{Paths: []string{"/etc/passwd"}}); err == nil {
		t.Fatal("expected a path-traversal error for absolute path outside the allowed roots")
	}
}

func TestBatchReadAllowsTempDir(t *testing.T) {
	base := t.TempDir()
	// The runtime container's /tmp is a legitimate scratch the worker reaches
	// with absolute paths (Go build/tmp work); the tools must allow it.
	tmpFile := filepath.Join(os.TempDir(), "orchicon-batch-test-"+filepath.Base(t.TempDir())+".txt")
	if err := os.WriteFile(tmpFile, []byte("tmp-content"), 0o644); err != nil {
		t.Skipf("cannot write temp file: %v", err)
	}
	defer os.Remove(tmpFile)
	out, err := BatchRead(base, ReadArgs{Paths: []string{tmpFile}})
	if err != nil {
		t.Fatalf("batch_read on a /tmp absolute path should be allowed: %v", err)
	}
	if !strings.Contains(out, "tmp-content") {
		t.Fatalf("expected temp file content, got: %s", out)
	}
}

func TestBatchGrepMatchesAndContext(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "src/app.go", "line1\nfunc main() {\nline3\n")
	out, err := BatchGrep(base, GrepArgs{Patterns: []string{"main"}, Paths: []string{"src"}, ContextLines: 1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "src/app.go:2") {
		t.Fatalf("expected match line, got:\n%s", out)
	}
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line3") {
		t.Fatalf("expected context lines, got:\n%s", out)
	}
}

func TestBatchGrepRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := BatchGrep(base, GrepArgs{Patterns: []string{"x"}, Paths: []string{"../"}}); err == nil {
		t.Fatal("expected a path-traversal error")
	}
}

// TestBatchGrepRecursesIntoSubtree pins the root-cause fix for
// "batch_grep: no files to search (no matching files for paths: internal)" —
// a directory path must search its WHOLE subtree, not just its immediate
// files (internal/ and proto/ contain only subdirectories, so the old
// immediate-files-only expansion found nothing).
func TestBatchGrepRecursesIntoSubtree(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "internal/x/a.go", "package x\n")
	writeFile(t, base, "internal/x/y/b.go", "package y\nneedle here\n")
	out, err := BatchGrep(base, GrepArgs{Patterns: []string{"needle"}, Paths: []string{"internal"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "internal/x/y/b.go") {
		t.Fatalf("expected a match in the nested file, got:\n%s", out)
	}
	if strings.Contains(out, "no files to search") {
		t.Fatalf("subtree search must not report no files:\n%s", out)
	}
}

func TestBatchReadRecursesIntoSubtree(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "docs/a.md", "alpha\n")
	writeFile(t, base, "docs/sub/b.md", "beta\n")
	out, err := BatchRead(base, ReadArgs{Paths: []string{"docs"}, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("directory expansion should recurse into subdirectories, got:\n%s", out)
	}
}

// TestBatchGrepPrunesNoiseDirs verifies the recursive walk skips VCS
// metadata and vendored/build output — a whole-tree search must never
// return matches from .git or node_modules.
func TestBatchGrepPrunesNoiseDirs(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, ".git/config", "needle in git\n")
	writeFile(t, base, "node_modules/pkg/index.js", "needle in node_modules\n")
	writeFile(t, base, "src/real.go", "real needle\n")
	out, err := BatchGrep(base, GrepArgs{Patterns: []string{"needle"}, Paths: []string{"."}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "src/real.go") {
		t.Fatalf("expected the real match, got:\n%s", out)
	}
	if strings.Contains(out, ".git/") || strings.Contains(out, "node_modules/") {
		t.Fatalf("pruned dirs must not be searched:\n%s", out)
	}
}

// TestBatchGrepReportsWalkCap verifies a truncated walk is flagged in the
// summary, so a partial search is never mistaken for an exhaustive one.
func TestBatchGrepReportsWalkCap(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < defaultMaxGrepFiles+1; i++ {
		writeFile(t, base, "many/f"+fmt.Sprint(i)+".txt", "x")
	}
	out, err := BatchGrep(base, GrepArgs{Patterns: []string{"needle"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "walk capped at 512 files") {
		t.Fatalf("expected the walk-cap note in the summary, got:\n%s", out)
	}
}

// TestBatchReadReportsWalkCap mirrors the grep cap test for batch_read's
// read-sized walk caps.
func TestBatchReadReportsWalkCap(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < defaultMaxFiles+1; i++ {
		writeFile(t, base, "many/f"+fmt.Sprint(i)+".txt", "x")
	}
	out, err := BatchRead(base, ReadArgs{Paths: []string{"many"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "walk capped at 64 files") {
		t.Fatalf("expected the walk-cap note in the summary, got:\n%s", out)
	}
}

func TestBatchWriteCreateOverwriteEditAppend(t *testing.T) {
	base := t.TempDir()
	out, err := BatchWrite(base, WriteArgs{Writes: []Write{
		{Path: "a.txt", Mode: "create", Content: "hello"},
		{Path: "a.txt", Mode: "append", Content: " world"},
		{Path: "a.txt", Mode: "edit", Old: "world", New: "orl"},
	}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "3 write(s)") {
		t.Fatalf("expected 3 applied, got: %s", out)
	}
	got, _ := os.ReadFile(filepath.Join(base, "a.txt"))
	if string(got) != "hello orl" {
		t.Fatalf("content = %q, want %q", got, "hello orl")
	}
}

func TestBatchWriteDryRunAbortsOnBadEdit(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "a.txt", "original")
	_, err := BatchWrite(base, WriteArgs{Writes: []Write{
		{Path: "b.txt", Mode: "create", Content: "should not be written"},
		{Path: "a.txt", Mode: "edit", Old: "does-not-exist", New: "x"},
	}})
	if err == nil {
		t.Fatal("expected a dry-run abort because the edit substring is absent")
	}
	// Nothing applied, including the first create.
	if _, sErr := os.Stat(filepath.Join(base, "b.txt")); !os.IsNotExist(sErr) {
		t.Fatal("create must not have been applied after a dry-run abort")
	}
}

func TestBatchWriteRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := BatchWrite(base, WriteArgs{Writes: []Write{{Path: "../x", Mode: "create"}}}); err == nil {
		t.Fatal("expected a path-traversal error")
	}
}
