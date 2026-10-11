package opencode

// apisurface_test.go — the two API surfaces, and the fact that the client actually uses the right one.
//
// opencode 2.x moved every session endpoint under /api and renamed several operations
// (V2_HTTP_API_AUDIT.md). The client used to write those routes as literals, so supporting both
// versions means the routes had to become data (apisurface.go) — and a data change is only worth
// anything if something asserts the client really follows it. That is what the end-to-end cases here
// do: they run the REAL client against a recording server and check which path arrived.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIShapeForVersion(t *testing.T) {
	cases := []struct {
		version string
		want    string // shape name
		parsed  bool
		why     string
	}{
		{"1.18.34", "v1", true, "the version on the development host"},
		{"2.0.26", "v2", true, "the version reported from WSL, and the one that broke model discovery"},
		{"v2.0.26", "v2", true, "a leading v is accepted, as `opencode --version` has used both"},
		{"2.1.0", "v2", true, "the break is at the MAJOR; a later minor stays v2"},
		{"10.0.0", "v2", true, "a two-digit major must not be mis-parsed as 1"},
		{"0.9.1", "v1", true, "a pre-1.0 version is still the v1 surface"},
		{"", "v1", false, "an empty version must NOT be guessed at — v1 keeps behaviour unchanged"},
		{"not-a-version", "v1", false, "unparseable input falls back to v1 and says so via the bool"},
	}
	for _, tc := range cases {
		got, parsed := apiShapeForVersion(tc.version)
		if got.name != tc.want || parsed != tc.parsed {
			t.Errorf("apiShapeForVersion(%q) = %q/%v, want %q/%v — %s",
				tc.version, got.name, parsed, tc.want, tc.parsed, tc.why)
		}
	}
}

// TestV2PathsMatchTheAudit is the mapping in executable form. Each expectation is the path from
// V2_HTTP_API_AUDIT.md's endpoint table, so a silent edit to a v2 route fails here with the audit
// quoted next to it rather than surfacing as a 404 in a worker.
func TestV2PathsMatchTheAudit(t *testing.T) {
	cases := []struct {
		got, want, audit string
	}{
		{shapeV2.healthPath, "/api/info", "001–002 GET /api/info (server.info) — replaces health+server"},
		{shapeV2.sessionsPath(), "/api/session", "048 POST /api/session (session.create)"},
		{shapeV2.sessionPath("ses_1"), "/api/session/ses_1", "051 GET /api/session/{sessionID} (session.get)"},
		{shapeV2.promptPath("ses_1"), "/api/session/ses_1/prompt", "059 POST /api/session/{sessionID}/prompt"},
		{shapeV2.compactPath("ses_1"), "/api/session/ses_1/compact", "064 POST /api/session/{sessionID}/compact"},
		{shapeV2.abortPath("ses_1"), "/api/session/ses_1/interrupt", "067 POST /api/session/{sessionID}/interrupt"},
		{shapeV2.permissionReplyPath("ses_1", "per_1"), "/api/session/ses_1/permission/per_1/reply",
			"100 POST /api/session/{sessionID}/permission/{requestID}/reply"},
		{shapeV2.eventPath(), "/api/event", "140 GET /api/event (event.subscribe)"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("v2 path = %q, want %q\n  from the audit: %s", tc.got, tc.want, tc.audit)
		}
	}
}

// TestV1PathsAreUnchanged is the other half: supporting v2 must not move a single v1 route, because
// every install in the field runs v1 and a moved route is a broken worker.
func TestV1PathsAreUnchanged(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{shapeV1.healthPath, "/global/health"},
		{shapeV1.sessionsPath(), "/session"},
		{shapeV1.sessionPath("ses_1"), "/session/ses_1"},
		{shapeV1.promptPath("ses_1"), "/session/ses_1/prompt_async"},
		{shapeV1.compactPath("ses_1"), "/session/ses_1/summarize"},
		{shapeV1.abortPath("ses_1"), "/session/ses_1/abort"},
		{shapeV1.permissionReplyPath("ses_1", "per_1"), "/session/ses_1/permissions/per_1"},
		{shapeV1.eventPath(), "/event"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("v1 path = %q, want %q — the v1 surface must be byte-identical to before",
				tc.got, tc.want)
		}
	}
}

// recordedRequest is what the server saw.
type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

