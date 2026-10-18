// Package instance is the IMPORTABLE half of the instance table.
//
// WHY A SECOND TABLE EXISTS AT ALL: the authoritative table is
// cmd/orchicon/install_ports.go's installerPorts(), and it is `package main` —
// a Wails app cannot import it. So the instance NAME SET and the CONTROL ports
// (the port the plane answers on, which is also the port that IDENTIFIES the
// instance) are mirrored here, deliberately small, and the agreement is
// ASSERTED by a test on this side (instance_test.go) the same way
// cmd/orchicon/install_ports_test.go already asserts the Go/shell agreement.
//
// Adding an instance to one side without the other is a RED TEST, not a silent
// fall-through to dev's port — which would point a Manage Server action at the
// wrong plane, the one mistake this whole feature must not make.
package instance

import (
	"fmt"
	"strings"
)

// Order is the canonical instance set, in documented order. It must match
// ORCHICON_INSTANCES in scripts/lib/instances.sh and instanceOrder in
// cmd/orchicon/install_ports.go; both agreements are asserted by tests.
var Order = []string{"dev", "prod", "test"}

// controlPorts is the ORCHICON_CONTROL_PORT default per instance — the ONE
// column of the port table this package needs, because the control port IS the
// instance (see FromControlPort).
var controlPorts = map[string]int{"dev": 8080, "prod": 8091, "test": 8092}

// Known reports whether name is an instance this build knows about.
func Known(name string) bool {
	_, ok := controlPorts[name]
	return ok
}

// ControlPort returns the instance's control port. ok=false is a REFUSAL: an
// unknown instance must never inherit another's port.
func ControlPort(name string) (int, bool) {
	p, ok := controlPorts[name]
	return p, ok
}

// FromControlPort maps a control port back to its instance. ok=false is the
// "ask rather than guess" case — an unrecognised port must produce a question,
// never a default.
func FromControlPort(port int) (string, bool) {
	for _, name := range Order {
		if controlPorts[name] == port {
			return name, true
		}
	}
	return "", false
}

// ValidateName applies the CLI's instance-name rule
// (cmd/orchicon/lifecycle.go:validateInstanceName): the name forms a DIRECTORY
// name, so a path separator or ".." is refused. The rule is shared by being
// asserted equal in instance_test.go, because the two copies cannot share a
// constant (one is package main).
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("no instance named: every action must say which instance it acts on")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid instance name %q: it forms the instance state directory name, so it "+
			"may not contain a path separator or \"..\"", name)
	}
	return nil
}
