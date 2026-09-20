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
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"a.go", "b.go", "a.go"}})
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
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"docs"}, MaxBytes: 100000})
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
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"big.txt"}, PerFile: 100})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got:\n%s", out)
	}
}

func TestBatchReadRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"../secret"}}); err == nil {
		t.Fatal("expected a path-traversal error for ../")
	}
	if _, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"/etc/passwd"}}); err == nil {
		t.Fatal("expected a path-traversal error for absolute path outside the allowed roots")
	}
}

func TestBatchReadAllowsTempDir(t *testing.T) {
	base := t.TempDir()
	// The sanctioned scratch dir /tmp/orchicon (guard.ScratchDir /
	// opencode.ScratchDir) is the one writable area outside the worktree the
	// tools may read AND write; this replaces the old broad os.TempDir() allow.
	if err := os.MkdirAll(DefaultScratchDir, 0o755); err != nil {
		t.Skipf("cannot create scratch dir %s: %v", DefaultScratchDir, err)
	}
	tmpFile := filepath.Join(DefaultScratchDir, "orchicon-batch-test-"+filepath.Base(t.TempDir())+".txt")
	if err := os.WriteFile(tmpFile, []byte("tmp-content"), 0o644); err != nil {
		t.Skipf("cannot write temp file: %v", err)
	}
	defer os.Remove(tmpFile)
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{tmpFile}})
	if err != nil {
		t.Fatalf("batch_read on a /tmp/orchicon absolute path should be allowed: %v", err)
	}
	if !strings.Contains(out, "tmp-content") {
		t.Fatalf("expected temp file content, got: %s", out)
	}
}

func TestBatchGrepMatchesAndContext(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "src/app.go", "line1\nfunc main() {\nline3\n")
	out, err := BatchGrep(BaseFor(base), GrepArgs{Patterns: []string{"main"}, Paths: []string{"src"}, ContextLines: 1})
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
	if _, err := BatchGrep(BaseFor(base), GrepArgs{Patterns: []string{"x"}, Paths: []string{"../"}}); err == nil {
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
	out, err := BatchGrep(BaseFor(base), GrepArgs{Patterns: []string{"needle"}, Paths: []string{"internal"}})
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
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"docs"}, MaxBytes: 100000})
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
	out, err := BatchGrep(BaseFor(base), GrepArgs{Patterns: []string{"needle"}, Paths: []string{"."}})
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
	out, err := BatchGrep(BaseFor(base), GrepArgs{Patterns: []string{"needle"}})
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
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"many"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "walk capped at 64 files") {
		t.Fatalf("expected the walk-cap note in the summary, got:\n%s", out)
	}
}

