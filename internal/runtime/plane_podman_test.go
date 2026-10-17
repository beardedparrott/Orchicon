package runtime

// Plane URL derivation under Podman (engine.go + lifecycle.go planePublicURL).
//
// What must NOT change when the engine changes: the per-instance PORT
// derivation (planeHTTPPort) and the override precedence — those are what
// keep one instance's workers from ever dialling another instance's plane.
// What MUST change: the host alias (Docker's host.docker.internal does not
// resolve inside a Podman container; host.containers.internal does), and
// the container-residency fallback (172.17.0.1 is a Docker bridge address
// that rootless Podman does not have).

import (
	"strings"
	"testing"
)

// clearPlaneURLEnvForEngine neutralises the plane-URL inputs AND the engine
// inputs, then applies the caller's overrides.
func clearPlaneURLEnvForEngine(t *testing.T, kv ...string) {
	t.Helper()
	t.Setenv(EnvRuntimeEngine, "")
	t.Setenv(EnvContainerBin, "")
	t.Setenv(EnvPlaneHostAlias, "")
	t.Setenv("ORCHICON_PLANE_PUBLIC_URL", "")
	t.Setenv("ORCHICON_CONTAINER_MODE", "")
	t.Setenv("ORCHICON_HTTP_EXTRA_BIND", "")
	t.Setenv("ORCHICON_HTTP_ADDR", "")
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestPlanePublicURLPodmanHostResidency(t *testing.T) {
	t.Run("stable podman host name plus this instance's port", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://host.containers.internal:8091" {
			t.Fatalf("podman host mode = %q, want http://host.containers.internal:8091", got)
		}
	})

	t.Run("podman never advertises the docker-only name", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); strings.Contains(got, "host.docker.internal") {
			t.Fatalf("podman host mode = %q — host.docker.internal does not resolve inside a podman container", got)
		}
	})

	t.Run("the extra bind still wins over the alias", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
			"ORCHICON_HTTP_EXTRA_BIND", "10.42.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://10.42.0.1:8091" {
			t.Fatalf("podman + extra bind = %q, want http://10.42.0.1:8091", got)
		}
	})

	t.Run("operator alias override", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
			EnvPlaneHostAlias, "plane-host.internal",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://plane-host.internal:8091" {
			t.Fatalf("alias override = %q, want http://plane-host.internal:8091", got)
		}
	})
}

func TestPlanePublicURLPodmanContainerResidency(t *testing.T) {
	t.Run("never emits the docker bridge gateway", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
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
		// Rootless podman: no bridge IP exists. Emitting 172.17.0.1 would hand
		// the run a Docker bridge address that can only time out, so the
		// derivation refuses (empty) and the mint path fails loudly.
		if got != "" {
			t.Fatalf("podman container mode without a resolvable IP = %q, want empty (explicit ORCHICON_PLANE_PUBLIC_URL required)", got)
		}
	})

	t.Run("explicit override satisfies podman container residency", func(t *testing.T) {
		clearPlaneURLEnvForEngine(t,
			EnvRuntimeEngine, "podman",
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_PLANE_PUBLIC_URL", "http://plane.example:9999/",
		)
		if got := planePublicURL(); got != "http://plane.example:9999" {
			t.Fatalf("override = %q, want http://plane.example:9999", got)
		}
	})
}

func TestPlanePublicURLPodmanPerInstance(t *testing.T) {
	// dev and prod share one podman machine and the same host alias; only the
	// PORT tells their planes apart. A hardcoded port would make one
	// instance's workers dial the other instance's plane.
	clearPlaneURLEnvForEngine(t,
		EnvRuntimeEngine, "podman",
		"ORCHICON_HTTP_ADDR", "127.0.0.1:8080",
	)
	dev := planePublicURL()

	clearPlaneURLEnvForEngine(t,
		EnvRuntimeEngine, "podman",
		"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
	)
	prod := planePublicURL()

	if dev != "http://host.containers.internal:8080" {
		t.Fatalf("dev URL = %q, want http://host.containers.internal:8080", dev)
	}
	if prod != "http://host.containers.internal:8091" {
		t.Fatalf("prod URL = %q, want http://host.containers.internal:8091", prod)
	}
	if dev == prod {
		t.Fatalf("dev and prod advertise the same plane URL %q — a run's workers would dial the other instance", dev)
	}
}