// recordingServe answers like a permissive opencode serve and records what it was asked.
func recordingServe(t *testing.T, seen *[]recordedRequest) *httptest.Server {
	t.Helper()
	// EVERY PLAUSIBLE PATH ANSWERS 200. The point is not to emulate a serve but to observe the
	// request, and a 404 would make the client fail before the assertion could run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{method: r.Method, path: r.URL.Path}
		if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &rec.body)
		}
		*seen = append(*seen, rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ses_test","sessionID":"ses_test"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheClientUsesTheShapeItWasGiven(t *testing.T) {
	cases := []struct {
		name      string
		shape     apiShape
		wantPath  string
		wantShape string
	}{
		{"the default (v1)", shapeV1, "/global/health", "v1"},
		{"opencode 2.x", shapeV2, "/api/info", "v2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []recordedRequest
			srv := recordingServe(t, &seen)

			c := NewSessionClient(srv.URL, "", "")
			// The default must be v1 WITHOUT being told: a client that never detects anything has
			// to behave exactly as before.
			if got := c.APIShapeName(); got != "v1" {
				t.Fatalf("a fresh client defaults to %q, want v1", got)
			}
			if tc.wantShape != "v1" {
				c.setAPIShape(tc.shape)
			}
			if got := c.APIShapeName(); got != tc.wantShape {
				t.Fatalf("shape = %q, want %q", got, tc.wantShape)
			}

			if !c.Healthy(context.Background()) {
				t.Fatal("Healthy reported false against a server answering 200")
			}
			if len(seen) != 1 || seen[0].path != tc.wantPath {
				t.Fatalf("health probe asked for %+v, want GET %s — this is the assertion the whole "+
					"file exists for: the route must follow the shape", seen, tc.wantPath)
			}

			// And a session-scoped route, which is where the suffixes differ.
			seen = nil
			if _, err := c.CreateSession(context.Background(), "t"); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			wantSession := "/session"
			if tc.wantShape == "v2" {
				wantSession = "/api/session"
			}
			if len(seen) != 1 || seen[0].path != wantSession {
				t.Errorf("CreateSession asked for %+v, want POST %s", seen, wantSession)
			}
		})
	}
}

func TestThePermissionReplyBodyKeyFollowsTheShape(t *testing.T) {
	// The audit's one documented BODY rename for this endpoint: v1 takes `response`, v2 `decision`.
	// Asserted on the wire rather than on the constant, because a rename that never reaches the
	// request is what a wrong constant would look like.
	cases := []struct {
		shape  apiShape
		key    string
		absent string
	}{
		{shapeV1, "response", "decision"},
		{shapeV2, "decision", "response"},
	}
	for _, tc := range cases {
		t.Run(tc.shape.name, func(t *testing.T) {
			var seen []recordedRequest
			srv := recordingServe(t, &seen)

			c := NewSessionClient(srv.URL, "", "")
			c.setAPIShape(tc.shape)
			if err := c.ReplyPermissionDecision(context.Background(), "ses_1", "per_1", "allow"); err != nil {
				t.Fatalf("ReplyPermissionDecision: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("expected one request, saw %+v", seen)
			}
			if seen[0].body[tc.key] != "allow" {
				t.Errorf("body %v must carry the decision under %q", seen[0].body, tc.key)
			}
			if _, present := seen[0].body[tc.absent]; present {
				t.Errorf("body %v must NOT use the other version's key %q", seen[0].body, tc.absent)
			}
			wantPath := "/session/ses_1/permissions/per_1"
			if tc.shape.name == "v2" {
				wantPath = "/api/session/ses_1/permission/per_1/reply"
			}
			if seen[0].path != wantPath {
				t.Errorf("path = %q, want %q", seen[0].path, wantPath)
			}
		})
	}
}

func TestDetectAPIShapeReadsTheVersionStub(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")

	cases := []struct {
		name    string
		output  string
		exit    string
		want    string
		wantOK  bool
		comment string
	}{
		{"a 2.x stub", "2.0.26", "0", "v2", true, "the reported WSL version"},
		{"a 1.x stub", "1.18.34", "0", "v1", true, "the development host version"},
		{"a banner before the number", "opencode 2.0.26 (linux/amd64)", "0", "v2", true,
			"real CLIs print more than a bare number; the first parseable token wins"},
		{"a failing binary", "", "1", "v1", false, "a probe that cannot run must fall back, not guess"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\n"
			if tc.output != "" {
				script += "echo '" + tc.output + "'\n"
			}
			script += "exit " + tc.exit + "\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}

			shape, _, ok := detectAPIShape(context.Background(), bin)
			if shape.name != tc.want || ok != tc.wantOK {
				t.Errorf("detectAPIShape = %q/%v, want %q/%v — %s",
					shape.name, ok, tc.want, tc.wantOK, tc.comment)
			}
		})
	}
}

func TestDetectAPIShapeWithNoBinaryFallsBack(t *testing.T) {
	// An empty path must not spawn anything or panic, and must land on v1.
	shape, _, ok := detectAPIShape(context.Background(), "")
	if shape.name != "v1" || ok {
		t.Errorf("detectAPIShape(\"\") = %q/%v, want v1/false", shape.name, ok)
	}
	if strings.TrimSpace(shape.healthPath) == "" {
		t.Error("the fallback shape must still be usable")
	}
}
