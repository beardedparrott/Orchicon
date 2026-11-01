//go:build !windows

package workerrestrict

import "strings"

// pathHasPrefix reports whether child is root or lies beneath it, using the platform's OWN comparison rule.
//
// POSIX paths are case-SENSITIVE, so this is an exact prefix test. Folding here would WIDEN the boundary:
// `/project` would match `/Project`, a different directory outside the worker's scope. See
// pathcase_windows.go for the Windows rule and why it differs.
func pathHasPrefix(child, root string) bool { return strings.HasPrefix(child, root) }

// pathEqual reports whether two paths name the same location on this platform.
func pathEqual(a, b string) bool { return a == b }
