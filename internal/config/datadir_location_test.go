package config

// datadir_location_test.go — the state dir is anchored to an ABSOLUTE path at construction.
//
// REPORTED: a worker on a Claude container run found a `secrets/kek` in the worktree root. The KEK lives at
// filepath.Join(DataDir, "secrets", "kek"), so a relative DataDir makes its location depend on the working
// directory of whichever process reads it — and a worker's CWD is the bind-mounted worktree.
//
// Abs() is used rather than refusing a relative override because a relative state dir is legitimate in dev
// (BlobStoreDir has always defaulted to "./data/blobs"); what is not legitimate is letting its MEANING depend
// on the working directory.

import (
	"path/filepath"
	"testing"
)

// A RELATIVE override must still yield an absolute DataDir.
func TestDataDirIsAbsoluteWhenTheOverrideIsRelative(t *testing.T) {
	t.Setenv("ORCHICON_DATA_DIR", "./relative-state")

	cfg := Default()

	if !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("DataDir = %q, want an ABSOLUTE path — a relative state dir makes the KEK's location depend "+
			"on the process's working directory, which is how one reached a worker's bind-mounted worktree",
			cfg.DataDir)
	}
	if cfg.DataDir == "./relative-state" {
		t.Fatal("DataDir is still the raw relative override")
	}
}

// The default is already absolute on every platform; it must stay exactly as it was.
func TestDataDirIsAbsoluteByDefault(t *testing.T) {
	t.Setenv("ORCHICON_DATA_DIR", "")

	cfg := Default()

	if !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("default DataDir = %q, want absolute (defaultDataDir is per-OS and absolute)", cfg.DataDir)
	}
	if cfg.DataDir != defaultDataDir() {
		t.Errorf("default DataDir = %q, want %q — absolution must not rewrite an already-absolute default",
			cfg.DataDir, defaultDataDir())
	}
}

// An absolute override is honoured verbatim — no rewriting, no symlink resolution.
func TestAbsoluteOverrideIsHonouredVerbatim(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHICON_DATA_DIR", dir)

	if got := Default().DataDir; got != dir {
		t.Fatalf("DataDir = %q, want the override verbatim %q", got, dir)
	}
}
