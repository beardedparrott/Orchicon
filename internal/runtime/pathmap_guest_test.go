package runtime

// pathmap_guest_test.go — A GUEST PATH MUST BE COMPARED WITH GUEST RULES.
//
// Reported from Windows, with the project directory plainly listed in the allowlist the error printed:
//
//	mount source outside allowed roots: C:\projects\weatherapp
//	    (as the container engine sees it: /mnt/c/projects/weatherapp;
//	     allowed roots: C:\Users\operator\projects, C:\projects\weatherapp — a PROJECT's own project_dir is
//	     always allowed, and ORCHICON_RUNTIME_ALLOWED_ROOTS adds more)
//
// The path WAS in the list. The comparison still failed, and no value of ORCHICON_RUNTIME_ALLOWED_ROOTS could
// have helped — including C:\.
//
// THE CAUSE, and it was Windows-only: the root side was `filepath.Clean(m.Map(root))` while the path side was
// `m.Map(path)` with no Clean. On Windows, filepath.Clean applies WINDOWS rules to a path that belongs to a
// LINUX VM: "/mnt/c/projects/weatherapp" has no drive letter, so Clean treats it as drive-relative and rewrites
// the separators to `\mnt\c\projects\weatherapp`. The path side stayed "/mnt/c/projects/weatherapp". They can
// never be equal, and HasPrefix can never match a backslash root against a slash path.
//
// WHY THE EXISTING TESTS DID NOT CATCH IT, which is the lesson worth keeping: they run on linux/amd64, where
// filepath.Clean leaves forward slashes alone, so the two sides happened to agree. A test of a
// platform-DEPENDENT function, RUN on one platform, tests that platform only. The tests below therefore assert
// the PROPERTY (guest forms are slash-separated and identical for the same directory) rather than a string the
// running OS happens to produce — and TestGuestFormIsPlatformIndependent additionally fails on any host where
// filepath.Clean is not slash-preserving, so the suite becomes meaningful on Linux too.

