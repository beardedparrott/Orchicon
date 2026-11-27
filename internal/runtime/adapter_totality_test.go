package runtime

// adapter_totality_test.go — the boot-profile half of the add-a-kind sweep. See
// internal/adapter/adapter_totality_test.go for why the catalog drives these tests.

import (
	"sort"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// TestAdapterKindTotalityBootProfile asserts every catalog kind is CLASSIFIED by classifyAdapter —
// the ONE declaration the daemon mounts from, the mount-never-bake guard reads and the host-input
// fingerprint walks.
//
// A kind missing here does not fail at registration: it fails at container create, as a run that
// mounts nothing and cannot find its CLI.
func TestAdapterKindTotalityBootProfile(t *testing.T) {
	set := adapter.BuiltinAdapterKinds()
	if len(set) < 4 {
		t.Fatalf("the builtin catalog declared only %d adapter kind(s) — the LIVE kind source shrank, so this "+
			"totality assertion under-covers", len(set))
	}
	kinds := make([]string, 0, len(set))
	for k := range set {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	const home = "/home/orchicon-host"
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			c, declared := classifyAdapter(home, kind)
			if !declared {
				t.Fatalf("adapter kind %q is declared by the builtin catalog but NOT classified by classifyAdapter "+
					"(internal/runtime/bootprofile.go). Add a case: its host installs (probe + fingerprint), both "+
					"consumption axes (perSessionBundle / runUnionAtServe) and a note for any negative axis. An "+
					"unclassified kind mounts nothing, is invisible to the host-input fingerprint and slips past "+
					"the mount-never-bake guard.", kind)
			}
			// THE NATIVE ADAPTER contributes no host install by design (it is the product binary,
			// bind-mounted at /usr/local/bin/orchicon). Every other kind must declare at least one.
			if kind != nativeAdapterKind && len(c.installs) == 0 {
				t.Errorf("adapter kind %q is classified but declares NO host installs — either declare where its CLI "+
					"lives on the host, or classify it as a product-binary kind like %q", kind, nativeAdapterKind)
			}
			// Both consumption axes negative requires an explanation; silence is how the next adapter
			// slips past. (The conformance gate states this at length; this is the totality echo.)
			if !c.perSessionBundle && c.note == "" {
				t.Errorf("adapter kind %q declares perSessionBundle:false with NO note — an unexplained negative", kind)
			}
			if !c.runUnionAtServe && c.note == "" {
				t.Errorf("adapter kind %q declares runUnionAtServe:false with NO note — an unexplained negative", kind)
			}
		})
	}

	// The fingerprint loop must actually reach the new kind, not just the two it shipped with.
	found := false
	for _, root := range adapterFingerprintRoots(home) {
		if root == "/home/orchicon-host/.local/bin/agy" {
			found = true
		}
	}
	if !found {
		t.Error("adapterFingerprintRoots() does not include antigravity's agy launcher — a CLI upgrade or reinstall " +
			"would then serve a stale adapter from a warm pooled container (add the kind to hostfp.go's loop)")
	}
}
