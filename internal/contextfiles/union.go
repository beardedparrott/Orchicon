package contextfiles

import (
	"sort"
	"strings"
)

// Union merges two path lists into one SORTED, DEDUPED list.
//
// It exists so the two prompt builders (the worker composite prompt and the Ask
// system prompt) combine a project's selection with a version's/conversation's
// selection through ONE list operation rather than one each. A union is a list
// utility, not a validator and not a renderer, so the renderer stays
// contextfiles.RenderManifest and the validator stays contextfiles.Validate /
// ValidateWithin — the single shared platform path both call sites ride.
//
// It is SORTED (not merely deduped) because the skills/context section lands
// inside the CACHED STATIC PREFIX of the prompt (ADR-0009 D5, AssembleSystem in
// internal/orchicon/prompt.go): any ordering the caller or the filesystem
// happens to produce would change the rendered bytes and invalidate the prefix
// cache on every turn. Sorting here makes the union's order a property of its
// CONTENTS, never of how the lists arrived.
//
// Entries are whitespace-trimmed; blank entries and duplicates are dropped. A
// nil or empty pair returns an empty (non-nil) slice.
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

// sortedByResolved returns paths ordered by their RESOLVED absolute form
// (contextfiles.Resolve), preserving each caller-supplied entry verbatim so
// error/degrade notes keep naming the path the operator chose.
//
// This is the general determinism fix the package's renderers need: the
// resolved form is what the OS call and the rendered section actually key on, so
// sorting by it makes the rendered bytes a function of the SELECTION rather than
// of the caller's slice order. Unresolvable entries (Resolve returns "") sort
// first and render nothing, so they cannot perturb the order of the rest.
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
