//go:build windows

package workerrestrict

import "strings"

// pathHasPrefix reports whether child is root or lies beneath it, using the platform's OWN comparison rule.
//
// WINDOWS PATHS ARE CASE-INSENSITIVE, and comparing them byte-wise is a silent denial rather than a
// permissiveness bug: the operator's project_dir is recorded as one spelling (`C:\projects\weatherapp`) while
// the tool input can arrive with another (`c:\projects\...` — the drive-letter case is not stable across the
// APIs that produce these strings). A case-sensitive prefix test then decides the target is "outside the
// project" and the worker's write is refused, which the model reports as a permission denial for a path that
// is plainly inside its worktree.
//
// Folding on Windows only. On POSIX these are DIFFERENT paths, and folding there would WIDEN the boundary —
// `/project` would match `/Project`, which is a different directory that the worker has no business writing.
func pathHasPrefix(child, root string) bool {
	return strings.HasPrefix(strings.ToLower(child), strings.ToLower(root))
}

// pathEqual reports whether two paths name the same location on this platform.
func pathEqual(a, b string) bool { return strings.EqualFold(a, b) }
