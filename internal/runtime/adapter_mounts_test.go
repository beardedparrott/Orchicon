package runtime

// A RUNTIME CONTAINER MUST BE ABLE TO RUN EVERY ADAPTER IT IS HANDED.
//
// The operator: "a container should definitely be able to run orchicon, opencode, claude, and eventually
// antigravity."
//
// Measured against orchicon-runtime:orchicon-dev with the PRE-FIX mount set, three of the four could not
// run at all:
//
//	orchicon  not on PATH                      (never baked; mounted at create — see the handshake test)
//	opencode  WORKED
//	claude    "not found"                      (dangling symlink: the link was mounted, its target was not)
//	agy       "not found"                      (~/.local/bin/agy declared, but the install is /usr/bin/agy)
//
// These tests pin the mount set, because that is what decides reachability.

import (
	"os"
	"strings"
	"testing"
)

// mountSources extracts the SOURCE side of every -v arg.
//
// "-v <src>:<dst>[:ro|:rw]" — the source is the segment before the FIRST colon. Splitting on the LAST
// colon yields "<src>:<dst>", and on a host with no colon in its paths the two differ only by accident.
func mountSources(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] != "-v" || i+1 >= len(args) {
			continue
		}
		spec := args[i+1]
		src, _, ok := strings.Cut(spec, ":")
		if !ok {
			continue
		}
		out = append(out, src)
	}
	return out
}

func hasMount(sources []string, want string) bool {
	for _, s := range sources {
		if s == want {
			return true
		}
	}
	return false
}

// A SYMLINKED ADAPTER NEEDS ITS TARGET MOUNTED, or the container gets a link into nothing.
//
// THE TARGET IS DELIBERATELY OUTSIDE THE DECLARED INSTALLS. claude's install table happens to declare
// ~/.local/share/claude as a directory, which covers its own symlink incidentally — so a fixture built
// on claude's layout PASSES even with the target-mount removed, and proves nothing. The regression this
// guards is any future adapter whose link points somewhere undeclared, so the fixture must be that shape.
func TestASymlinkedAdapterBinaryMountsItsTarget(t *testing.T) {
	home := t.TempDir()
	// bin/claude -> an UNDECLARED directory outside the install table.
	verDir := home + "/.adapter-store/versions/2.1.319"
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := home + "/.local/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verDir+"/claude", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(verDir+"/claude", binDir+"/claude"); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}

	sources := mountSources(adapterHostMounts(home, "claude"))
	if !hasMount(sources, binDir+"/claude") {
		t.Errorf("the link itself is not mounted; mounts: %v", sources)
	}
	// The invariant that was broken: the TARGET must be reachable in the container.
	found := false
	for _, s := range sources {
		if strings.HasPrefix(s, home+"/.adapter-store") {
			found = true
		}
	}
	if !found {
		t.Errorf("the symlink's target is not mounted, so the container receives a DANGLING link and the "+
			"adapter fails with \"not found\" exactly as it did in production. mounts: %v", sources)
	}
}

// A system install must be mounted AS THE BINARY, never as the whole system directory.
func TestASystemAdapterInstallMountsTheBinaryNotTheDirectory(t *testing.T) {
	sources := mountSources(adapterHostMounts("/nonexistent-home", "antigravity"))
	// With no home, only the declared /usr/bin/agy can be mounted — and only if it exists on this host.
	if _, err := os.Stat("/usr/bin/agy"); err != nil {
		t.Skip("/usr/bin/agy is not present on this host")
	}
	if !hasMount(sources, "/usr/bin/agy") {
		t.Errorf("the system agy install is not mounted, so the antigravity adapter cannot run in a "+
			"container. mounts: %v", sources)
	}
	if hasMount(sources, "/usr/bin") {
		t.Error("the whole /usr/bin was mounted into the container. A system install is declared " +
			"explicitly so the BINARY is mounted; sweeping the directory puts hundreds of unrelated tools " +
			"inside a worker's container.")
	}
}

// The parent-directory fallback exists for $HOME installs; outside it, it must not fire.
func TestTheParentDirectoryMountNeverEscapesHome(t *testing.T) {
	home := t.TempDir()
	// Put an opencode-shaped binary under home so the resolver has something to resolve.
	ocDir := home + "/.opencode/bin"
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ocDir+"/opencode", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := mountSources(adapterHostMounts(home, "opencode"))

	for _, s := range sources {
		if !pathWithin(s, home) {
			t.Errorf("mount %q lies outside home. The parent-directory fallback would otherwise mount a "+
				"whole SYSTEM directory (e.g. /usr/bin) for any adapter binary resolved outside $HOME.", s)
		}
	}
}

// pathWithin is the guard the test above relies on; pin it directly.
func TestPathWithin(t *testing.T) {
	cases := []struct {
		p, home string
		want    bool
	}{
		{"/home/u/.local/bin/agy", "/home/u", true},
		{"/home/u", "/home/u", true},
		{"/usr/bin", "/home/u", false},
		{"/usr/bin/agy", "/home/u", false},
		{"/home/user2/x", "/home/user", false}, // a sibling whose name shares a prefix
		{"/home/u/../etc", "/home/u", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.p, c.home); got != c.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", c.p, c.home, got, c.want)
		}
	}
}
