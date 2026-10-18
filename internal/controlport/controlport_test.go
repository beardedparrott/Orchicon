package controlport

import "testing"

// THE CONTROL PORT IS THE INSTANCE, and these are the three the installer
// defines (cmd/orchicon/install_ports.go's ORCHICON_CONTROL_PORT row, mirrored
// on the shell side by scripts/lib/instances.sh).
func TestDefaults(t *testing.T) {
	want := map[string]int{"dev": 8080, "prod": 8091, "test": 8092}
	for instance, port := range want {
		if got := Defaults()[instance]; got != port {
			t.Errorf("Defaults()[%q] = %d, want %d", instance, got, port)
		}
	}
	if len(Defaults()) != len(want) {
		t.Errorf("Defaults() describes %d instances, want %d — a new instance needs the installer's table and this one kept equal",
			len(Defaults()), len(want))
	}
}

func TestInstanceForPort(t *testing.T) {
	for port, want := range map[int]string{8080: "dev", 8091: "prod", 8092: "test"} {
		got, ok := InstanceForPort(port)
		if !ok || got != want {
			t.Errorf("InstanceForPort(%d) = (%q, %v), want (%q, true)", port, got, ok, want)
		}
	}
	// AN UNKNOWN PORT IS A REFUSAL. A silent "dev" here would make the window
	// title and the Manage Server page describe a plane that is not the one the
	// app is attached to.
	if got, ok := InstanceForPort(9999); ok || got != "" {
		t.Errorf("InstanceForPort(9999) = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestInstanceForURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:8080":  "dev",
		"http://127.0.0.1:8091":  "prod",
		"http://localhost:8092/": "test",
	} {
		got, ok := InstanceForURL(raw)
		if !ok || got != want {
			t.Errorf("InstanceForURL(%q) = (%q, %v), want (%q, true)", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"http://example.com",    // scheme default port 80 — no instance
		"https://127.0.0.1",     // 443 — no instance
		"http://127.0.0.1:9999", // unknown port
		"not a url",
		"",
	} {
		if got, ok := InstanceForURL(raw); ok || got != "" {
			t.Errorf("InstanceForURL(%q) = (%q, %v), want (\"\", false)", raw, got, ok)
		}
	}
}

func TestHostPort(t *testing.T) {
	if got := HostPort("http://127.0.0.1:8091"); got != "127.0.0.1:8091" {
		t.Errorf("HostPort = %q, want 127.0.0.1:8091", got)
	}
	// An unparseable URL is echoed rather than blanked: the title must still say
	// WHICH address the app was pointed at, typo included.
	if got := HostPort("::nonsense"); got != "::nonsense" {
		t.Errorf("HostPort of an unparseable URL = %q, want the raw string", got)
	}
}
