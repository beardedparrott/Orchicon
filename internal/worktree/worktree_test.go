package worktree

import (
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
		t.Fatal("expected a path-traversal error for absolute path")
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
