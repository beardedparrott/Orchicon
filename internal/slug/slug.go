// Package slug provides the shared kebab-case slugifier used for
// project slugs and deterministic git branch names (WorktreeReconciler).
package slug

import (
	"regexp"
	"strings"
)

// nonAlnum matches any run that is not a lowercase letter or digit.
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a name into a lowercase kebab-case slug: lowercase,
// runs of non-alphanumeric characters collapsed to single hyphens,
// leading/trailing hyphens trimmed. Returns "" when the input contains
// no alphanumeric characters. The result is always a valid git ref name
// component (never empty, "." or "..", no "/", no forbidden bytes).
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
