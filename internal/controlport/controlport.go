// Package controlport maps a control-plane port to the ORCHICON instance that
// owns it.
//
// THE INSTANCE *IS* THE CONTROL PORT. A plane's control port (the web UI + API
// port) is the one thing a client can observe about which instance it is
// talking to without an API call, so "which instance is this app pointing at?"
// is answered by the port table and by nothing else.
//
// THERE IS ALREADY A TABLE, AND IT IS NOT THIS ONE. cmd/orchicon/install_ports.go
// owns the per-instance port defaults (dev 8080 / prod 8091 / test 8092) and
// scripts/lib/instances.sh mirrors it on the shell side;
// TestInstallPortDefaultsMatchShellInstanceTable asserts those two agree. The
// desktop app is the THIRD consumer of the same knowledge, so it must consume
// the SAME numbers, not a third copy that will drift. That is what
// cmd/orchicon/controlport_table_test.go asserts: this package's Defaults()
// must deep-equal the installer's ORCHICON_CONTROL_PORT row.
//
// STDLIB ONLY, for the same reason internal/planecontrol is: the desktop shell
// is a leaf client and must be able to use this without pulling the plane's
// HTTP stack (or a webview) in behind it.
package controlport

import (
	"net/url"
	"strconv"
)

// Defaults returns the control port per instance. Values are the
// ORCHICON_CONTROL_PORT row of cmd/orchicon/install_ports.go — see the package
// doc for why they are copied here at all (and the test that keeps them equal).
func Defaults() map[string]int {
	return map[string]int{"dev": 8080, "prod": 8091, "test": 8092}
}

// InstanceForPort resolves a control port to an instance.
//
// AN UNKNOWN PORT IS A REFUSAL, NOT A FALLBACK (ok=false): mirroring
// installPort.defaultFor in cmd/orchicon/install_ports.go, where an unknown
// instance must not inherit another's port either. Silently answering "dev" for
// a plane on an operator's custom port would make the app's title bar — and the
// Manage Server page — state something false about which plane it is attached
// to, which is worse than admitting it does not know.
func InstanceForPort(port int) (string, bool) {
	for instance, p := range Defaults() {
		if p == port {
			return instance, true
		}
	}
	return "", false
}

// InstanceForURL resolves http(s)://host:port to an instance. An empty port
// means the scheme's default (80/443), which no instance uses — so it is a
// refusal like any other unknown port.
func InstanceForURL(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", false
		}
		port = n
	}
	if port == 0 {
		switch u.Scheme {
		case "https":
			port = 443
		case "http":
			port = 80
		default:
			return "", false
		}
	}
	return InstanceForPort(port)
}

// HostPort returns the host:port a URL points at, for display in the window
// title. It falls back to the raw string when the URL does not parse, because a
// title that says nothing about the plane is worse than one that repeats the
// operator's typo.
func HostPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
