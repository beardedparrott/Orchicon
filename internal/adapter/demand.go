package adapter

import "sort"

// DemandSet is the set of adapter kinds some consumer of this plane could
// dispatch to — the demand-keyed subject of the lazy host opencode serve.
//
// It is THE shared demand-set primitive (one computation, one place): the
// run-start runtime-serve gate (scheduler.runNeedsServe) accumulates its
// per-step worker model refs into AdapterDemandSet and asks it NeedsServe,
// and the host-side plane collects its tenant refs the same way
// (TenantDemandSet). A plane whose demand set contains no serve-dependent
// kind is "opencode-free": it must not spawn (or probe for) the host
// opencode serve at boot.
//
// The zero value is the empty set (nothing demanded) — the conservative
// answer is produced by AdapterDemandSet, never by the zero value.
type DemandSet map[string]struct{}

// AdapterDemandSet computes the adapter demand set for a list of model
// refs. Each ref contributes exactly the kind the DISPATCHER would route
// it to (adapter.AdapterKind) — the same routing view the reconciler and
// the model picker use, so demand and dispatch can never disagree. A ref
// that yields no kind (empty/malformed, or an unresolvable worker whose
// version lookup failed) contributes the DEFAULT kind ("opencode"), which
// reproduces the delivered run-start gate's conservative rule: an
// unresolvable step behaves exactly as it did before the gate became
// adapter-aware (gated on the serve) rather than silently skipping it.
//
// This generalizes the delivered gate predicate (workflow_reconciler.go
// runNeedsServe) into the single shared computation AC 7 requires; that
// predicate now DELEGATES here instead of carrying its own copy.
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

// Kinds returns the demanded kinds, sorted (stable, deduped by
// construction — map keys are unique). An empty set yields an empty
// (non-nil) slice.
func (d DemandSet) Kinds() []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NeedsServe reports whether any demanded kind is serve-dependent, using
// the CALLER's serve-dependency predicate. The predicate is passed in so
// there is exactly ONE serve-dependency evaluation on the plane
// (runtime.Lifecycle.ServeDependent) — the demand set itself never
// hardcodes which kinds need a serve, so the gate and the host serve
// cannot drift apart (AC 7).
//
// A nil predicate means "no kind knowledge available": nothing is
// known to need a serve (false).
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
