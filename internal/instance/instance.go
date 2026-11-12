// Package instance is the IMPORTABLE half of the instance table.
package instance

import (
	"fmt"
	"strings"
)

// Order is the canonical instance set, in documented order.
var Order = []string{"dev", "prod", "test"}

// controlPorts is the ORCHICON_CONTROL_PORT default per instance — the ONE column of the port table
// this package needs, because the control port IS the instance (see FromControlPort).
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

// FromControlPort maps a control port back to its instance.
func FromControlPort(port int) (string, bool) {
	for _, name := range Order {
		if controlPorts[name] == port {
			return name, true
		}
	}
	return "", false
}

// ValidateName applies the CLI's instance-name rule
// (cmd/orchicon/lifecycle.go:validateInstanceName).
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
