package providers

import (
	"strings"
	"testing"
)

// AC: "Custom providers must not be rewritten on a host plane." A provider stored as
// http://localhost:11434 keeps that URL: on the host, localhost IS the host.
func TestResolveForPlaneHostModeKeepsLoopback(t *testing.T) {
	t.Setenv("ORCHICON_CONTAINER_MODE", "")

	if gw := hostGatewayIP(); gw != "" {
		t.Fatalf("host mode must not detect a bridge gateway, got %q", gw)
	}

	for _, stored := range []string{
		"http://localhost:11434",
		"http://localhost:11434/v1",
		"http://127.0.0.1:8095/v1",
		"http://0.0.0.0:8000",
	} {
		got, note, changed := resolveForPlane(stored)
		if got != stored || changed || note != "" {
			t.Errorf("host plane rewrote a custom provider: in=%q out=%q changed=%v note=%q", stored, got, changed, note)
		}
	}

	// repairCandidates must not invent a rewritten gateway host either — on the host there is no
	// gateway to reach, so the stored loopback URL is already correct and every candidate keeps it.
	for _, cand := range repairCandidates("http://localhost:11434") {
		if strings.Contains(cand, "172.17.") {
			t.Errorf("host plane repair invented a gateway candidate: %q", cand)
		}
		if !strings.Contains(cand, "localhost") {
			t.Errorf("host plane repair dropped the correct loopback host: %q", cand)
		}
	}
}

// The other half of the contract, and the reason the host path is safe: in container mode the
// rewrite DOES happen and preserves the port.
func TestResolveForPlaneContainerModeRewritesToGateway(t *testing.T) {
	t.Setenv("ORCHICON_CONTAINER_MODE", "1")
	gw := hostGatewayIP()
	if gw == "" {
		t.Skip("no detectable default gateway in this environment")
	}
	got, note, changed := resolveForPlane("http://localhost:11434/v1")
	if !changed {
		t.Fatalf("container mode must rewrite loopback, got %q (note %q)", got, note)
	}
	if strings.Contains(got, "localhost") || !strings.Contains(got, gw) {
		t.Errorf("rewrite did not target the gateway %q: %q", gw, got)
	}
	if !strings.HasSuffix(got, ":11434/v1") {
		t.Errorf("rewrite dropped the port or path: %q", got)
	}
}
