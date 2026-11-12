package main

// runtime_copyself_test.go — the daemon's bind-mount copy must follow CONTENT, not mtime.
//
// WHY THIS IS THE ROOT CAUSE OF THE STALE-DAEMON CLASS. The daemon copies its own executable to the
// socket dir and bind-mounts THAT copy into every runtime container. copySelf decided "the copy is
// current" by comparing ModTime and returning early when the source was not NEWER:
//
//	if !si.ModTime().After(st.ModTime()) { return nil }
//
// bin/orchicon is rebuilt by every `make build`, but its mtime can be OLDER than the copy — a
// `git checkout` or `cp -p` preserves timestamps, a clock can move backwards between a build and a
// start, and a restored file carries its original time. When that happens the copy is SKIPPED, every
// container runs the previous binary, and RESTARTING DOES NOT HELP because the restart skips again.
//
// MEASURED ON A REAL HOST: the stable copy was 13 bytes larger than bin/orchicon with a newer mtime,
// so `make runtime-daemon` reported success while the daemon served a binary that no longer existed on
// disk — eight days stale, every restart "succeeding".

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCopySelfReplacesACopyWhoseMtimeIsNewer is the reported shape: a stale copy that a timestamp
// comparison would keep.
func TestCopySelfReplacesACopyWhoseMtimeIsNewer(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "orchicon")
	dst := filepath.Join(dir, "stable")

	if err := os.WriteFile(src, []byte("NEW BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The copy is NEWER than the source — exactly the state that made the old check refuse.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(dst, future, future); err != nil {
		t.Fatal(err)
	}

	if err := copySelf(src, dst); err != nil {
		t.Fatalf("copySelf: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("the stable copy still holds %q.\n"+
			"  A newer mtime on the copy must not keep the daemon on a stale binary: every container\n"+
			"  bind-mounts this file, and a restart that skips the copy cannot fix it.", got)
	}
}

// TestCopySelfSkipsAnIdenticalCopy — the idempotence the daemon relies on at every start.
func TestCopySelfSkipsAnIdenticalCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "orchicon")
	dst := filepath.Join(dir, "stable")
	body := []byte("SAME BINARY")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// An mtime the copy will keep: if copySelf rewrote it, the mtime would move forward.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dst, past, past); err != nil {
		t.Fatal(err)
	}
	if err := copySelf(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.ModTime().After(past.Add(time.Minute)) {
		t.Error("copySelf rewrote an IDENTICAL copy — the daemon start does this every time, and a\n" +
			"  ~100 MB rewrite on every start is wasted work for no change")
	}
}

// TestCopySelfDetectsASameSizeDifferentContent — the case a size check alone would miss, and the one a
// rebuilt binary of identical length produces.
func TestCopySelfDetectsASameSizeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "orchicon")
	dst := filepath.Join(dir, "stable")
	if err := os.WriteFile(src, []byte("AAAA"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("BBBB"), 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dst, future, future); err != nil {
		t.Fatal(err)
	}
	if err := copySelf(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "AAAA" {
		t.Errorf("a same-size, different-content copy was kept as current (%q) — a rebuild of identical\n"+
			"  length would leave the daemon on the old binary", got)
	}
}
