package opencode

// binary_platform_test.go — the Windows probe table, tested from ANY host.

import (
	"path/filepath"
	"strings"
	"testing"
)

func winEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestWindowsCandidatesHaveAnExtensionAndCoverThePerUserInstalls(t *testing.T) {
	env := winEnv(map[string]string{
		"USERPROFILE":       `C:\Users\operator`,
		"LOCALAPPDATA":      `C:\Users\operator\AppData\Local`,
		"APPDATA":           `C:\Users\operator\AppData\Roaming`,
		"ProgramFiles":      `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`,
	})
	got := openCodePlatformCandidates("windows", env)
	if len(got) == 0 {
		t.Fatal("no Windows candidates at all — a Windows install can never be resolved, which is the report")
	}

	// EVERY CANDIDATE MUST NAME A REAL EXECUTABLE. This is the whole bug: a path without .exe cannot exist on
	// Windows, so probing it is probing nothing.
	for _, c := range got {
		if !strings.EqualFold(filepath.Ext(c), ".exe") {
			t.Errorf("candidate %q has no .exe — os.Stat will never find it on Windows", c)
		}
	}

	// The per-user locations, which is what a desktop machine has: no elevation needed.
	for _, want := range []string{
		filepath.Join(`C:\Users\operator`, ".opencode", "bin", "opencode.exe"),
		filepath.Join(`C:\Users\operator\AppData\Local`, "Programs", "opencode", "opencode.exe"),
		filepath.Join(`C:\Users\operator`, "scoop", "shims", "opencode.exe"),
		filepath.Join(`C:\Users\operator\AppData\Local`, "Microsoft", "WinGet", "Links", "opencode.exe"),
	} {
		found := false
		for _, c := range got {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing candidate %q — this install would be reported as 'not on PATH'%s, got %v",
				want, "", got)
		}
	}
}

func TestWindowsCandidatesAreSilentWhenTheEnvironmentIsEmpty(t *testing.T) {
	// A service-shaped environment with no USERPROFILE must not produce "\\opencode.exe" — a relative path
	// that would be resolved against the process's own directory.
	for _, c := range openCodePlatformCandidates("windows", winEnv(map[string]string{})) {
		if c == "" || !filepath.IsAbs(c) {
			t.Errorf("candidate %q is not absolute; probing it would search the current directory", c)
		}
	}
}

func TestUnixHasNoExtraPlatformCandidates(t *testing.T) {
	// On Unix the $HOME list plus PATH IS the whole story, and the system prefixes are diagnostic only — a
	// resolution there would hand the plane a binary its runtime containers cannot execute.
	if got := openCodePlatformCandidates("linux", winEnv(map[string]string{"HOME": "/home/u"})); len(got) != 0 {
		t.Errorf("linux produced platform candidates %v; the system prefixes must stay diagnostic", got)
	}
	if got := openCodePlatformCandidates("darwin", winEnv(map[string]string{"HOME": "/Users/u"})); len(got) != 0 {
		t.Errorf("darwin produced platform candidates %v", got)
	}
}

func TestTheShimIsReportedRatherThanResolved(t *testing.T) {
	// THE TRAP THIS AVOIDS.
	env := winEnv(map[string]string{"APPDATA": `C:\Users\operator\AppData\Roaming`})
	shims := openCodeShimCandidates("windows", env)
	if len(shims) == 0 {
		t.Fatal("no shim candidates — the 'but I installed it' hint could never fire")
	}
	// The .cmd and .ps1 shims npm writes, and the extensionless one Git Bash uses.
	joined := strings.Join(shims, "\n")
	for _, want := range []string{"opencode.cmd", "opencode.bat", "opencode.ps1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shim candidates are missing %s: %v", want, shims)
		}
	}
	// And a resolved path must never be one of them.
	for _, c := range openCodePlatformCandidates("windows", env) {
		if strings.Contains(strings.ToLower(c), ".cmd") || strings.Contains(strings.ToLower(c), ".bat") {
			t.Errorf("a shim (%q) is in the RESOLUTION list; it must only ever be reported", c)
		}
	}
}
