package workerrestrict

// winpath.go — WINDOWS PATH SEMANTICS AS A PURE FUNCTION, SO THEY CAN BE TESTED OFF WINDOWS.
//
// WHY THIS EXISTS. `OutsideProject` classifies a tool's path target inside/outside the project
// boundary, and on a real Windows host `filepath.*` gives it the right answer automatically. The
// problem is that a unit test running on the Linux CI box cannot exercise that: `filepath.IsAbs("C:\\x")`
// is FALSE on linux, `filepath.Separator` is `/`, and `pathEqual` compares byte-for-byte. So the one
// platform the boundary most needs proving on is the one the test suite cannot reach.
//
// THE FIX IS TO NOT DEPEND ON THE COMPILED PLATFORM. This file has NO build tag and implements the
// Windows rules directly: a drive letter, `\` and `/` as separators, and CASE-INSENSITIVE comparison
// for both the volume and the path. `OutsideProject` delegates here whenever the BOUNDARY (the project
// dir) is spelled like a Windows path, so a Windows-style boundary classifies correctly from any host,
// and on a real Windows host the answer is the same as the filepath path would give.
//
// THE RULES MIRROR AGY'S OWN DOCUMENTED NORMALISATION (Antigravity CLI permissions, "Cross-platform
// command and path matching"): strip the drive letter and convert `\` to `/`. `NormalizeAgyPath` is
// exactly that, and it is exported so the adapter can show an operator the form agy matches against.
// Comparison, however, does NOT simply strip the drive: see winKey below.

import (
	"path"
	"strings"
)

// looksWindows reports whether a path is spelled in Windows form: a leading drive letter
// (`C:\...` or `C:/...`) or a UNC share (`\\server\share`). This is the switch that makes the
// Windows rules reachable from a Linux test, and it is deliberately strict — a POSIX boundary such
// as `/home/dev/repo` must never take the Windows branch.
func looksWindows(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' {
		if len(p) == 2 {
			return true
		}
		return p[2] == '\\' || p[2] == '/'
	}
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// isDriveLetter reports whether b is an ASCII letter usable as a volume name.
func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// NormalizeAgyPath returns the form agy matches permission rules against: the drive letter is
// STRIPPED and every `\` becomes `/`. agy does this so one rule written on one OS matches the same
// file on another.
//
// It is exported for display/reporting. It is NOT what the boundary comparison uses — stripping the
// drive would make `C:\repo` and `D:\repo` compare equal, so `winKey` keeps it (case-folded).
func NormalizeAgyPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if driveLen := drivePrefixLen(p); driveLen > 0 {
		p = p[driveLen:]
	}
	return cleanSlashPath(p)
}

// cleanSlashPath normalises a slash-separated path with `..`/`.` folded away and no trailing slash,
// keeping a leading `/` so an absolute path stays distinguishable from a relative one.
func cleanSlashPath(p string) string {
	if p == "" {
		return ""
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// drivePrefixLen returns 2 when p starts with `X:` (a volume), 0 otherwise.
func drivePrefixLen(p string) int {
	if len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' {
		return 2
	}
	return 0
}

// winKey reduces a Windows path to a canonical comparison key: the volume (lower-cased, if there is
// one) followed by the `\`→`/` normalised path, all lower-cased.
//
// THE VOLUME IS KEPT, NOT STRIPPED. agy strips it for rule matching, but for the BOUNDARY a stripped
// drive would make `D:\other\x` look like it is inside `C:\repo` whenever the tails happened to line
// up, and that is a boundary that silently allows an external write. Two different drives are two
// different volumes; a target with no drive at all (a POSIX-shaped carve-out such as /tmp/orchicon)
// simply never matches a drive-qualified root.
func winKey(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	drive := ""
	if n := drivePrefixLen(p); n > 0 {
		drive = strings.ToLower(p[:1]) + ":"
		p = p[n:]
	}
	return drive + strings.ToLower(cleanSlashPath(p))
}

// joinWinPath joins a RELATIVE target onto the project boundary, the way the POSIX branch joins with
// filepath.Join. An absolute target (drive-qualified or rooted at `/`) is returned unchanged.
func joinWinPath(projectDir, target string) string {
	t := strings.ReplaceAll(strings.TrimSpace(target), `\`, "/")
	if drivePrefixLen(t) > 0 || strings.HasPrefix(t, "/") {
		return target
	}
	return strings.TrimSuffix(strings.ReplaceAll(projectDir, `\`, "/"), "/") + "/" + t
}

// isInsideWin reports whether the already-keyed child is the root itself or lies beneath it,
// separator-bounded so `/repo2` is not inside `/repo`.
func isInsideWin(childKey, rootKey string) bool {
	if rootKey == "" {
		return false
	}
	if childKey == rootKey {
		return true
	}
	return strings.HasPrefix(childKey, rootKey+"/")
}

// InsideProjectWin reports whether target lies inside projectDir under WINDOWS rules, regardless of
// the host the binary was compiled for: a case-insensitive volume and path, `\` and `/` treated
// alike, and a relative target resolved against projectDir.
//
// It is the pure seam `OutsideProject` delegates to for a Windows-style boundary, and it is directly
// testable on Linux — which is the whole point (AC4).
func InsideProjectWin(target, projectDir string) bool {
	root := strings.TrimSpace(projectDir)
	if root == "" {
		return false
	}
	t := strings.TrimSpace(target)
	if t == "" {
		return false
	}
	if !isAbsWin(t) {
		t = joinWinPath(root, t)
	}
	return isInsideWin(winKey(t), winKey(root))
}

// isAbsWin reports whether a Windows path is rooted: drive-qualified (`C:...`), or starts with a
// separator (a rooted-on-current-volume path, or a UNC share once `\`→`/` has happened).
func isAbsWin(p string) bool {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if drivePrefixLen(p) > 0 {
		return len(p) >= 3 && p[2] == '/'
	}
	return strings.HasPrefix(p, "/")
}

// hasRunDirSegmentWin is hasRunDirSegment with Windows separators, so `.orchicon` run metadata stays
// addressable under a Windows boundary.
func hasRunDirSegmentWin(p string) bool {
	segs := strings.Split(NormalizeAgyPath(p), "/")
	for i, s := range segs {
		if s == RunDirSegment && i < len(segs)-1 {
			return true
		}
	}
	return false
}

// outsideProjectWindows is the Windows-boundary arm of OutsideProject. It mirrors the POSIX arm's
// order exactly — boundary first, then the scratch carve-out, then the `.orchicon` run-metadata
// segment — so a Windows worker gets the same carve-outs a POSIX one does.
func outsideProjectWindows(target, projectDir string) (allowed bool, rule string) {
	if InsideProjectWin(target, projectDir) {
		return true, ""
	}
	if InsideProjectWin(target, ScratchDir) {
		return true, ""
	}
	if hasRunDirSegmentWin(target) {
		return true, ""
	}
	return false, "*"
}