func TestBatchWriteCreateOverwriteEditAppend(t *testing.T) {
	base := t.TempDir()
	out, err := BatchWrite(BaseFor(base), WriteArgs{Writes: []Write{
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
	_, err := BatchWrite(BaseFor(base), WriteArgs{Writes: []Write{
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
	if _, err := BatchWrite(BaseFor(base), WriteArgs{Writes: []Write{{Path: "../x", Mode: "create"}}}); err == nil {
		t.Fatal("expected a path-traversal error")
	}
}

func TestBatchReadParallelOrderDeterministic(t *testing.T) {
	// D3: independent file reads run concurrently, but the RESULT must be in
	// deterministic path order (request order), never completion order.
	base := t.TempDir()
	for i := 0; i < 24; i++ {
		// Deliberately big-ish content so reads actually take measurable time.
		writeFile(t, base, fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("content-%02d-%s\n", i, strings.Repeat("z", 20000)))
	}
	paths := make([]string, 24)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%02d.txt", i)
	}
	// Force high parallelism so completion order would differ from request order.
	t.Setenv("ORCHICON_TOOL_PARALLELISM", "24")
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: paths, MaxBytes: 1_000_000})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	last := -1
	for i := 0; i < 24; i++ {
		marker := fmt.Sprintf("==> f%02d.txt", i)
		idx := strings.Index(out, marker)
		if idx < 0 {
			t.Fatalf("missing file %d in output", i)
		}
		if idx < last {
			t.Fatalf("output order not deterministic: file %d appeared before file %d", i, last)
		}
		last = idx
	}
}

func TestBatchReadParallelismOneIsSerial(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "a.txt", "aaa")
	writeFile(t, base, "b.txt", "bbb")
	t.Setenv("ORCHICON_TOOL_PARALLELISM", "1")
	out, err := BatchRead(BaseFor(base), ReadArgs{Paths: []string{"a.txt", "b.txt"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Index(out, "==> a.txt") > strings.Index(out, "==> b.txt") {
		t.Fatalf("serial mode must keep request order:\n%s", out)
	}
}

// TestBatchReadProjectRootRunFile verifies AC1: a read may resolve the
// project root (run-state .orchicon/<run>/ files and architecture-notes),
// which live at the project root — a sibling of the run worktree. Both a
// `..`-relative path and an absolute project-root path must work.
func TestBatchReadProjectRootRunFile(t *testing.T) {
	proj := t.TempDir()
	run := "run-123"
	wt := filepath.Join(proj, ".orchicon-worktrees", run)
	if err := os.MkdirAll(filepath.Join(proj, ".orchicon", run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	facts := "FACTS LEARNED: the project root is readable.\n"
	if err := os.WriteFile(filepath.Join(proj, ".orchicon", run, "facts_learned"), []byte(facts), 0o644); err != nil {
		t.Fatal(err)
	}
	b := Base{Worktree: wt, ProjectRoot: proj, ScratchDir: DefaultScratchDir}

	// Relative `..` traversal that resolves back under the project root.
	out, err := BatchRead(b, ReadArgs{Paths: []string{"../../.orchicon/" + run + "/facts_learned"}})
	if err != nil {
		t.Fatalf("relative project-root read should be allowed: %v", err)
	}
	if !strings.Contains(out, facts) {
		t.Fatalf("expected run-file content, got: %s", out)
	}

	// Absolute project-root path.
	abs := filepath.Join(proj, ".orchicon", run, "facts_learned")
	out, err = BatchRead(b, ReadArgs{Paths: []string{abs}})
	if err != nil {
		t.Fatalf("absolute project-root read should be allowed: %v", err)
	}
	if !strings.Contains(out, facts) {
		t.Fatalf("expected run-file content (absolute), got: %s", out)
	}
}

// TestBatchWriteScratchDir verifies AC2: a write may target the sanctioned
// scratch dir (/tmp/orchicon) via an absolute path, and persists.
func TestBatchWriteScratchDir(t *testing.T) {
	if err := os.MkdirAll(DefaultScratchDir, 0o755); err != nil {
		t.Skipf("cannot create scratch dir %s: %v", DefaultScratchDir, err)
	}
	base := t.TempDir()
	b := Base{Worktree: base, ScratchDir: DefaultScratchDir}
	target := filepath.Join(DefaultScratchDir, "batch-write-test-"+filepath.Base(t.TempDir())+".txt")
	defer os.Remove(target)

	out, err := BatchWrite(b, WriteArgs{Writes: []Write{{Path: target, Mode: "create", Content: "scratch-content"}}})
	if err != nil {
		t.Fatalf("batch_write to scratch dir should be allowed: %v", err)
	}
	if !strings.Contains(out, "1 write(s)") {
		t.Fatalf("expected 1 applied write, got: %s", out)
	}
	got, rerr := os.ReadFile(target)
	if rerr != nil || string(got) != "scratch-content" {
		t.Fatalf("scratch write did not persist: content=%q err=%v", got, rerr)
	}
}

// TestBatchWriteProjectRootStillBlocked verifies AC2's counter: a WRITE never
// gets the ProjectRoot scope, so `..` that escapes toward the project root is
// still rejected (batch_write must not land in the main checkout).
func TestBatchWriteProjectRootStillBlocked(t *testing.T) {
	proj := t.TempDir()
	wt := filepath.Join(proj, ".orchicon-worktrees", "run")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	b := Base{Worktree: wt, ProjectRoot: proj, ScratchDir: DefaultScratchDir}
	if _, err := BatchWrite(b, WriteArgs{Writes: []Write{{Path: "../../x.txt", Mode: "create", Content: "nope"}}}); err == nil {
		t.Fatal("expected a path-traversal error when batch_write escapes toward the project root")
	}
}

// TestBatchWriteLargePayload verifies AC5: a single batch_write whose content
// is well over 64 KiB succeeds and persists (the bufio.Scanner token cap
// previously made the whole MCP server exit).
func TestBatchWriteLargePayload(t *testing.T) {
	base := t.TempDir()
	b := BaseFor(base)
	big := strings.Repeat("x", 200_000)
	out, err := BatchWrite(b, WriteArgs{Writes: []Write{{Path: "big.txt", Mode: "create", Content: big}}})
	if err != nil {
		t.Fatalf("large batch_write failed: %v", err)
	}
	if !strings.Contains(out, "1 write(s)") {
		t.Fatalf("expected 1 applied write, got: %s", out)
	}
	got, rerr := os.ReadFile(filepath.Join(base, "big.txt"))
	if rerr != nil || len(got) != len(big) {
		t.Fatalf("large write did not persist all bytes: len=%d err=%v", len(got), rerr)
	}
}
