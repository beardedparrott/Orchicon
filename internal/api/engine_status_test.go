package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// testEngineProbe is a Probe that can only see what the test gives it — the same posture as
// internal/containerengine's own tests, so no case here needs podman, docker, or a network.
func testEngineProbe(goos string, env map[string]string, bins map[string]string, run func(bin string, args []string) (string, error)) containerengine.Probe {
	return containerengine.Probe{
		LookPath: func(name string) (string, error) {
			if p, ok := bins[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Run:    func(_ context.Context, bin string, args ...string) (string, error) { return run(bin, args) },
		Stat:   func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		GOOS:   goos,
		Getenv: func(k string) string { return env[k] },
		IsRoot: func() bool { return false },
		Sleep:  func(time.Duration) {},
	}
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) containerengine.Status {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status code %d", rec.Code)
	}
	var st containerengine.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode /enginez body %q: %v", rec.Body.String(), err)
	}
	return st
}

// TestEnginezReadyAndUnavailable is AC5's server half: the three states the UI renders, with
// the reason spelled out when containers are unavailable.
func TestEnginezReadyAndUnavailable(t *testing.T) {
	ready := testEngineProbe("linux", nil, map[string]string{"podman": "/fake/podman"}, func(_ string, args []string) (string, error) {
		if args[0] == "--version" {
			return "podman version 5.2.1", nil
		}
		return "amd64", nil
	})
	rec := httptest.NewRecorder()
	newEngineStatusCache(time.Minute, ready).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/enginez", nil))
	st := decodeStatus(t, rec)
	if !st.Available || st.State != containerengine.StateReady || st.Engine != containerengine.EnginePodman || st.Version != "5.2.1" {
		t.Fatalf("ready body: %+v", st)
	}

	none := testEngineProbe("linux", nil, nil, func(string, []string) (string, error) { return "", nil })
	rec = httptest.NewRecorder()
	newEngineStatusCache(time.Minute, none).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/enginez", nil))
	st = decodeStatus(t, rec)
	if st.Available || st.State != containerengine.StateUnavailable || st.Reason == "" {
		t.Fatalf("unavailable body must carry a reason: %+v", st)
	}
}

// TestEnginezProvisioning is the third state: the installer's marker file makes "being
// installed" truthful instead of a spinner that may be a lie.
func TestEnginezProvisioning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, containerengine.ProvisionMarkerFile), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := testEngineProbe("linux", map[string]string{"ORCHICON_DATA_DIR": dir}, nil, func(string, []string) (string, error) { return "", nil })
	p.Stat = os.Stat
	rec := httptest.NewRecorder()
	newEngineStatusCache(time.Minute, p).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/enginez", nil))
	st := decodeStatus(t, rec)
	if st.State != containerengine.StateProvisioning || st.Available {
		t.Fatalf("provisioning body: %+v", st)
	}
}

// TestEnginezMemo pins the cost: a second poll inside the window must NOT re-run the probe
// (that process would fork `podman` per request from a polling browser), and ?refresh=1 must.
func TestEnginezMemo(t *testing.T) {
	calls := 0
	p := testEngineProbe("linux", nil, map[string]string{"podman": "/fake/podman"}, func(_ string, args []string) (string, error) {
		calls++
		if args[0] == "--version" {
			return "podman version 5.2.1", nil
		}
		return "amd64", nil
	})
	cache := newEngineStatusCache(time.Minute, p)

	cache.status(context.Background(), false)
	first := calls
	cache.status(context.Background(), false)
	if calls != first {
		t.Fatalf("a second poll inside the memo window re-ran the probe (%d → %d calls)", first, calls)
	}
	cache.status(context.Background(), true)
	if calls == first {
		t.Fatal("?refresh=1 must force a fresh probe")
	}
}
