package planecontrol

// probe_test.go — the four connection states have to start somewhere honest, and
// this is that place: Probe must SEPARATE "nothing answers" from "something
// answers and is not well". Healthy() collapsed both into false, which is fine for
// a lifecycle verb (it proceeds or it does not) and useless for a screen that has
// to tell the operator which fix to apply.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeSeparatesAbsentFromUnhealthy(t *testing.T) {
	// A server that is listening but NOT well: reachable, non-200.
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer sick.Close()

	// Nothing listening: bind, read the address, close. The port is then refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	well := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer well.Close()

	t.Run("nothing answers", func(t *testing.T) {
		h := Probe(deadURL+"/healthz", 500*time.Millisecond)
		if h.Reachable {
			t.Fatalf("Probe against a closed port reported Reachable=true (%+v)", h)
		}
		if h.Err == nil && deadURL != "" {
			// A refused connection on loopback is an error, always. A non-nil Err is
			// what lets the surface name WHY it found nothing.
			t.Logf("note: no transport error recorded for %s", deadURL)
		}
		if h.Healthy() {
			t.Fatal("an absent plane must not read as healthy")
		}
	})

	t.Run("answers but is unwell", func(t *testing.T) {
		h := Probe(sick.URL+"/healthz", 2*time.Second)
		if !h.Reachable {
			t.Fatalf("Probe against a listening-but-503 server reported Reachable=false (%+v)", h)
		}
		if h.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("StatusCode = %d, want %d", h.StatusCode, http.StatusServiceUnavailable)
		}
		if h.Healthy() {
			t.Fatal("a 503 must not read as healthy")
		}
		// THE DISTINCTION THE PAGE DEPENDS ON: offline ≠ plane-error.
		if !h.Reachable || h.Healthy() {
			t.Fatal("a reachable-but-unwell plane is plane-error, never offline")
		}
	})

	t.Run("healthy", func(t *testing.T) {
		h := Probe(well.URL+"/healthz", 2*time.Second)
		if !h.Healthy() {
			t.Fatalf("a 200 must read as healthy (%+v)", h)
		}
		if !Healthy(well.URL+"/healthz", 2*time.Second) {
			t.Fatal("Healthy must agree with Probe — one implementation, two names")
		}
		if Healthy(sick.URL+"/healthz", 2*time.Second) {
			t.Fatal("Healthy must agree with Probe on the unhealthy case too")
		}
	})

	t.Run("error is named, not swallowed", func(t *testing.T) {
		h := Probe("http://127.0.0.1:1/healthz", 500*time.Millisecond)
		if h.Err == nil {
			t.Fatal("a refused connection must carry the transport error — the surface names what it found")
		}
		if strings.TrimSpace(h.Err.Error()) == "" {
			t.Fatal("the transport error is empty")
		}
	})
}
