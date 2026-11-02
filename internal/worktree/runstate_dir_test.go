package worktree

// runstate_dir_test.go — the run-state directory must never be CWD-relative.
//
// MEASURED, on a Windows install: a tree
//
//	<project>\C␺projects␼weatherapp\.orchicon\<runid>\todos.json
//
// where the directory name is the project path carved into ONE component with
// U+F03A for ':' and U+F05C for '\' (rune 0xF000|c). The MCP sidecar derived its
// run-state dir from ORCHICON_MCP_PROJECT_DIR, that value was not absolute, and
// filepath.Join resolved it against the sidecar's working directory — which was
// the project root.
//
// The sanitizer is not in this codebase (checked: no Go source, no Go module, no
// frontend source contains U+F03A), so it arrives as DATA. What is ours is
// accepting it: a relative project dir must be refused, not silently resolved.

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THE REPORTED NAME, byte for byte: C, U+F03A, projects, U+F05C, weatherapp.
func debrisShapedName() string {
	return "C" + string(rune(0xF03A)) + "projects" + string(rune(0xF05C)) + "weatherapp"
}

func TestRunStateDirRefusesARelativeProjectDir(t *testing.T) {
	if got := RunStateDir(debrisShapedName(), "01M6EG9V073TQ5NP13WMR8VEV8"); got != "" {
		t.Fatalf("RunStateDir(%q) = %q, want \"\" — a relative project dir resolves against the process's "+
			"working directory, which is exactly how a .orchicon tree ended up under a directory named after "+
			"a carved-up path", debrisShapedName(), got)
	}
	for _, rel := range []string{"weatherapp", "./weatherapp", "../x"} {
		if got := RunStateDir(rel, "run-1"); got != "" {
			t.Errorf("RunStateDir(%q) = %q, want \"\"", rel, got)
		}
	}
}

// An absolute project dir still works, and the result stays under it.
func TestRunStateDirAcceptsAnAbsoluteProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	got := RunStateDir(projectDir, "run-1")
	if got == "" {
		t.Fatal("RunStateDir refused an absolute project dir")
	}
	if filepath.Dir(filepath.Dir(got)) != projectDir {
		t.Fatalf("RunStateDir = %q, want it under %q", got, projectDir)
	}
	if !strings.HasSuffix(got, filepath.Join(".orchicon", "run-1")) {
		t.Fatalf("RunStateDir = %q, want it to end in .orchicon/run-1", got)
	}
}

// Empty inputs keep their documented behaviour.
func TestRunStateDirEmptyInputs(t *testing.T) {
	if got := RunStateDir("", "run-1"); got != "" {
		t.Errorf("empty project dir yielded %q", got)
	}
	if got := RunStateDir(t.TempDir(), ""); got != "" {
		t.Errorf("empty run id yielded %q", got)
	}
}

// The Windows-shaped absolute path is accepted too — this is a platform rule, not a POSIX one.
func TestRunStateDirAcceptsAWindowsAbsolutePathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.IsAbs answers for the host platform; the Windows case is exercised on Windows CI")
	}
	if got := RunStateDir(`C:\projects\weatherapp`, "run-1"); got == "" {
		t.Fatal("an absolute Windows project dir was refused")
	}
}
