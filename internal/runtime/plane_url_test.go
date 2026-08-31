package runtime

import (
	"os"
	"testing"
)

// TestPlanePublicURLResolution verifies the plane-channel URL the minted
// credential advertises is reachable from a runtime container on the docker
// bridge. The regression: ORCHICON_PLANE_PUBLIC_URL pointed at the host's
// published port (172.17.0.1:8091 for prod), which a bridge container cannot
// reach (hairpin-NAT drop) — so the plane-channel MCP sidecar timed out on
// every call. In container mode the plane must advertise its OWN bridge IP +
// internal port 8080, which runtime containers reach directly on the bridge.
func TestPlanePublicURLResolution(t *testing.T) {
	// Preserve + restore env so tests are hermetic.
	prevURL := os.Getenv("ORCHICON_PLANE_PUBLIC_URL")
	prevMode := os.Getenv("ORCHICON_CONTAINER_MODE")
	defer func() {
		setEnvOrUnset("ORCHICON_PLANE_PUBLIC_URL", prevURL)
		setEnvOrUnset("ORCHICON_CONTAINER_MODE", prevMode)
	}()

	// 1. Explicit override always wins (trailing slash trimmed).
	os.Setenv("ORCHICON_PLANE_PUBLIC_URL", "http://plane.example:9999/")
	os.Setenv("ORCHICON_CONTAINER_MODE", "1")
	if got := planePublicURL(); got != "http://plane.example:9999" {
		t.Fatalf("override = %q, want http://plane.example:9999 (trailing slash trimmed)", got)
	}

	// 2. Container mode with a resolvable IP: use the plane's own bridge IP.
	os.Unsetenv("ORCHICON_PLANE_PUBLIC_URL")
	os.Setenv("ORCHICON_CONTAINER_MODE", "1")
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Docker's /etc/hosts entry for the container hostname.
	body := "127.0.0.1 localhost\n172.17.0.3 " + hostname + "\n"
	if got := parseHostsIP(body, hostname); got != "172.17.0.3" {
		t.Fatalf("parseHostsIP = %q, want 172.17.0.3", got)
	}

	// 3. Container mode but IP unresolvable (no /etc/hosts entry): fall back
	// to the gateway default rather than emitting an empty URL.
	os.Setenv("ORCHICON_CONTAINER_MODE", "1")
	if got := planePublicURL(); got == "" {
		t.Fatal("container mode with unresolvable IP must not return an empty URL")
	}

	// 4. Host mode (no container-mode flag): gateway default.
	os.Unsetenv("ORCHICON_CONTAINER_MODE")
	if got := planePublicURL(); got != "http://172.17.0.1:8080" {
		t.Fatalf("host mode = %q, want http://172.17.0.1:8080", got)
	}
}

// TestContainerIPAddressParsesHosts verifies containerIPAddress reads the
// container's own bridge IP from /etc/hosts (Docker writes "<ip> <hostname>").
func TestContainerIPAddressParsesHosts(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	body := "127.0.0.1 localhost\n172.17.0.3 " + hostname + "\n"
	if got := parseHostsIP(body, hostname); got != "172.17.0.3" {
		t.Fatalf("parseHostsIP = %q, want 172.17.0.3", got)
	}
	if got := parseHostsIP(body, "nonexistent"); got != "" {
		t.Fatalf("parseHostsIP unknown host = %q, want empty", got)
	}
}

func setEnvOrUnset(k, v string) {
	if v == "" {
		os.Unsetenv(k)
		return
	}
	os.Setenv(k, v)
}
