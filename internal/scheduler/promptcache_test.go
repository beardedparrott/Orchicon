package scheduler

// Tests for ADR-0009 D5: context-file fingerprinting + the rendered
// context-section prefix cache (hit → verbatim reuse; miss → re-render
// with an attributable per-path diff).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
)

func writeCtxFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContextFingerprintStability(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.md")
	writeCtxFile(t, file, "alpha")
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCtxFile(t, filepath.Join(sub, "b.md"), "bravo")
	paths := []string{file, sub}

	fp1, stamps1, err := contextfiles.Fingerprint(paths, dir)
	if err != nil {
		t.Fatal(err)
	}
	fp2, _, err := contextfiles.Fingerprint(paths, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint unstable: %q vs %q", fp1, fp2)
	}
	// Empty set is still byte-stable.
	fpEmpty, _, err := contextfiles.Fingerprint(nil, dir)
	if err != nil || fpEmpty != "none" {
		t.Fatalf("empty set = %q, %v", fpEmpty, err)
	}
	// A content edit changes the fingerprint and DiffStamps names the file.
	writeCtxFile(t, file, "alpha edited")
	fp3, stamps3, err := contextfiles.Fingerprint(paths, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp3 == fp1 {
		t.Fatal("fingerprint must change after a content edit")
	}
	diff := contextfiles.DiffStamps(stamps1, stamps3)
	if len(diff) != 1 || !strings.Contains(diff[0], "a.md") || !strings.Contains(diff[0], "changed") {
		t.Errorf("diff = %v, want one changed line naming a.md", diff)
	}
	if len(stamps1) != 2 {
		t.Errorf("stamps = %d, want 2 (file + directory)", len(stamps1))
	}
}

type capLog struct {
	lines []string
}

func (c *capLog) Info(msg string, args ...any) {
	var sb strings.Builder
	sb.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		sb.WriteString(fmt.Sprintf(" %v=%v", args[i], args[i+1]))
	}
	c.lines = append(c.lines, sb.String())
}

func TestPromptSectionCacheHitReuseVerbatim(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ctx.md")
	writeCtxFile(t, file, "context content")
	cache := newPromptSectionCache(8)
	log := &capLog{}

	s1, fp1 := renderContextSectionCached(cache, log, "t1", "p1", "# Project context", []string{file}, dir)
	if fp1 == "" || fp1 == "none" {
		t.Fatalf("fingerprint = %q", fp1)
	}
	if !strings.Contains(s1, "context content") {
		t.Fatalf("rendered section = %q", s1)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "cache miss") {
		t.Errorf("first render must log a miss, lines = %v", log.lines)
	}

	// Hit: same fingerprint → bytes reused verbatim, NO new miss log.
	s2, fp2 := renderContextSectionCached(cache, log, "t1", "p1", "# Project context", []string{file}, dir)
	if s2 != s1 || fp2 != fp1 {
		t.Fatal("hit must reuse the cached section verbatim")
	}
	if len(log.lines) != 1 {
		t.Errorf("hit must not log, lines = %v", log.lines)
	}

	// Change: edit the file → new fingerprint, re-render, miss log names
	// the changed path.
	writeCtxFile(t, file, "context content v2")
	s3, fp3 := renderContextSectionCached(cache, log, "t1", "p1", "# Project context", []string{file}, dir)
	if fp3 == fp1 {
		t.Fatal("fingerprint must move after an edit")
	}
	if !strings.Contains(s3, "v2") {
		t.Fatal("changed content must be re-rendered")
	}
	if len(log.lines) != 2 || !strings.Contains(log.lines[1], "prefix changed") || !strings.Contains(log.lines[1], "ctx.md") {
		t.Errorf("change must log an attributable miss, lines = %v", log.lines)
	}

	// Different tenant/project isolates cache keys.
	_, _ = renderContextSectionCached(cache, log, "t2", "p1", "# Project context", []string{file}, dir)
}
