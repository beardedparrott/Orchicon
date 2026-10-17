package runtime

// hostfp_engine_test.go — THE POOL KEY'S ENGINE TERM (ADR-0013 D7.1/D7.2).
//
// The warm pool's key is "environment AND host inputs". After the engine swap
// that must ALSO carry the engine: a container created by podman is not a
// container the docker daemon can hand out, a podman UPGRADE can change
// create/removal semantics under a warm container, and an engine whose mount
// SHAPE changes (the `,z` relabel, ADR-0013 D3.2) must invalidate every warm
// container created without it — a running container's mounts cannot be
// changed in place.
//
// The fingerprint is folded through Daemon.hostInputsFingerprint (daemon.go), so
// poolEnvKey keeps its 2-argument signature: dozens of call sites (pool_test.go,
// hostfp_test.go) stay valid and there is exactly ONE place a host input can be
// added or forgotten.
//
// Every case here is HERMETIC: the engine version comes from the engineVersionFn
// seam, never a real engine exec, so the suite is deterministic on a host with no
// container engine installed at all.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// engineTestDaemon builds a Daemon with a scratch ExePath (so the binary term is
// stable) and no HostHome, isolating the engine term under test.
func engineTestDaemon(t *testing.T, eng Engine, version string) *Daemon {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "orchicon")
	if err := os.WriteFile(exe, []byte("engine-fp test binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		Log:       slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		ExePath:   exe,
		Engine:    eng,
		ghTokenFn: func() string { return "" },
	}
	if version != "" {
		v := version
		d.engineVersionFn = func() string { return v }
	}
	return d
}

// discardWriter is a tiny io.Writer so these tests need no io import clutter.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestEngineFingerprintChangesWithEngineAndVersion is AC 3's engine half: the
// pool key must change when the ENGINE changes and when its VERSION changes, and
// must stay stable when neither does.
func TestEngineFingerprintChangesWithEngineAndVersion(t *testing.T) {
	podman5 := engineTestDaemon(t, Engine{Name: "podman"}, "5.0.0")
	podman5Again := engineTestDaemon(t, Engine{Name: "podman"}, "5.0.0")
	podman50 := engineTestDaemon(t, Engine{Name: "podman"}, "5.0.0")
	podman51 := engineTestDaemon(t, Engine{Name: "podman"}, "5.1.0")
	docker5 := engineTestDaemon(t, Engine{Name: "docker"}, "5.0.0")

	base := podman5.engineInputFingerprint()
	if !strings.HasPrefix(base, "engine=podman@5.0.0") {
		t.Fatalf("engine term must name the engine and its version, got %q", base)
	}
	if !strings.Contains(base, "shape=") {
		t.Fatalf("engine term must carry the create-time shape, got %q", base)
	}
	if got := podman50.engineInputFingerprint(); got != base {
		t.Fatalf("the same engine at the same version must key identically: %q vs %q", base, got)
	}
	if got := podman5Again.engineInputFingerprint(); got != base {
		t.Fatalf("a restart must resolve the same engine term: %q vs %q", base, got)
	}
	if got := podman51.engineInputFingerprint(); got == base {
		t.Fatalf("an ENGINE UPGRADE must change the pool key (got the same %q) — a podman upgrade can change "+
			"create/removal semantics under a warm container", base)
	}
	if got := docker5.engineInputFingerprint(); got == base {
		t.Fatalf("an ENGINE SWITCH must change the pool key (docker got the podman term %q)", base)
	}

	// The relation must survive the trip through poolEnvKey, because THAT is
	// what the pool actually keys on.
	req := CreateRequest{Image: "img:v1"}
	if poolEnvKey(req, podman50.hostInputsFingerprint()) != poolEnvKey(req, podman5.hostInputsFingerprint()) {
		t.Fatalf("two identical environments must produce one pool key")
	}
	if poolEnvKey(req, podman51.hostInputsFingerprint()) == poolEnvKey(req, podman5.hostInputsFingerprint()) {
		t.Fatalf("a version change must produce a DIFFERENT pool key")
	}
	if poolEnvKey(req, docker5.hostInputsFingerprint()) == poolEnvKey(req, podman5.hostInputsFingerprint()) {
		t.Fatalf("an engine switch must produce a DIFFERENT pool key")
	}
}

// TestEngineFingerprintShapeTerm pins the shape term: it is derived from the
// Engine value alone (no exec), it differs per engine family, and it names the
// relabel marker that ADR-0013 D3.2 will flip to "z".
func TestEngineFingerprintShapeTerm(t *testing.T) {
	podman := Engine{Name: "podman"}
	docker := Engine{Name: "docker"}

	ps, ds := podman.Shape(), docker.Shape()
	if ps == ds {
		t.Fatalf("podman and docker create structurally different containers; shapes must differ (both %q)", ps)
	}
	for _, shape := range []string{ps, ds} {
		if !strings.Contains(shape, "relabel="+shapeRelabel) {
			t.Fatalf("shape %q must name the relabel marker (%q) — flipping it is what invalidates "+
				"warm containers created under the old mount shape (D7.2)", shape, shapeRelabel)
		}
		if !strings.Contains(shape, "userns=") {
			t.Fatalf("shape %q must name the namespace mapping", shape)
		}
	}
	if !strings.Contains(ps, "keep-id") {
		t.Fatalf("podman's shape must record --userns=keep-id (the host-uid mapping), got %q", ps)
	}
	if strings.Contains(ds, "keep-id") {
		t.Fatalf("docker's shape must NOT claim a keep-id mapping it never emits, got %q", ds)
	}
}

// TestEngineFingerprintNeverEmpty pins the fail-safe direction: an engine whose
// version cannot be resolved must NOT fold an empty term away (that is the exact
// silent-degradation the fingerprint exists to prevent). It resolves to the
// literal "unknown", which is still an engine term.
func TestEngineFingerprintNeverEmpty(t *testing.T) {
	d := engineTestDaemon(t, Engine{Name: "podman", Bin: "/nonexistent/orchicon-no-such-engine-binary"}, "")
	got := d.engineInputFingerprint()
	if !strings.Contains(got, "engine=podman@unknown") {
		t.Fatalf("an unresolvable engine version must resolve to the literal \"unknown\", got %q", got)
	}
	if strings.Contains(got, "@|") || strings.HasSuffix(got, "@") {
		t.Fatalf("the version term degraded to empty (%q) — it would fold away and stop covering the engine", got)
	}
	// A daemon whose seam returns "" is the same case, through the other door.
	blank := engineTestDaemon(t, Engine{Name: "docker"}, "  ")
	if b := blank.engineInputFingerprint(); !strings.Contains(b, "engine=docker@unknown") {
		t.Fatalf("a blank version seam must resolve to \"unknown\", got %q", b)
	}
}

// TestEngineVersionResolvedOnce pins the sync.Once: the pool key is computed on
// every checkout and every reset, so a version probe per call would exec the
// engine on the dispatch path.
func TestEngineVersionResolvedOnce(t *testing.T) {
	d := engineTestDaemon(t, Engine{Name: "podman"}, "")
	var calls int32
	d.engineVersionFn = func() string {
		atomic.AddInt32(&calls, 1)
		return "5.0.0"
	}
	first := d.hostInputsFingerprint()
	for i := 0; i < 5; i++ {
		if got := d.hostInputsFingerprint(); got != first {
			t.Fatalf("call %d: the host-inputs fingerprint must be stable (got %q, want %q)", i, got, first)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("the engine version was resolved %d times; it must be resolved ONCE per daemon process "+
			"(it is on the checkout path)", n)
	}
}
