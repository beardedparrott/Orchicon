package config

// instance_test.go — `orch --instance dev|prod|test`.
//
// THE FEATURE: the TUI should be able to name an instance the way the desktop app
// does. That means ONE control-port table (internal/controlport, which the installer
// owns), not a second set of numbers, and a REFUSAL rather than a substitute when the
// name is not recognised — the same rule internal/controlport already states, and the
// reason an operator who types `prod` must never end up reading dev.

import (
	"fmt"
	"testing"

	"github.com/beardedparrott/orchicon/internal/controlport"
)

func TestInstanceURLUsesTheSharedTable(t *testing.T) {
	for name, port := range controlport.Defaults() {
		want := fmt.Sprintf("http://127.0.0.1:%d", port)
		if got := InstanceURL(name); got != want {
			t.Errorf("InstanceURL(%q) = %q, want %q — the TUI must read the same table the "+
				"installer and the desktop app read, not a third copy", name, got, want)
		}
	}
}

func TestInstanceURLRefusesAnUnknownName(t *testing.T) {
	// "" rather than a default: the caller turns this into an error. Substituting a
	// real instance here is how `--instance prdo` silently connects to dev.
	for _, name := range []string{"", "nope", "dev ", "DEV"} {
		if got := InstanceURL(name); got != "" {
			t.Errorf("InstanceURL(%q) = %q, want \"\" (an unknown instance must not resolve)", name, got)
		}
	}
}

func TestProfileForInstanceMatchesTheSavedCredential(t *testing.T) {
	cfg := &Config{Profiles: map[string]*Profile{
		"localhost:8080": {Name: "localhost:8080", URL: "http://localhost:8080", Token: "dev-token"},
		"localhost:8091": {Name: "localhost:8091", URL: "http://localhost:8091", Token: "prod-token"},
		"remote.example": {Name: "remote.example", URL: "https://orch.example.com", Token: "remote-token"},
	}}

	if p := ProfileForInstance(cfg, "dev"); p == nil || p.Token != "dev-token" {
		t.Errorf("dev resolved to %v, want the saved dev credential", p)
	}
	if p := ProfileForInstance(cfg, "prod"); p == nil || p.Token != "prod-token" {
		t.Errorf("prod resolved to %v, want the saved prod credential", p)
	}
	// An instance with no saved profile is NOT an error and NOT a fallback to another
	// instance: the caller targets the instance and collects a credential.
	if p := ProfileForInstance(cfg, "test"); p != nil {
		t.Errorf("test resolved to %v, want nil (nothing saved for it)", p)
	}
	// A remote profile must never be mistaken for a local instance.
	for _, name := range []string{"dev", "prod", "test"} {
		if p := ProfileForInstance(cfg, name); p != nil && p.Token == "remote-token" {
			t.Errorf("%q matched the REMOTE profile — a remote plane is not an instance", name)
		}
	}
	if p := ProfileForInstance(nil, "dev"); p != nil {
		t.Error("a nil config must resolve to nil, not panic or guess")
	}
}

func TestProfileForInstanceDoesNotMutateTheSavedProfile(t *testing.T) {
	// The retarget happens on a COPY (cmd/orch). This pins the shape it relies on: the
	// value handed back is the stored profile, so a caller that forgot to copy would
	// rewrite the operator's saved credentials in memory.
	saved := &Profile{Name: "localhost:8080", URL: "http://localhost:8080", Token: "dev-token"}
	cfg := &Config{Profiles: map[string]*Profile{"localhost:8080": saved}}

	p := ProfileForInstance(cfg, "dev")
	if p != saved {
		t.Fatal("ProfileForInstance returned a copy; the caller copies before retargeting, and this " +
			"test documents which side owns that copy")
	}
	cp := *p
	cp.URL = InstanceURL("dev")
	if saved.URL != "http://localhost:8080" {
		t.Errorf("the saved profile's URL changed to %q — retargeting must happen on a copy", saved.URL)
	}
}
