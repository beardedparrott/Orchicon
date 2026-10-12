package opencode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSelectAgentRouteIsSurfaceSpecific pins that the agent route exists on the
// surface that NEEDS it and nowhere else.
//
// On v2 an agent's `system` is the only channel that actually governs the model,
// so a missing route would mean the worker's prompt silently never arrives. On v1
// there is no agent involved at all — the prompt rides the per-message `system`
// field — so a route there would be a request against an endpoint that does not
// exist.
func TestSelectAgentRouteIsSurfaceSpecific(t *testing.T) {
	if shapeV1.setAgentPath != nil {
		t.Error("v1 has no agent route — SelectAgent must be a no-op there")
	}
	if shapeV2.setAgentPath == nil {
		t.Fatal("v2 must carry the agent route: the agent's system is the only channel that governs the model there")
	}
	if got, want := shapeV2.setAgentPath("ses_abc"), "/api/session/ses_abc/agent"; got != want {
		t.Errorf("setAgentPath = %q, want %q", got, want)
	}
}

// TestSelectAgentIsANoOpOnV1 pins the no-op — and pins it by CONSEQUENCE rather
// than by a flag: the client is pointed at a dead address, so any attempt to send
// a request would fail. A nil error therefore proves no request was made, which is
// the behaviour that keeps a 1.x execution byte-identical.
func TestSelectAgentIsANoOpOnV1(t *testing.T) {
	c := NewSessionClient("http://127.0.0.1:1", "pw", "")
	c.shape = shapeV1
	if err := c.SelectAgent(context.Background(), "ses_x", "orchicon-worker-01m1"); err != nil {
		t.Errorf("SelectAgent must be a no-op on v1, got %v", err)
	}
	// An empty name is a no-op on EVERY surface: the name is derived from a worker
	// id the caller may not have, and a session left on the default agent is a
	// degraded run rather than a failure.
	v2 := NewSessionClient("http://127.0.0.1:1", "pw", "")
	v2.shape = shapeV2
	if err := v2.SelectAgent(context.Background(), "ses_x", ""); err != nil {
		t.Errorf("an empty agent name must be a no-op, got %v", err)
	}
}

// TestSelectAgentPostsTheAgentName pins the request itself: the route v2 documents
// (session.switchAgent) and the field it reads. A wrong field name here is the
// same silent failure this whole change exists to remove — the request would be
// accepted and the agent would not change.
func TestSelectAgentPostsTheAgentName(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewSessionClient(srv.URL, "pw", "")
	c.shape = shapeV2
	if err := c.SelectAgent(context.Background(), "ses_abc", "orchicon-worker-01m1"); err != nil {
		t.Fatalf("SelectAgent: %v", err)
	}
	if gotPath != "/api/session/ses_abc/agent" {
		t.Errorf("posted to %q, want the v2 agent route", gotPath)
	}
	if !strings.Contains(gotBody, `"agent":"orchicon-worker-01m1"`) {
		t.Errorf("body = %q, want it to carry the agent name", gotBody)
	}
}
