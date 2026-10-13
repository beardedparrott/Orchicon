package runtime

// opencodeapi_test.go — THE RUN-START GATE MUST PASS ON BOTH API GENERATIONS.
//
// The regression: serveUsableAt hardcoded the v1 pair (`GET /global/health` + `POST /session`) and
// required 200 from each. Against opencode 2.x the health path still answers — the newer serve keeps
// it as compatibility — but `POST /session` is answered with 405, so the gate returned false for ever.
// Its caller reports "serve did not become usable within 30s", which is the run-start gate for every
// workflow EXECUTION: on a 2.x install executions could not start at all while Ask Orchicon worked,
// because Ask goes through the session client (fixed) and this gate does not.
//
// The status codes below are the ones MEASURED against opencode 2.0.26 and 1.18.34, not invented — the
// 405 is the detail that made the old gate fail rather than merely mis-report.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveSurface stands up a fake serve that answers ONLY the paths of one API generation, with the
// statuses observed on the real binaries.
func serveSurface(t *testing.T, v2 bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v2 {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/info":
				w.WriteHeader(http.StatusOK) // the 2.x readiness path
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				w.WriteHeader(http.StatusCreated) // 2xx, and specifically not 200 — see below
			default:
				// The v1 routes on a 2.x serve: health is kept as compatibility, but the session
				// create is NOT a POST route any more. That 405 is what defeated the old gate.
				if r.URL.Path == "/global/health" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheRunStartGatePassesOnAV1Serve(t *testing.T) {
	srv := serveSurface(t, false)
	if !serveUsableAt(srv.URL, "") {
		t.Error("the gate must pass on a v1 serve — this is every install in the field, and a " +
			"regression here stops every execution")
	}
}

func TestTheRunStartGatePassesOnAV2Serve(t *testing.T) {
	// THE REGRESSION PIN. This is the case that returned false for ever.
	srv := serveSurface(t, true)
	if !serveUsableAt(srv.URL, "") {
		t.Error("the gate must pass on a v2 serve. It did not before this fix: the health path " +
			"answered but the session-create path is not a POST route on the newer surface (405), " +
			"and requiring 200 from it meant executions could never start on a 2.x install")
	}
}

func TestTheGateStillRejectsAServeThatCannotCreateSessions(t *testing.T) {
	// The gate's whole reason for probing a CREATE and not just health: a cold-starting serve answers
	// health before its session machinery is up, so health alone would dispatch into a serve that
	// cannot accept a turn. Widening it to two surfaces must not weaken that.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health answers on both surfaces; NO create ever succeeds.
		if r.Method == http.MethodGet && (r.URL.Path == "/global/health" || r.URL.Path == "/api/info") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	if serveUsableAt(srv.URL, "") {
		t.Error("a serve that never accepts a session create must NOT be reported usable — " +
			"that check is why the probe creates rather than only reading health")
	}
}

func TestAServeThatAnswersNothingIsNotUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if serveUsableAt(srv.URL, "") {
		t.Error("a serve answering 404 everywhere must not be usable")
	}
}

func TestTheCreateAcceptanceIncludes2xxNotJust200(t *testing.T) {
	// A create that succeeds may answer 201. Requiring exactly 200 is how a working serve gets called
	// unusable, so the acceptance is a range and it is pinned as one.
	for _, code := range []int{200, 201, 202, 204} {
		if !opencodeAcceptableCreateStatus(code) {
			t.Errorf("status %d must be accepted as a successful create", code)
		}
	}
	for _, code := range []int{301, 400, 401, 403, 404, 405, 500, 503} {
		if opencodeAcceptableCreateStatus(code) {
			t.Errorf("status %d must NOT be accepted as a successful create", code)
		}
	}
}
