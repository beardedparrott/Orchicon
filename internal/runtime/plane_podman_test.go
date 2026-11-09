package runtime

// Plane URL derivation under the PODMAN engine (engine.go + lifecycle.go planePublicURL), for the
// parts that are NOT the host-alias spelling.

import (
	"strings"
	"testing"
)

// clearPlaneURLEnvForEngine neutralises the plane-URL inputs AND the engine
// inputs, then applies the caller's overrides.
func clearPlaneURLEnvForEngine(t *testing.T, kv ...string) {
	t.Helper()
	t.Setenv(EnvEngine, "")
	t.Setenv(EnvEngineAlias, "")
	t.Setenv("ORCHICON_PLANE_PUBLIC_URL", "")
	t.Setenv("ORCHICON_CONTAINER_MODE", "")
	t.Setenv("ORCHICON_HTTP_EXTRA_BIND", "")
	t.Setenv("ORCHICON_HTTP_ADDR", "")
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestPlanePublicURLPodmanHostResidency(t *testing.T) {
	t.Run("the stable host name plus this instance's port", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://host.docker.internal:8091" {
			t.Fatalf("podman host mode = %q, want http://host.docker.internal:8091 (ADR-0013 D2.4 maps that name for podman too)", got)
		}
	})

	t.Run("the extra bind still wins over the alias", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_HTTP_EXTRA_BIND", "10.42.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://10.42.0.1:8091" {
			t.Fatalf("podman + extra bind = %q, want http://10.42.0.1:8091", got)
		}
	})
}

func TestPlanePublicURLPodmanContainerResidency(t *testing.T) {
	t.Run("never emits the docker bridge gateway", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_HTTP_ADDR", ":8092",
		)
		got := planePublicURL()
		if ip := containerIPAddress(); ip != "" {
			// Rootful podman / netavark: the plane's own address is derivable
			// and must be used, at THIS instance's port.
			if want := "http://" + ip + ":8092"; got != want {
				t.Fatalf("podman container mode = %q, want %q", got, want)
			}
			return
		}
		// Rootless podman: no bridge IP exists.
		if got != "" {
			t.Fatalf("podman container mode without a resolvable IP = %q, want empty (explicit ORCHICON_PLANE_PUBLIC_URL required)", got)
		}
	})

	t.Run("explicit override satisfies podman container residency", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_PLANE_PUBLIC_URL", "http://plane.example:9999/",
		)
		if got := planePublicURL(); got != "http://plane.example:9999" {
			t.Fatalf("override = %q, want http://plane.example:9999", got)
		}
	})
}

func TestPlanePublicURLPodmanPerInstance(t *testing.T) {
	// dev and prod share one podman machine and the same host alias; only the PORT tells their planes
	// apart.
	clearPlaneURLEnvForEngine(t,
		EnvEngine, enginePodman,
		"ORCHICON_HTTP_ADDR", "127.0.0.1:8080",
	)
	dev := planePublicURL()

	clearPlaneURLEnvForEngine(t,
		EnvEngine, enginePodman,
		"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
	)
	prod := planePublicURL()

	if dev != "http://host.docker.internal:8080" {
		t.Fatalf("dev URL = %q, want http://host.docker.internal:8080", dev)
	}
	if prod != "http://host.docker.internal:8091" {
		t.Fatalf("prod URL = %q, want http://host.docker.internal:8091", prod)
	}
	if dev == prod {
		t.Fatalf("dev and prod advertise the same plane URL %q — a run's workers would dial the other instance", dev)
	}
}

// TestPlaneURLForRun pins the mint-path guard around the derivation.
func TestPlaneURLForRun(t *testing.T) {
	t.Run("undeliverable container-resident podman plane fails loudly", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_HTTP_ADDR", ":8092",
		)
		// Pin the UNDERIVABLE case instead of inheriting the test host's /etc/hosts: a container-resident
		// plane with no hostname entry has no address to derive, which is exactly the Podman topology.
		prevHosts := hostsFilePath
		hostsFilePath = "/nonexistent-hosts-file-for-test"
		t.Cleanup(func() { hostsFilePath = prevHosts })
		if ip := containerIPAddress(); ip != "" {
			t.Fatalf("containerIPAddress() = %q with no hosts file, want empty", ip)
		}
		got, err := planeURLForRun()
		if err == nil {
			t.Fatalf("planeURLForRun() = %q, nil — a run must never be handed a plane URL this instance cannot derive", got)
		}
		if got != "" {
			t.Fatalf("planeURLForRun() URL = %q alongside the error, want empty", got)
		}
		for _, want := range []string{"ORCHICON_PLANE_PUBLIC_URL", enginePodman} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q must name %q so the operator knows the fix", err.Error(), want)
			}
		}
	})

	t.Run("a derivable plane URL passes through unchanged", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvEngine, enginePodman,
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		got, err := planeURLForRun()
		if err != nil {
			t.Fatalf("planeURLForRun() error = %v, want nil", err)
		}
		if want := "http://host.docker.internal:8091"; got != want {
			t.Fatalf("planeURLForRun() = %q, want %q", got, want)
		}
	})
}
