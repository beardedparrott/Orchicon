package contextfiles

import (
	"sort"
	"strings"
)

// Union merges two path lists into one SORTED, DEDUPED list.
func Union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	add := func(in []string) {
		for _, p := range in {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	add(a)
	add(b)
	sort.Strings(out)
	return out
}

// sortedByResolved returns paths ordered by their RESOLVED absolute form (contextfiles.Resolve),
// preserving each caller-supplied entry verbatim so error/degrade notes keep naming the path.
func sortedByResolved(paths []string, projectDir string) []string {
	if len(paths) < 2 {
		return paths
	}
	out := make([]string, len(paths))
	copy(out, paths)
	sort.SliceStable(out, func(i, j int) bool {
		return Resolve(out[i], projectDir) < Resolve(out[j], projectDir)
	})
	return out
}
