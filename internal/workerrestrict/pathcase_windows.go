//go:build windows

package workerrestrict

import "strings"

// pathHasPrefix reports whether child is root or lies beneath it, using the platform's OWN
// comparison rule.
func pathHasPrefix(child, root string) bool {
	return strings.HasPrefix(strings.ToLower(child), strings.ToLower(root))
}

// pathEqual reports whether two paths name the same location on this platform.
func pathEqual(a, b string) bool { return strings.EqualFold(a, b) }
