package workerrestrict

// pathcase_test.go — the project boundary must use the PLATFORM'S comparison rule.
//
// REPORTED shape: a claude worker on Windows had every write refused as a permission denial. One cause is the
// unquoted hook command (internal/claude/hook_command_test.go); this is the other, and it is silent here:
// Windows paths are case-insensitive, so a drive-letter case difference between the recorded project_dir and
// the tool's own path makes a byte-wise prefix test decide the target is OUTSIDE the project.

import (
	"runtime"
	"testing"
)

// The comparison helpers must fold on Windows and NOT fold elsewhere — folding on POSIX would widen the
// boundary rather than fix it.
func TestPathComparisonFollowsThePlatform(t *testing.T) {
	sameDifferent := pathEqual(`C:\projects\weatherapp`, `c:\projects\weatherapp`)
	prefixDifferent := pathHasPrefix(`c:\projects\weatherapp\src`, `C:\projects\weatherapp`)

	if runtime.GOOS == "windows" {
		if !sameDifferent {
			t.Error("pathEqual folded nothing on Windows — a drive-letter case difference is the same path there")
		}
		if !prefixDifferent {
			t.Error("pathHasPrefix folded nothing on Windows — an in-project write would be refused as 'outside " +
				"the project' whenever the drive-letter case differs")
		}
		return
	}
	// POSIX: these are DIFFERENT paths, and matching them would let a worker write outside its project.
	if sameDifferent {
		t.Error("pathEqual folded on a case-sensitive platform — /project and /Project would be conflated")
	}
	if prefixDifferent {
		t.Error("pathHasPrefix folded on a case-sensitive platform — /project would match /Project, outside the " +
			"worker's scope")
	}
}

// The boundary itself still holds: a genuinely foreign path is refused on every platform.
func TestOutsideProjectStillRefusesAForeignPath(t *testing.T) {
	projectDir := t.TempDir()
	allowed, rule := OutsideProject("/definitely/not/the/project/file.txt", projectDir)
	if allowed {
		t.Fatalf("a path outside the project was allowed (%q)", rule)
	}
}

// And an in-project path is allowed.
func TestOutsideProjectAllowsAChildOfTheProject(t *testing.T) {
	projectDir := t.TempDir()
	allowed, rule := OutsideProject(projectDir+"/src/file.txt", projectDir)
	if !allowed {
		t.Fatalf("an in-project path was refused: %q", rule)
	}
}
