package runtime

// Per-instance derivation of the plane URL handed to a run container
// (lifecycle.go planePublicURL / planeHTTPPort).
//
// THE HAZARD THIS FILE PINS: the two instances (dev, prod) migrate
// independently and can therefore sit on DIFFERENT ports. A hardcoded
// 8080 in either branch makes one host-resident instance advertise the
// other's plane — a dev run's automation worker would dial prod's plane (or
// vice versa). The dial is *rejected* rather than silently accepted (the
// minted credential is instance-bound: its hash lives in the minting
// instance's database — see the cross-instance test in the server package),
// so the failure mode is a broken run, not a wrong-instance write. Both
// must be asserted, not assumed.

import (
	"os"
	"strings"
	"testing"
)

// clearPlaneURLEnv neutralises every input planePublicURL reads, then applies
// the caller's overrides. t.Setenv with "" is equivalent to unset here: the
// function treats an empty value as absent.
func clearPlaneURLEnv(t *testing.T, kv ...string) {
	t.Helper()
	t.Setenv("ORCHICON_PLANE_PUBLIC_URL", "")
	t.Setenv("ORCHICON_CONTAINER_MODE", "")
	t.Setenv("ORCHICON_HTTP_EXTRA_BIND", "")
	t.Setenv("ORCHICON_HTTP_ADDR", "")
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestPlaneHTTPPort(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("ORCHICON_HTTP_ADDR", "")
		if got := planeHTTPPort(); got != 8080 {
			t.Fatalf("planeHTTPPort() = %d, want 8080", got)
		}
	})
	t.Run("this instance's port", func(t *testing.T) {
		t.Setenv("ORCHICON_HTTP_ADDR", "127.0.0.1:8091")
		if got := planeHTTPPort(); got != 8091 {
			t.Fatalf("planeHTTPPort() = %d, want 8091", got)
		}
	})
	t.Run("unparsable falls back to 8080", func(t *testing.T) {
		for _, bad := range []string{"127.0.0.1", "127.0.0.1:http", "127.0.0.1:0", "127.0.0.1:99999"} {
			t.Setenv("ORCHICON_HTTP_ADDR", bad)
			if got := planeHTTPPort(); got != 8080 {
				t.Fatalf("planeHTTPPort(%q) = %d, want 8080 (never a zero/absent port)", bad, got)
			}
		}
	})
}

func TestPlanePublicURLDerivation(t *testing.T) {
	t.Run("override wins in both residency shapes and trims the slash", func(t *testing.T) {
		clearPlaneURLEnv(t,
			"ORCHICON_PLANE_PUBLIC_URL", "http://plane.example:9999/",
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_HTTP_EXTRA_BIND", "10.42.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://plane.example:9999" {
			t.Fatalf("override = %q, want http://plane.example:9999", got)
		}
	})

	t.Run("container residency: own IP plus THIS instance's port", func(t *testing.T) {
		// The containerized plane must keep resolving its own container IP
		// exactly as before — only the port stops being a literal.
		clearPlaneURLEnv(t,
			"ORCHICON_CONTAINER_MODE", "1",
			"ORCHICON_HTTP_ADDR", ":8092",
		)
		want := "http://172.17.0.1:8092" // container-mode guard when the IP is unresolvable
		if ip := containerIPAddress(); ip != "" {
			want = "http://" + ip + ":8092"
		}
		got := planePublicURL()
		if got != want {
			t.Fatalf("container mode = %q, want %q (own IP + this instance's port)", got, want)
		}
		if strings.HasSuffix(got, ":8080") {
			t.Fatalf("container mode still advertises the hardcoded 8080: %q", got)
		}
	})

	t.Run("host residency: the bridge bind host plus this instance's port", func(t *testing.T) {
		clearPlaneURLEnv(t,
			"ORCHICON_HTTP_EXTRA_BIND", "10.42.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); got != "http://10.42.0.1:8091" {
			t.Fatalf("host mode = %q, want http://10.42.0.1:8091", got)
		}
	})

	t.Run("host residency without an extra bind: stable host name + port", func(t *testing.T) {
		// A manual `orchicon serve` (no launcher-provided bind): the stable
		// host name, which resolves because every runtime container is
		// created with --add-host=host.docker.internal:host-gateway.
		clearPlaneURLEnv(t, "ORCHICON_HTTP_ADDR", "127.0.0.1:8091")
		if got := planePublicURL(); got != "http://host.docker.internal:8091" {
			t.Fatalf("host mode without extra bind = %q, want http://host.docker.internal:8091", got)
		}
	})

	t.Run("dev and prod never receive the same URL", func(t *testing.T) {
		// dev: loopback 8080 + bridge 8080. prod: loopback 8091 + bridge 8091.
		// Both instances share the host's docker bridge IP; only the PORT
		// tells them apart, which is exactly what the old literal destroyed.
		clearPlaneURLEnv(t,
			"ORCHICON_HTTP_EXTRA_BIND", "172.17.0.1:8080",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8080",
		)
		dev := planePublicURL()

		clearPlaneURLEnv(t,
			"ORCHICON_HTTP_EXTRA_BIND", "172.17.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		prod := planePublicURL()

		if dev != "http://172.17.0.1:8080" {
			t.Fatalf("dev URL = %q, want http://172.17.0.1:8080", dev)
		}
		if prod != "http://172.17.0.1:8091" {
			t.Fatalf("prod URL = %q, want http://172.17.0.1:8091", prod)
		}
		if dev == prod {
			t.Fatalf("dev and prod advertise the same plane URL %q — a run's workers would dial the other instance", dev)
		}
		if strings.Contains(prod, "8080") {
			t.Fatalf("prod URL %q contains 8080 (the other instance's port)", prod)
		}
	})

	t.Run("host residency ignores the container IP", func(t *testing.T) {
		// A host-resident plane has no container IP; the branch must be
		// selected by ORCHICON_CONTAINER_MODE alone.
		clearPlaneURLEnv(t,
			"ORCHICON_HTTP_EXTRA_BIND", "172.17.0.1:8091",
			"ORCHICON_HTTP_ADDR", "127.0.0.1:8091",
		)
		if got := planePublicURL(); strings.Contains(got, "host.docker.internal") {
			t.Fatalf("host mode with an extra bind must use the bind host, got %q", got)
		}
		if len(os.Getenv("ORCHICON_CONTAINER_MODE")) != 0 {
			t.Fatal("test setup error: container mode leaked into the host-residency case")
		}
	})
}
