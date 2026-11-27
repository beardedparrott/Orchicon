package workerrestrict

// winpath_test.go — AC4: Windows-style permission paths classify inside/outside the project
// correctly, and they do so from a NON-Windows host.
//
// WHY THESE CASES AND NOT A `GOOS=windows` BUILD: cross-vet proves the package COMPILES for Windows,
// but a compile cannot prove a boundary. The bug this guards against is real and asymmetric — on a
// Linux build `filepath.IsAbs("C:\\repo\\src\\a.go")` is false, so a naive implementation resolves the
// target RELATIVE and puts an external write inside the project. These cases fail loudly if that
// regresses, on any host.

import (
	"path/filepath"
	"testing"
)

// TestNormalizeAgyPath pins the agy-documented normalisation: strip the drive letter, `\`→`/`.
// agy matches permission rules against this form, so an operator reading a rule and Orchicon
// reasoning about the same file must agree on it.
func TestNormalizeAgyPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Users\me b\x`, `/Users/me b/x`},
		{`c:\repo\src\a.go`, `/repo/src/a.go`},
		{`C:/repo/src/a.go`, `/repo/src/a.go`},
		{`\\server\share\x`, `/server/share/x`},
		{`/home/me/x`, `/home/me/x`},
		{`/tmp/orchicon`, `/tmp/orchicon`},
		{``, ``},
	}
	for _, c := range cases {
		if got := NormalizeAgyPath(c.in); got != c.want {
			t.Errorf("NormalizeAgyPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLooksWindowsKeepsPOSIXOutOfTheWindowsArm is the guard on the delegation switch itself: a POSIX
// boundary must NOT take the Windows branch, or every existing worker's boundary changes meaning.
func TestLooksWindowsKeepsPOSIXOutOfTheWindowsArm(t *testing.T) {
	for _, p := range []string{`/home/me/repo`, `/tmp/orchicon`, ``, `relative/dir`} {
		if looksWindows(p) {
			t.Errorf("looksWindows(%q) = true, want false — a POSIX boundary must keep the filepath comparison", p)
		}
	}
	for _, p := range []string{`C:\repo`, `c:/repo`, `D:\`, `\\server\share`, `C:`} {
		if !looksWindows(p) {
			t.Errorf("looksWindows(%q) = false, want true", p)
		}
	}
}

// TestInsideProjectWin is AC4's table: the same file spelled with a drive letter and with forward
// slashes, in mixed case, and the neighbours that must stay OUTSIDE.
func TestInsideProjectWin(t *testing.T) {
	const project = `C:\repo`
	cases := []struct {
		target string
		want   bool
		why    string
	}{
		// --- inside -------------------------------------------------------------------------
		{`C:\repo\src\a.go`, true, "the plain Windows spelling of a file in the project"},
		{`c:/repo/src/a.go`, true, "forward slashes and a lower-case drive are the same file"},
		{`C:\REPO\src`, true, "the volume and every component compare case-insensitively"},
		{`C:/Repo/Src/A.GO`, true, "mixed case, mixed separators"},
		{`C:\repo`, true, "the boundary dir itself is inside itself"},
		{`C:\repo\..\repo\src\a.go`, true, "`..` folds away inside the project"},
		{`src\a.go`, true, "a relative target resolves against the boundary"},
		{`src/a.go`, true, "…with either separator"},
		{`C:\repo\.orchicon\runs\01\x.json`, true, "the .orchicon run-metadata carve-out"},

		// --- outside ------------------------------------------------------------------------
		{`C:\repo2\a.go`, false, "separator-bounded: repo2 is not inside repo"},
		{`C:\repo\..\etc\x`, false, "`..` escapes and stays escaped"},
		{`D:\other\a.go`, false, "a DIFFERENT volume is outside even if the tail matched"},
		{`C:\Windows\system32\x`, false, "a system path"},
		{`\\server\share\x`, false, "a UNC share is not inside a local project"},
		{`/etc/passwd`, false, "a POSIX-shaped absolute path is not inside a drive-qualified root"},
	}
	for _, c := range cases {
		if got := InsideProjectWin(c.target, project); got != c.want {
			t.Errorf("InsideProjectWin(%q, %q) = %v, want %v (%s)", c.target, project, got, c.want, c.why)
		}
	}
}

// TestOutsideProjectWindowsBoundary is AC4 through the REAL entry point: the adapter's permission
// handler calls OutsideProject, not InsideProjectWin, so the delegation has to be proved here too —
// including the carve-outs a Windows worker is entitled to.
func TestOutsideProjectWindowsBoundary(t *testing.T) {
	const project = `C:\repo`
	cases := []struct {
		target  string
		allowed bool
		why     string
	}{
		{`C:\repo\src\a.go`, true, "a write inside the project"},
		{`c:/repo/src/a.go`, true, "the same write spelled the other way"},
		{`C:\repo\.orchicon\runs\x.json`, true, "the .orchicon run-metadata carve-out"},
		{`/tmp/orchicon/scratch.txt`, true, "the /tmp/orchicon scratch carve-out"},
		{`C:\Windows\system32\x`, false, "a system path"},
		{`D:\other\a.go`, false, "another volume"},
		{`C:\repo2\a.go`, false, "a sibling directory sharing the prefix"},
	}
	for _, c := range cases {
		allowed, rule := OutsideProject(c.target, project)
		if allowed != c.allowed {
			t.Errorf("OutsideProject(%q, %q) = %v (%q), want %v (%s)", c.target, project, allowed, rule, c.allowed, c.why)
			continue
		}
		if !allowed && rule != "*" {
			t.Errorf("OutsideProject(%q) refused with rule %q, want the external_directory catch-all \"*\"", c.target, rule)
		}
	}
	// An empty target is not a boundary question (the caller's tool names no path), and it must read
	// the same under a Windows boundary as under a POSIX one.
	if allowed, _ := OutsideProject("", project); !allowed {
		t.Error("OutsideProject(\"\", windows boundary) = false, want true — an empty target names no path")
	}
}

// TestOutsideProjectPOSIXUnchanged is the regression half of AC6 for this package: the POSIX boundary
// must behave EXACTLY as it did before winpath.go existed.
func TestOutsideProjectPOSIXUnchanged(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("the filepath comparison rules only apply to a POSIX build; the Windows arm is covered above")
	}
	project := t.TempDir()
	cases := []struct {
		target  string
		allowed bool
	}{
		{filepath.Join(project, "src", "a.go"), true},
		{filepath.Join(project, "..", "etc", "x"), false},
		{project + "2/a.go", false},
		{"/tmp/orchicon/scratch.txt", true},
		{filepath.Join(project, ".orchicon", "runs", "x.json"), true},
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		if allowed, _ := OutsideProject(c.target, project); allowed != c.allowed {
			t.Errorf("OutsideProject(%q, %q) = %v, want %v (POSIX behaviour must not change)", c.target, project, allowed, c.allowed)
		}
	}
}
