package instance

// instance_test.go — the ANTI-DRIFT gate for the mirrored instance table, plus
// the two refusals the Manage Server page depends on ("ask, never guess").
//
// This package exists only because cmd/orchicon's table is `package main`, so the
// agreement with the two authoritative sides (the Go installer table and the
// shell instance table) is ASSERTED here rather than promised in a comment.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestControlPortsMatchTheInstallerTable reads the CONTROL-PORT row out of
// cmd/orchicon/install_ports.go and compares it to controlPorts for every
// instance in Order. A port drifting here would make Manage Server address one
// instance while displaying another.
func TestControlPortsMatchTheInstallerTable(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "orchicon", "install_ports.go"))
	if err != nil {
		t.Fatalf("read cmd/orchicon/install_ports.go: %v", err)
	}
	text := string(src)

	// The row is: {Env: "ORCHICON_CONTROL_PORT", ... Defaults: portDefaults(d, p, t), ...}
	i := strings.Index(text, `Env: "ORCHICON_CONTROL_PORT"`)
	if i < 0 {
		t.Fatal("install_ports.go has no ORCHICON_CONTROL_PORT row — the table this package mirrors is gone")
	}
	rest := text[i:]
	j := strings.Index(rest, "portDefaults(")
	if j < 0 {
		t.Fatalf("the ORCHICON_CONTROL_PORT row carries no portDefaults(...) triple")
	}
	args := rest[j+len("portDefaults("):]
	k := strings.Index(args, ")")
	if k < 0 {
		t.Fatal("unterminated portDefaults( in the ORCHICON_CONTROL_PORT row")
	}
	fields := strings.Split(args[:k], ",")
	if len(fields) != len(Order) {
		t.Fatalf("portDefaults in install_ports.go has %d columns, instance.Order has %d", len(fields), len(Order))
	}
	for idx, name := range Order {
		want, err := strconv.Atoi(strings.TrimSpace(fields[idx]))
		if err != nil {
			t.Fatalf("column %d of portDefaults is not a number (%q)", idx, fields[idx])
		}
		got, ok := ControlPort(name)
		if !ok {
			t.Fatalf("instance.Order has %q but controlPorts does not — the mirrored table drifted", name)
		}
		if got != want {
			t.Errorf("control port drifted for %q: install_ports.go says %d, internal/instance says %d", name, want, got)
		}
	}
}

// TestInstanceSetMatchesTheShellTable asserts BOTH directions against
// scripts/lib/instances.sh: ORCHICON_INSTANCES is the set, and each instance has
// a real case arm whose PLANE_HTTP_PORT equals the control port here.
func TestInstanceSetMatchesTheShellTable(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "lib", "instances.sh"))
	if err != nil {
		t.Fatalf("read scripts/lib/instances.sh: %v", err)
	}
	script := string(src)

	var shellInstances []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ORCHICON_INSTANCES=") {
			shellInstances = strings.Fields(strings.Trim(strings.TrimPrefix(line, "ORCHICON_INSTANCES="), `"'`))
			break
		}
	}
	if len(shellInstances) == 0 {
		t.Fatal("scripts/lib/instances.sh declares no ORCHICON_INSTANCES")
	}
	if got, want := strings.Join(shellInstances, " "), strings.Join(Order, " "); got != want {
		t.Fatalf("instance set drifted: scripts/lib/instances.sh has %q, internal/instance.Order has %q", got, want)
	}

	// Per-instance PLANE_HTTP_PORT must equal our control port.
	for _, name := range Order {
		armStart := strings.Index(script, "\n    "+name+")")
		if armStart < 0 {
			t.Errorf("scripts/lib/instances.sh has no case arm for %q", name)
			continue
		}
		arm := script[armStart:]
		if end := strings.Index(arm, ";;"); end > 0 {
			arm = arm[:end]
		}
		var shellPort int
		for _, line := range strings.Split(arm, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "PLANE_HTTP_PORT=") {
				shellPort, _ = strconv.Atoi(strings.TrimPrefix(line, "PLANE_HTTP_PORT="))
			}
		}
		got, _ := ControlPort(name)
		if shellPort != got {
			t.Errorf("control port drifted for %q: scripts/lib/instances.sh PLANE_HTTP_PORT=%d, internal/instance says %d",
				name, shellPort, got)
		}
	}
}

// TestValidateNameMatchesTheCLIRule keeps the path-safety rule equal to
// cmd/orchicon/lifecycle.go's validateInstanceName — a name the CLI refuses must
// not be usable through the desktop binding.
func TestValidateNameMatchesTheCLIRule(t *testing.T) {
	cli, err := os.ReadFile(filepath.Join("..", "..", "cmd", "orchicon", "lifecycle.go"))
	if err != nil {
		t.Fatalf("read cmd/orchicon/lifecycle.go: %v", err)
	}
	// The CLI's rule is a single refusal expression; if it changes shape this
	// assertion is the place that finds out.
	if !strings.Contains(string(cli), `strings.ContainsAny(instance, "/\\")`) {
		t.Error("cmd/orchicon's validateInstanceName no longer refuses path separators — re-check internal/instance.ValidateName against the CLI rule")
	}
	if !strings.Contains(string(cli), `strings.Contains(instance, "..")`) {
		t.Error("cmd/orchicon's validateInstanceName no longer refuses \"..\" — re-check internal/instance.ValidateName against the CLI rule")
	}

	for _, name := range []string{"dev", "prod", "test", "dev2", "my-instance"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) refused a good name: %v", name, err)
		}
	}
	for _, name := range []string{"", "   ", "dev/../prod", "../prod", "a/b", `a\b`, "dev..prod"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) accepted a name the CLI would refuse", name)
		}
	}
}

// TestFromControlPortRefusesAnUnknownPort is AC7's "ask rather than guess": an
// unrecognised port maps to NO instance, so the page must prompt.
func TestFromControlPortRefusesAnUnknownPort(t *testing.T) {
	for _, name := range Order {
		port, ok := ControlPort(name)
		if !ok {
			t.Fatalf("ControlPort(%q) refused a known instance", name)
		}
		got, ok := FromControlPort(port)
		if !ok || got != name {
			t.Errorf("FromControlPort(%d) = (%q, %v), want (%q, true)", port, got, ok, name)
		}
	}
	for _, port := range []int{0, 9999, 3000, 5432} {
		if got, ok := FromControlPort(port); ok || got != "" {
			t.Errorf("FromControlPort(%d) = (%q, %v) — an unknown port must NOT resolve to an instance", port, got, ok)
		}
	}
}