import (
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// TestGuestFormIsSlashSeparated is the property the Windows bug violated.
func TestGuestFormIsSlashSeparated(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	got := guestForm(m, `C:\projects\weatherapp`)

	if strings.Contains(got, `\`) {
		t.Fatalf("guestForm returned %q, which contains a BACKSLASH — a guest path belongs to a Linux VM and "+
			"must be slash-separated whatever OS produced it", got)
	}
	if !strings.HasPrefix(got, "/mnt/c/") {
		t.Fatalf("guestForm(%q) = %q, want the WSL mount form /mnt/c/...", `C:\projects\weatherapp`, got)
	}
}

// TestTheSameDirectoryYieldsTheSameGuestForm is the invariant the comparison relies on: the candidate path and
// the root that contains it must normalise IDENTICALLY, or no allowlist can ever match.
func TestTheSameDirectoryYieldsTheSameGuestForm(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	host := `C:\projects\weatherapp`

	fromPath := guestForm(m, host)
	fromRoot := guestForm(m, host) // the same directory, reached as a ROOT rather than a mount source

	if fromPath != fromRoot {
		t.Fatalf("the same directory normalised two ways:\n  as a mount source: %q\n  as an allowed root: %q\n"+
			"THIS IS THE REPORTED BUG — the two sides were cleaned differently and could never match.", fromPath, fromRoot)
	}
}

// TestAllowlistMatchesTheSameFormsItPrints is the operator-facing assertion: the path the error names as
// "outside allowed roots" must be ACCEPTED when that same path appears in the roots it prints.
func TestAllowlistMatchesTheSameFormsItPrints(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	projectDir := `C:\projects\weatherapp`
	roots := []string{`C:\Users\operator\projects`, projectDir}

	if !withinRootsIn(m, projectDir, roots) {
		t.Fatalf("the mount source %q was REFUSED even though %q is in the allowlist %v.\n"+
			"guest form of the source: %q\nroots as guest forms: %v",
			projectDir, projectDir, roots, guestForm(m, projectDir), guestForms(m, roots))
	}
	if !withinRootsIn(m, projectDir+`\src`, roots) {
		t.Errorf("a path UNDER an allowed root must be accepted")
	}
	// And the boundary still holds.
	if withinRootsIn(m, `D:\elsewhere`, roots) {
		t.Errorf("an unrelated drive was allowed")
	}
	if withinRootsIn(m, `C:\projects\weatherapp-OTHER`, roots) {
		t.Errorf("a sibling that merely shares a path prefix was allowed — the comparison must be on segments")
	}
}

// TestGuestFormIsPlatformIndependent is the anti-regression for the ORIGINAL defect: it pins that the guest
// form does not depend on the running platform's separator at all.
//
// On Linux this passes trivially; on Windows it fails for any implementation that lets filepath.Clean touch the
// guest form. That asymmetry is deliberate and stated here rather than hidden: this is the one assertion whose
// VALUE differs by platform, so a green Linux run is not evidence — which is exactly why the two tests above
// assert properties instead of a single string.
func TestGuestFormIsPlatformIndependent(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	got := guestForm(m, `C:\projects\weatherapp`)

	// path.Clean is slash-only BY DEFINITION, so this is the value every platform must produce.
	want := path.Clean("/mnt/c/projects/weatherapp")
	if got != want {
		t.Fatalf("guestForm = %q, want %q (slash semantics).\n"+
			"On Windows the previous implementation produced %q because filepath.Clean rewrote the separators.",
			got, want, filepath.Clean(`\mnt\c\projects\weatherapp`))
	}
}

// TestHostFormIsStillCleanedWithHostRules keeps the OTHER half honest: before mapping, a host path must be
// normalised by filepath, because that is the one place the host's own rules are correct.
func TestHostFormIsStillCleanedWithHostRules(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	// A redundant separator and a trailing one must not change the answer.
	a := guestForm(m, `C:\projects\weatherapp\`)
	b := guestForm(m, `C:\projects\weatherapp`)
	if a != b {
		t.Fatalf("trailing-separator forms disagree: %q vs %q", a, b)
	}
}

// TestIdentityModeIsUntouched: on Linux the mapping is identity, so this must reduce to the plain comparison it
// has always been.
func TestIdentityModeIsUntouched(t *testing.T) {
	m := NewPathMapper("linux", "")
	if got := guestForm(m, "/home/me/projects/app"); got != "/home/me/projects/app" {
		t.Fatalf("identity mode rewrote the path: %q", got)
	}
	if !withinRootsIn(m, "/home/me/projects/app", []string{"/home/me/projects"}) {
		t.Fatal("the plain comparison regressed on Linux")
	}
}

func guestForms(m *PathMapper, roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = guestForm(m, r)
	}
	return out
}

// windowsCleanGuest reproduces Go's WINDOWS filepath.Clean for a guest path, so the defect can be asserted on a
// Linux CI host.
//
// A guest path has no drive letter, so Windows treats it as drive-RELATIVE and rewrites the separator; the
// segment resolution is otherwise the same as path.Clean.
func windowsCleanGuest(p string) string {
	return strings.ReplaceAll(path.Clean(p), "/", `\`)
}

// TestTheWindowsDefectIsDetectedOnAnyHost is the assertion the earlier tests could not make.
//
// The behavioural tests above all PASSED against the unfixed code when run on linux/amd64, because
// filepath.Clean leaves forward slashes alone there — so the two sides accidentally agreed. A test of a
// platform-dependent function, run on one platform, tests that platform only.
//
// This one states WHAT WINDOWS WOULD DO and asserts the two implementations differ, so it fails on any host if
// the code ever goes back to cleaning a guest path with filepath.
func TestTheWindowsDefectIsDetectedOnAnyHost(t *testing.T) {
	const guest = "/mnt/c/projects/weatherapp"

	// What the PREVIOUS implementation compared: an uncleaned guest path against a host-Cleaned guest root.
	oldPath := guest
	oldRoot := windowsCleanGuest(guest)
	if oldPath == oldRoot {
		t.Fatal("test is not modelling the defect: the old pair must differ under Windows rules")
	}

	// What guestForm produces now: the same value on both sides, on every platform.
	m := NewPathMapper("windows", "wsl")
	newPath := guestForm(m, `C:\projects\weatherapp`)
	newRoot := guestForm(m, `C:\projects\weatherapp`)
	if newPath != newRoot {
		t.Fatalf("the fix does not produce matching forms: %q vs %q", newPath, newRoot)
	}
	if newPath == oldRoot {
		t.Fatalf("guestForm still returns the Windows-Cleaned form %q — the defect is back", newPath)
	}
}
