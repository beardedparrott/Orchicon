package main

import (
	"reflect"
	"testing"

	"github.com/beardedparrott/orchicon/internal/controlport"
)

// TestControlPortTableMatchesInstaller — the desktop app resolves an instance from the plane's
// control port, and the numbers it uses must be THE INSTALLER'S, not a copy that will drift.
func TestControlPortTableMatchesInstaller(t *testing.T) {
	var found bool
	for _, p := range installerPorts() {
		if p.Env != "ORCHICON_CONTROL_PORT" {
			continue
		}
		found = true
		if !reflect.DeepEqual(controlport.Defaults(), p.Defaults) {
			t.Errorf("internal/controlport.Defaults() = %v, but the installer's %s row is %v — they describe the "+
				"same instances and must agree", controlport.Defaults(), p.Env, p.Defaults)
		}
		// And the row must be the one that is BOTH the control plane's and the host plane's bind,
		// because that is what makes "the control port is the instance" true.
		if !p.HostPlaneBinds {
			t.Errorf("the %s row no longer sets HostPlaneBinds — the control port is the port the host plane "+
				"listens on, which is why the desktop app can read the instance off it", p.Env)
		}
	}
	if !found {
		t.Fatal("installerPorts() has no ORCHICON_CONTROL_PORT row — the desktop instance resolution has no table to agree with")
	}
}
