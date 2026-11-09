package adapter

import "sort"

// DemandSet is the set of adapter kinds some consumer of this plane could dispatch to — the
// demand-keyed subject of the lazy host opencode serve.
type DemandSet map[string]struct{}

// AdapterDemandSet computes the adapter demand set for a list of model refs.
func AdapterDemandSet(refs ...string) DemandSet {
	out := make(DemandSet, len(refs))
	for _, ref := range refs {
		k := AdapterKind(ref)
		if k == "" {
			k = DefaultAdapterKind
		}
		out[k] = struct{}{}
	}
	return out
}

// Has reports whether kind is in the demand set.
func (d DemandSet) Has(kind string) bool {
	_, ok := d[kind]
	return ok
}

// Empty reports whether nothing is demanded. A nil set is empty.
func (d DemandSet) Empty() bool { return len(d) == 0 }

// Kinds returns the demanded kinds, sorted (stable, deduped by construction — map keys are unique).
func (d DemandSet) Kinds() []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NeedsServe reports whether any demanded kind is serve-dependent, using the CALLER's
// serve-dependency predicate.
func (d DemandSet) NeedsServe(serveDependent func(kind string) bool) bool {
	if serveDependent == nil {
		return false
	}
	for _, k := range d.Kinds() {
		if serveDependent(k) {
			return true
		}
	}
	return false
}
