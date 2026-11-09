package runtime

import (
	"os"
	"testing"
)

// TestPlanePublicURLResolution verifies the plane-channel URL the minted credential advertises is
// reachable from a runtime container on the docker bridge.
func TestPlanePublicURLResolution(t *testing.T) {
	// Preserve + restore env so tests are hermetic.
	prevURL := os.Getenv("ORCHICON_PLANE_PUBLIC_URL")
	prevMode := os.Getenv("ORCHICON_CONTAINER_MODE")
	prevEngine := os.Getenv(EnvEngine)
	prevEngineAlias := os.Getenv(EnvEngineAlias)
	defer func() {
		setEnvOrUnset("ORCHICON_PLANE_PUBLIC_URL", prevURL)
		setEnvOrUnset("ORCHICON_CONTAINER_MODE", prevMode)
		setEnvOrUnset(EnvEngine, prevEngine)
		setEnvOrUnset(EnvEngineAlias, prevEngineAlias)
	}()
	// Docker is the engine whose container residency HAS a fallback: a rootless podman container has
	// neither an /etc/hosts bridge IP nor a 172.17.0.1 gateway.
	os.Setenv(EnvEngine, engineDocker)
	os.Unsetenv(EnvEngineAlias)

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

	// 3.
	os.Setenv("ORCHICON_CONTAINER_MODE", "1")
	if got := planePublicURL(); got == "" {
		t.Fatal("container mode with unresolvable IP must not return an empty URL under docker")
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
