package main

// services_test.go pins the contract `orchicon services` (the inner-loop verb the
// Makefile calls) depends on: the port map is DERIVED per instance, an explicit
// ORCHICON_*_PORT still wins, an unknown instance is REFUSED rather than silently
// given dev's ports, and an unknown verb is a usage error.
//
// HOME is redirected to a temp dir in each test so the instance's OWN port record
// (ports.json) cannot decide the result: with no record the defaults+overrides
// path is what runs, which is exactly the developer-who-never-installed case.

import (
	"testing"
)

func TestServicesPortsForUsesTheInstancesOwnColumn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCHICON_POSTGRES_PORT", "")
	t.Setenv("ORCHICON_NATS_PORT", "")

	ports, err := servicesPortsFor("prod")
	if err != nil {
		t.Fatalf("servicesPortsFor(prod): %v", err)
	}
	// prod is 5433/4223 — NOT dev's 5432/4222. Getting this wrong is the collision
	// the per-instance table exists to prevent.
	if got := ports["ORCHICON_POSTGRES_PORT"]; got != 5433 {
		t.Errorf("prod postgres port = %d, want 5433", got)
	}
	if got := ports["ORCHICON_NATS_PORT"]; got != 4223 {
		t.Errorf("prod nats port = %d, want 4223", got)
	}
	if got, want := servicesDSN(ports), "postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable"; got != want {
		t.Errorf("servicesDSN(prod) = %q, want %q", got, want)
	}
}

func TestServicesPortsForHonoursThePortOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCHICON_POSTGRES_PORT", "6543")

	ports, err := servicesPortsFor("dev")
	if err != nil {
		t.Fatalf("servicesPortsFor(dev): %v", err)
	}
	if got := ports["ORCHICON_POSTGRES_PORT"]; got != 6543 {
		t.Errorf("ORCHICON_POSTGRES_PORT override = %d, want 6543", got)
	}
	// The DSN follows the port, so the string a developer pastes cannot disagree
	// with the port the services bound.
	if got, want := servicesDSN(ports), "postgres://orchicon:orchicon@localhost:6543/orchicon?sslmode=disable"; got != want {
		t.Errorf("servicesDSN with the override = %q, want %q", got, want)
	}
}

func TestServicesPortsForRefusesAnUnknownInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A silent fallback to dev's ports is the collision the table forbids: an
	// unknown instance must be an error, not dev's column.
	if _, err := servicesPortsFor("staging"); err == nil {
		t.Fatal("servicesPortsFor(staging) = nil error, want a refusal")
	}
}

func TestRunServicesUnknownVerbIsAUsageError(t *testing.T) {
	if rc := runServices([]string{"frobnicate"}); rc != 2 {
		t.Errorf("runServices(frobnicate) = %d, want 2", rc)
	}
}
