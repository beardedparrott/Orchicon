package orchicon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxed bash redirects XDG_DATA_HOME into the suite's PRIVATE scratch, and a podman image store built
// there used to live on in /tmp (RAM, on a tmpfs) after the execution: 34.7 GB on one machine. The scratch is
// per session now, and removed when the execution ends.

func suiteWithScratch(t *testing.T) *HostTools {
	t.Helper()
	h := NewHostTools(t.TempDir(), t.TempDir())
	h.base.ScratchDir = t.TempDir()
	return h
}

func xdgOf(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "XDG_DATA_HOME="); ok {
			return v
		}
	}
	return ""
}

func TestEachSuiteGetsItsOwnScratch(t *testing.T) {
	a, b := suiteWithScratch(t), suiteWithScratch(t)
	b.base.ScratchDir = a.base.ScratchDir
	if xdgOf(a.bashEnviron()) == xdgOf(b.bashEnviron()) {
		t.Fatal("two executions share one XDG_DATA_HOME — one's cleanup would delete the other's store")
	}
	if !strings.HasPrefix(xdgOf(a.bashEnviron()), filepath.Join(a.base.ScratchDir, "sessions")+string(filepath.Separator)) {
		t.Errorf("XDG_DATA_HOME is not under <scratch>/sessions/: %s", xdgOf(a.bashEnviron()))
	}
}

func TestCleanupScratchRemovesWhatBashBuilt(t *testing.T) {
	h := suiteWithScratch(t)
	xdg := xdgOf(h.bashEnviron()) // creates <session>/xdg
	store := filepath.Join(xdg, "containers", "storage", "overlay", "layer", "blob")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte("image layer"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := filepath.Dir(xdg)

	if err := h.CleanupScratch(); err != nil {
		t.Fatalf("CleanupScratch: %v", err)
	}
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Errorf("the execution's scratch survived its cleanup: %v", err)
	}
	// Idempotent, and safe on a suite that never ran bash.
	if err := h.CleanupScratch(); err != nil {
		t.Errorf("second cleanup errored: %v", err)
	}
	if err := suiteWithScratch(t).CleanupScratch(); err != nil {
		t.Errorf("cleanup of a suite with no scratch errored: %v", err)
	}
}

func TestAnAskConversationKeepsOneScratchAcrossCalls(t *testing.T) {
	scratch := t.TempDir()
	mk := func() *HostTools {
		h := NewHostToolsUnrestricted(t.TempDir())
		h.base.ScratchDir = scratch
		h.SetScratchSession("ask-01CONVERSATION")
		return h
	}
	// A suite is rebuilt for every tool call; the conversation's scratch must be the same one each time, or a
	// `podman build` could never be followed by a `podman run`.
	if xdgOf(mk().bashEnviron()) != xdgOf(mk().bashEnviron()) {
		t.Error("an Ask conversation got a different scratch on its second call")
	}
}

// THE WIRING THAT MAKES IT STICK: a fix that exists but is not called is not a fix.
func TestScratchLifecycleIsWiredAtEveryCallSite(t *testing.T) {
	for file, want := range map[string]string{
		"bridge.go":                      "host.CleanupScratch()",
		"../askorchicon/native_tools.go": "SetScratchSession(",
		"../server/server.go":            "worktree.SweepSessions(",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s no longer calls %s — the scratch lifecycle has a hole", file, want)
		}
	}
}
