package aigateway

import (
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
)

// warmedDiscoverer returns a discoverer whose cache is already fresh, so
// ListModels answers in-process with no subprocess probe (see fetchOrCache).
func warmedDiscoverer(providerIDs ...string) *ModelDiscoverer {
	d := &ModelDiscoverer{ttl: time.Hour}
	d.cached = time.Now()
	for _, id := range providerIDs {
		d.cache = append(d.cache, &apiv1.OpenCodeModel{ProviderId: id, Id: id + "-some-model"})
	}
	return d
}

// TestCLIProviderRegistryScopesCLIIdsToTheDefaultKind is the regression pin for
// the picker bug: the live CLI provider ids are the OPENCODE CLI's namespace, so
// they may extend the DEFAULT kind only.
//
// The failure it prevents is not cosmetic. Unioning them into every kind put
// `commandcode`/`deepseek`/`gufo`/`halogen` in the CLAUDE picker's provider tier;
// selecting one then failed at the provider layer (no builtin profile for those
// ids), so the claude model tier loaded nothing and claude looked as though it
// could not source models at all.
func TestCLIProviderRegistryScopesCLIIdsToTheDefaultKind(t *testing.T) {
	reg := NewCLIProviderRegistry(nil, warmedDiscoverer("deepseek", "gufo", "halogen"))

	// The claude tier is its static catalog set — untouched by the CLI.
	if got, want := reg.Providers(adapter.KindClaude), []string{"anthropic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Providers(claude) = %v, want %v — the opencode CLI's provider ids are not claude's namespace", got, want)
	}
	for _, id := range []string{"deepseek", "gufo", "halogen", "commandcode"} {
		if reg.IsKnownProvider(adapter.KindClaude, id) {
			t.Errorf("IsKnownProvider(claude, %q) = true, want false: the claude tier would offer a provider its model tier cannot resolve", id)
		}
	}

	// The native kind is likewise governed by the static catalog for CLI ids:
	// its own providers come from the merged providers view, not the CLI.
	for _, id := range []string{"gufo", "halogen"} {
		if reg.IsKnownProvider(adapter.KindOrchicon, id) {
			t.Errorf("IsKnownProvider(orchicon, %q) = true, want false", id)
		}
	}
}

// TestCLIProviderRegistryKeepsCLIIdsUnderTheDefaultKind pins the other half: the
// gate must not REMOVE the CLI ids, or legacy 2-segment refs (which infer the
// default kind via adapter.ParseModelRef) stop validating and every existing
// opencode ref fails at save.
func TestCLIProviderRegistryKeepsCLIIdsUnderTheDefaultKind(t *testing.T) {
	reg := NewCLIProviderRegistry(nil, warmedDiscoverer("deepseek", "gufo"))

	for _, id := range []string{"deepseek", "gufo"} {
		if !reg.IsKnownProvider(adapter.DefaultAdapterKind, id) {
			t.Errorf("IsKnownProvider(%s, %q) = false, want true — legacy refs would stop validating", adapter.DefaultAdapterKind, id)
		}
	}
	got := reg.Providers(adapter.DefaultAdapterKind)
	for _, id := range []string{"deepseek", "gufo"} {
		found := false
		for _, p := range got {
			if p == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Providers(%s) = %v, want it to still include the CLI id %q", adapter.DefaultAdapterKind, got, id)
		}
	}

	// The empty/legacy kind folds to the default: the 2-segment grammar infers
	// the default kind, and parse-time callers pass that explicitly — a bare
	// kind must behave the same rather than losing the CLI namespace.
	if !reg.IsKnownProvider("", "gufo") {
		t.Error("IsKnownProvider(\"\", \"gufo\") = false, want true (an empty kind is the default kind)")
	}
}

// TestCLIProviderRegistryKeepsTenantCustomsPerKind pins that the additive layer
// (tenant customs, applied on clones) is still per-kind and unaffected by the
// CLI gate — a tenant-defined provider under one kind must not leak into
// another, and must survive for the kind it was added to.
func TestCLIProviderRegistryKeepsTenantCustomsPerKind(t *testing.T) {
	reg := NewCLIProviderRegistry(nil, warmedDiscoverer("gufo"))
	clone := reg.Clone()
	clone.AddAdapterKind(adapter.KindClaude, "my-anthropic-proxy")

	if !clone.IsKnownProvider(adapter.KindClaude, "my-anthropic-proxy") {
		t.Error("a tenant custom added under claude must validate under claude")
	}
	if reg.IsKnownProvider(adapter.KindClaude, "my-anthropic-proxy") {
		t.Error("AddAdapterKind on a clone mutated the shared registry")
	}
	if clone.IsKnownProvider(adapter.KindOrchicon, "my-anthropic-proxy") {
		t.Error("a tenant custom added under claude leaked into the orchicon kind")
	}
	// ...and the gate does not drop the custom from the kind's provider set.
	got := clone.Providers(adapter.KindClaude)
	found := false
	for _, p := range got {
		if p == "my-anthropic-proxy" {
			found = true
		}
	}
	if !found {
		t.Errorf("Providers(claude) = %v, want it to include the tenant custom", got)
	}
}
