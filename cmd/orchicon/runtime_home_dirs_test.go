package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// The runtime container's $HOME directories are created FROM $HOME at start, not baked into the image
// at build time. The image used to `mkdir -p /home/<the author's username>/...`, which is fixed while
// HOME is not — so on any other machine the directories were created where nothing looked for them
// and the operator's real home had none, leaving the runtime uid unable to write `$HOME/.config` and
// the rest. Reading $HOME here is correct for every user by construction.
func TestEnsureHomeDirsUsesTheHomeItWasGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ensureHomeDirs(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	for _, sub := range []string{".local/share", ".local/state", ".config", ".cache", ".npm", "projects"} {
		p := filepath.Join(home, sub)
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s was not created: %v", sub, err)
			continue
		}
		if !st.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
}

// A HOME that cannot be written must NOT be fatal: the runtime has to start regardless, because a
// read-only mount over one of these paths is a normal condition rather than a broken host.
func TestEnsureHomeDirsIsBestEffort(t *testing.T) {
	// A path under a FILE cannot be created, so every MkdirAll below fails.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("ensureHomeDirs panicked on an unusable HOME: %v", rec)
		}
	}()
	ensureHomeDirs(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// And an EMPTY HOME is a no-op rather than a MkdirAll against a relative path — which would create
// `.local/share` in the process's working directory.
func TestEnsureHomeDirsIgnoresAnEmptyHome(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", "")

	// UserHomeDir falls back to the passwd entry, so force the empty case at the source by pointing
	// HOME at nothing and asserting the only thing that matters: no relative dirs are created here.
	ensureHomeDirs(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if _, err := os.Stat(filepath.Join(dir, ".local")); err == nil {
		t.Error("an empty HOME created a RELATIVE .local/ in the working directory")
	}
}
