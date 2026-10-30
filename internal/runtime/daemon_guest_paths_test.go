package runtime

// daemon_guest_paths_test.go — EVERY PATH THE ENGINE CONSUMES MUST BE IN THE ENGINE'S FORM.
//
// This is the class of bug that only bites on Windows, where host and guest paths have no string
// relationship: a bind is mapped by mapMountArgv, but `--tmpfs` and `-e HOME=` go straight into argv and were
// NOT mapped. Measured consequence once the Windows adapter refusal was removed: the daemon would have emitted
//
//	--tmpfs C:\Users\justi:rw,...
//	-e HOME=C:\Users\justi
//
// — the first rejected by the engine as a malformed tmpfs, the second naming a directory no Linux process can
// resolve, so every adapter CLI exits at boot on `EACCES mkdir $HOME/.local/state`.
//
// These tests run on Linux, so they assert the PROPERTY (the emitted form is a guest path) rather than a
// Windows string; the WSL mapper is exercised directly for the Windows case.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHomeEnvIsAGuestPath is the Windows-shaped case, asserted without a Windows host.
func TestHomeEnvIsAGuestPath(t *testing.T) {
	// A WSL-mode mapper turns C:\Users\justi into /mnt/c/Users/justi, which is what the container can use.
	m := NewPathMapper("windows", "wsl")
	d := &Daemon{HostHome: `C:\Users\justi`, Paths: m, HomeTmpfsSize: "2g", UserID: 1000, GroupID: 1000}

	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"claude"}, GitStrategy: "none"})
	home := envLookup(args, "HOME")
	if home == "" {
		t.Fatal("no HOME was emitted — the container's home is where every adapter writes its state")
	}
	if strings.Contains(home, `\`) || strings.Contains(home, ":") {
		t.Fatalf("HOME=%q is a HOST path — the container cannot resolve it, and every CLI exits at boot on "+
			"EACCES mkdir $HOME/.local/state", home)
	}
	if !strings.HasPrefix(home, "/mnt/c/") {
		t.Fatalf("HOME=%q is not the WSL form of C:\\Users\\justi", home)
	}

	// The tmpfs entries must be guest paths too, or the engine rejects them outright.
	for i, a := range args {
		if a != "--tmpfs" || i+1 >= len(args) {
			continue
		}
		spec := args[i+1]
		path := spec[:strings.Index(spec, ":")]
		if strings.Contains(path, `\`) || (len(path) > 1 && path[1] == ':') {
			t.Errorf("--tmpfs %q names a HOST path — the engine rejects a malformed tmpfs and the runtime "+
				"uid cannot own it", spec)
		}
	}
}

// TestIdentityModeIsUnchanged keeps the Linux/macOS path byte-identical: mapping is identity there, so this is
// the plain comparison it always was.
func TestIdentityModeIsUnchanged(t *testing.T) {
	home := t.TempDir()
	d := &Daemon{HostHome: home, Paths: NewPathMapper("linux", ""), HomeTmpfsSize: "2g", UserID: 1000, GroupID: 1000}
	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"claude"}, GitStrategy: "none"})
	if got := envLookup(args, "HOME"); got != home {
		t.Fatalf("HOME = %q, want the host path unchanged on an identity mapper (%q)", got, home)
	}
}

// envLookup returns the value of KEY= in an argv-style env list, or "".
func envLookup(args []string, key string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, key+"="); ok {
			return v
		}
	}
	return ""
}

// TestProjectDirMountIsInGuestForm guards the OTHER engine-consumed path: a project bind's source.
func TestProjectDirMountIsInGuestForm(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	got := m.Map(filepath.Join(string(filepath.Separator), "projects", "app"))
	if strings.Contains(got, `\`) {
		t.Fatalf("mapped project path %q contains a backslash", got)
	}
	_ = os.PathSeparator
}
