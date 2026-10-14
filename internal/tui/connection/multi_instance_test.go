package connection

// multi_instance_test.go — ONE ENTRY PER INSTANCE, AND ONE INSTANCE PER PROCESS.
//
// orch is meant to be run more than once at a time, and the operator's report is that it
// was not: "It seems to flop which port you are connecting to each time you run it." It
// was not random. Every profile was named the literal "default" and SaveProfile sets
// cfg.Active to that name, so there was only ever ONE slot: connecting to a second
// instance overwrote the first one's URL, access token and refresh token in place, and
// every later launch went to whichever instance had been connected to last.

import (
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func profileNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		out = append(out, name)
	}
	return out
}

// CONNECTING TO A SECOND INSTANCE MUST NOT DESTROY THE FIRST ONE'S ENTRY.
//
// This drives the REAL save path twice — once as a connect to dev, once as a connect to
// prod — and asserts both entries survive with their own credentials.
func TestTwoInstancesGetTwoProfileSlots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	dev := New(nil, ProbeFuncs{})
	dev.inputs[fieldURL].SetValue("http://localhost:8080")
	if err := SaveProfile(path, &Result{Profile: dev.buildProfile("dev-token", ""), ServerVersion: "v1"}); err != nil {
		t.Fatalf("save dev: %v", err)
	}

	prod := New(nil, ProbeFuncs{})
	prod.inputs[fieldURL].SetValue("http://localhost:8091")
	if err := SaveProfile(path, &Result{Profile: prod.buildProfile("prod-token", ""), ServerVersion: "v1"}); err != nil {
		t.Fatalf("save prod: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("connecting to a second instance left %d profile(s) %v, want 2 — the first instance's "+
			"URL and credentials were overwritten, which is the operator's \"it flops which port you "+
			"connect to\"", len(cfg.Profiles), profileNames(cfg))
	}
	if got := cfg.Profiles["localhost-8080"]; got == nil || got.URL != "http://localhost:8080" || got.Token != "dev-token" {
		t.Fatalf("the dev entry was not preserved: %+v", got)
	}
	if got := cfg.Profiles["localhost-8091"]; got == nil || got.URL != "http://localhost:8091" || got.Token != "prod-token" {
		t.Fatalf("the prod entry was not saved: %+v", got)
	}
	// Active follows the most recent connect: that is which instance a plain `orch` uses
	// next, and it is the behaviour the single-slot build was TRYING to express.
	if cfg.Active != "localhost-8091" {
		t.Errorf("Active = %q, want the most recently connected instance", cfg.Active)
	}
}

// The name is the INSTANCE, not a constant — and it must be a name render() can write as
// a TOML bare key ([profiles.<name>]), or the config would not load back.
func TestProfileNameIsDerivedFromTheInstance(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080":    "localhost-8080",
		"http://localhost:8080/":   "localhost-8080",
		"https://orch.example.com": "orch-example-com",
		// An IPv6 authority collapses to this rather than to something readable — an
		// acceptable cost: it is still a name render() can write as a bare key and
		// parse() can read back, which is the only property the naming has to hold. An
		// IPv6 instance is typically reached by hostname anyway.
		"https://[::1]:8091": "--1-8091",
		"":                   "default",
		"not a url":          "default",
	}
	for raw, want := range cases {
		if got := profileNameForURL(raw); got != want {
			t.Errorf("profileNameForURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
