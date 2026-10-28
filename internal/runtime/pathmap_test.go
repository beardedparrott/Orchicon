package runtime

// pathmap_test.go — THE PATH FORM THE ENGINE'S VM ACTUALLY SEES.
//
// Podman's own docs are the source of truth here, not inference:
//
//	podman-machine(1):            "Podman on MacOS and Windows requires a virtual machine … containers are Linux."
//	podman-machine-init(1):       "Since all drives are mounted at /mnt on startup by default in WSL … The host home
//	                               directory for a C: drive will be mounted at /mnt/c/Users/<my username>."
//
// The plane sends `C:\projects\app` as both the mount source and the container destination. Left unmapped, the
// engine inside the VM is handed a source it cannot resolve — and the container then cannot see the project it
// is supposed to work on. These tests pin the mapping, and pin that nothing ELSE changes.

import "testing"

func TestWSLModeMapsADrivePath(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	cases := map[string]string{
		`C:\projects\app`:              "/mnt/c/projects/app",
		`C:\projects\app\`:             "/mnt/c/projects/app/",
		`c:\projects\app`:              "/mnt/c/projects/app",
		`C:/projects/app`:              "/mnt/c/projects/app",
		`D:\data\repo`:                 "/mnt/d/data/repo",
		`C:\projects\my app`:           "/mnt/c/projects/my app",
		`C:\Users\op\.local\share\gh`:  "/mnt/c/Users/op/.local/share/gh",
		`C:\a\b\.orchicon-worktrees\1`: "/mnt/c/a/b/.orchicon-worktrees/1",
		// A doubled separator in the source must not produce a doubled one in the guest.
		`C:\\projects\\app`: "/mnt/c/projects/app",
	}
	for in, want := range cases {
		if got := m.Map(in); got != want {
			t.Errorf("Map(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSpacesNeedNoQuotingBecauseTheSpecIsOneArgvElement pins the thing that looks like a problem and is not.
//
// mountArgv builds `source:dest:opts` as a SINGLE argv element and the engine parses the colons itself, so a
// space in the path needs no escaping at either end. (This is why the operator's instinct — "handle spaces
// properly" — is already satisfied by the argv shape, and adding quotes would actively break it.)
func TestSpacesNeedNoQuotingBecauseTheSpecIsOneArgvElement(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	spec := (MountSpec{Source: `C:\projects\my app`, Dest: `C:\projects\my app`})
	mapped := mapMounts(m, []MountSpec{spec})
	argv := mountArgv(mapped)
	if len(argv) != 2 {
		t.Fatalf("mountArgv returned %d elements, want 2 (-v, spec)", len(argv))
	}
	if argv[0] != "-v" {
		t.Errorf("first element = %q, want -v", argv[0])
	}
	want := "/mnt/c/projects/my app:/mnt/c/projects/my app"
	if argv[1] != want {
		t.Errorf("spec = %q, want %q (no quoting: the engine splits the colons, not the shell)", argv[1], want)
	}
}

// TestBothSidesOfEveryMountAreMapped is the correction that matters: Source and Dest happen to hold the same
// string today, and BOTH have to move — the source because the VM must resolve it, the destination because the
// in-container serve and every bash/file tool resolve the project through it.
func TestBothSidesOfEveryMountAreMapped(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	in := []MountSpec{
		{Source: `C:\projects\app`, Dest: `C:\projects\app`},
		{Source: `C:\Users\op\.gitconfig`, Dest: `C:\Users\op\.gitconfig`, RO: true},
	}
	out := mapMounts(m, in)
	for i, ms := range out {
		if ms.Source != "/mnt/c/"+trimDrive(in[i].Source) {
			t.Errorf("mount %d source = %q, not mapped", i, ms.Source)
		}
		if ms.Dest != "/mnt/c/"+trimDrive(in[i].Dest) {
			t.Errorf("mount %d dest = %q, not mapped", i, ms.Dest)
		}
		if ms.RO != in[i].RO {
			t.Errorf("mount %d RO flag changed", i)
		}
	}
	// The INPUT is not mutated: the request may be retried or logged afterwards.
	if in[0].Source != `C:\projects\app` {
		t.Error("mapMounts mutated the caller's request")
	}
}

func trimDrive(p string) string {
	rest := p[3:] // C:\
	out := []rune{}
	for _, r := range rest {
		if r == '\\' {
			out = append(out, '/')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// TestIdentityOnLinux is the regression guard: nothing about this change may alter a Linux daemon's argv.
func TestIdentityOnLinux(t *testing.T) {
	m := NewPathMapper("linux", "")
	in := []MountSpec{{Source: "/home/op/projects/app", Dest: "/home/op/projects/app"}}
	out := mapMounts(m, in)
	if out[0] != in[0] {
		t.Errorf("linux mount was rewritten: %+v -> %+v", in[0], out[0])
	}
	if got := m.Map("/home/op/x"); got != "/home/op/x" {
		t.Errorf("linux path rewritten: %q", got)
	}
	if got := mapMountArgv(m, []string{"-v", "/a:/b:ro"}); got[1] != "/a:/b:ro" {
		t.Errorf("linux -v argv rewritten: %v", got)
	}
}

// TestMacOSIsIdentity pins that podman machine on macOS shares $HOME at IDENTICAL guest paths, so no mapping
// applies — the same architecture, a different filesystem contract.
func TestMacOSIsIdentity(t *testing.T) {
	m := NewPathMapper("darwin", "applehv")
	if m.Mode() != PathModeIdentity {
		t.Errorf("darwin mode = %q, want identity (podman machine shares $HOME unchanged)", m.Mode())
	}
	if got := m.Map("/Users/op/projects/app"); got != "/Users/op/projects/app" {
		t.Errorf("darwin path rewritten: %q", got)
	}
}

// TestUnmappablePathsAreLeftAlone states the refusal explicitly, because an invented /mnt path would be worse
// than the engine's own error naming the real one.
func TestUnmappablePathsAreLeftAlone(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	for _, p := range []string{
		`\\server\share\project`, // UNC: not on a drive WSL mounts
		`C:relative`,             // drive-relative: no /mnt equivalent
		`/already/guest/path`,    // already in guest form (idempotent)
		`relative/path`,          // not absolute
		``,                       // empty
	} {
		if got := m.Map(p); got != p {
			t.Errorf("Map(%q) = %q, want it unchanged (unmappable must not be invented)", p, got)
		}
	}
}

// TestHyperVKeepsWindowsForm pins the other Windows provider, so the difference is deliberate rather than a
// forgotten branch: a Hyper-V machine resolves volumes on the SERVER side, so there is no /mnt share.
func TestHyperVKeepsWindowsForm(t *testing.T) {
	m := NewPathMapper("windows", "hyperv")
	if m.Mode() != PathModeWindowsShare {
		t.Fatalf("hyperv mode = %q, want windows-share", m.Mode())
	}
	if got := m.Map(`C:\projects\app`); got != `C:\projects\app` {
		t.Errorf("hyperv path was mapped: %q", got)
	}
}

// TestUnknownWindowsProviderFallsBackToWSL is the safety default: podman's own default provider on Windows IS
// wsl, so an unspecified provider must map rather than pass a form that certainly will not resolve.
func TestUnknownWindowsProviderFallsBackToWSL(t *testing.T) {
	for _, provider := range []string{"", "wsl", "something-new"} {
		m := NewPathMapper("windows", provider)
		if m.Mode() != PathModeWSL {
			t.Errorf("provider %q gave mode %q, want wsl", provider, m.Mode())
		}
	}
}

// TestMapMountArgvEditsOnlyTheSource is the argv-boundary guarantee: adapter/git mount blocks are fed through
// whole, and only the host side of each -v may change.
func TestMapMountArgvEditsOnlyTheSource(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	in := []string{
		"-v", `C:\Users\op\.opencode:/home/op/.opencode:ro`,
		"--tmpfs", "/home/op/.private:rw,size=2g",
		"-e", "SOME_KEY=value",
		"-v", `C:\Users\op\.gitconfig:C:\Users\op\.gitconfig:ro`,
	}
	out := mapMountArgv(m, in)
	if out[0] != "-v" || out[2] != "--tmpfs" || out[4] != "-e" {
		t.Fatalf("non-mount flags were edited: %v", out)
	}
	if out[3] != "/home/op/.private:rw,size=2g" {
		t.Errorf("a tmpfs value was edited: %q", out[3])
	}
	if out[5] != "SOME_KEY=value" {
		t.Errorf("an env value was edited: %q", out[5])
	}
	if want := "/mnt/c/Users/op/.opencode:/home/op/.opencode:ro"; out[1] != want {
		t.Errorf("source not mapped: %q, want %q", out[1], want)
	}
	if want := "/mnt/c/Users/op/.gitconfig:/mnt/c/Users/op/.gitconfig:ro"; out[7] != want {
		t.Errorf("both-sides mapping failed: %q, want %q", out[7], want)
	}
}

// TestMountSpecSplitHandlesTheDriveLetterColon is the subtle one: a Windows source carries a colon of its own, so
// a naive strings.Split(spec, ":") or LastIndex split mangles exactly the case this exists for.
func TestMountSpecSplitHandlesTheDriveLetterColon(t *testing.T) {
	cases := []struct{ spec, wantSrc, wantRest string }{
		{`C:\projects\app:/mnt/c/projects/app:ro`, `C:\projects\app`, "/mnt/c/projects/app:ro"},
		{`C:/projects/app:/mnt/c/projects/app`, `C:/projects/app`, "/mnt/c/projects/app"},
		{`/host/path:/dest:ro`, "/host/path", "/dest:ro"},
		{`C:\only`, "", ""},
		{"/only", "", ""},
	}
	for _, c := range cases {
		src, rest := splitMountSpec(c.spec)
		if src != c.wantSrc || rest != c.wantRest {
			t.Errorf("splitMountSpec(%q) = (%q,%q), want (%q,%q)", c.spec, src, rest, c.wantSrc, c.wantRest)
		}
	}
}

// TestMappingIsIdempotent matters because a declaration may already carry a guest path (a value that came FROM
// the container), and double-mapping would produce `/mnt/c/mnt/c/...`.
func TestMappingIsIdempotent(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	once := m.Map(`C:\projects\app`)
	twice := m.Map(once)
	if twice != once {
		t.Errorf("mapping twice changed the path: %q -> %q", once, twice)
	}
}
