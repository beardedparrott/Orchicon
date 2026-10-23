package desktop

// surfaces_test.go — every one of the shell's endpoints must be REACHABLE, whatever order the table is in.
//
// The reported failure was:
//
//	Could not read this machine's servers: desktop shell answered 404
//
// because controlPath ("/__orchicon_desktop/") is a PREFIX of serversPath ("/__orchicon_desktop/servers/") and
// the control surface was dispatched first. It claimed the request, missed every case in its switch, and 404ed.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/instancectl"
)

// surfacesShell builds a shell with BOTH surfaces installed and a known token.
func surfacesShell(t *testing.T, sv Servers) *Shell {
	t.Helper()
	s := &Shell{}
	s.SetControl(&planebindStub{}, "tok")
	if sv != nil {
		s.SetServers(sv)
	}
	return s
}

// A SERVERS REQUEST MUST BE ANSWERED BY THE SERVERS SURFACE, not by the control surface's 404.
//
// This is the regression, asserted through the real dispatcher: the 404 came from handleControl claiming a
// path it does not implement.
func TestServersRequestsAreNotSwallowedByControl(t *testing.T) {
	s := surfacesShell(t, &fakeServers{list: []instancectl.Instance{{Name: "dev"}}})

	r := httptest.NewRequest(http.MethodGet, serversPath+"list", nil)
	r.Header.Set(controlTokenHeader, "tok")
	w := httptest.NewRecorder()
	if !s.handleOwnEndpoint(w, r) {
		t.Fatal("no surface owned a servers request")
	}
	if w.Code == http.StatusNotFound {
		t.Fatalf("a servers request answered 404 — the control surface claimed it because controlPath is a "+
			"prefix of serversPath.\n  body: %s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("servers list answered %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"dev"`) {
		t.Errorf("the list did not return the server: %s", w.Body.String())
	}
}

// AND NO PREFIX MAY SHADOW ANOTHER, at any future ordering.
//
// The table is sorted longest-first, so this holds by construction — but asserting it on the SORTED output is
// what makes the property visible, and it stays true if someone adds a third surface.
func TestNoOwnSurfaceShadowsAnother(t *testing.T) {
	s := surfacesShell(t, &fakeServers{})
	list := s.surfaces()
	if len(list) < 2 {
		t.Fatalf("expected at least two surfaces, got %d", len(list))
	}
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			// list[j] is considered AFTER list[i]; if list[j]'s prefix starts with list[i]'s, it can never
			// be reached.
			if strings.HasPrefix(list[j].prefix, list[i].prefix) {
				t.Errorf("%q is dispatched after %q, but %q starts with it — every request to %q would be "+
					"claimed by %q first and answered 404.\n  sorted order: %v",
					list[j].prefix, list[i].prefix, list[j].prefix, list[j].prefix, list[i].prefix, prefixes(list))
			}
		}
	}
}

func prefixes(list []ownSurface) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.prefix)
	}
	return out
}

// THE CONTROL SURFACE STILL WORKS, so the fix did not trade one surface for the other.
func TestControlSurfaceStillReachable(t *testing.T) {
	s := surfacesShell(t, &fakeServers{})
	r := httptest.NewRequest(http.MethodGet, controlPath+"instances", nil)
	r.Header.Set(controlTokenHeader, "tok")
	w := httptest.NewRecorder()
	if !s.handleOwnEndpoint(w, r) {
		t.Fatal("no surface owned a control request")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("control instances answered %d, want 200", w.Code)
	}
}

// A PATH THAT BELONGS TO NEITHER IS NOT CLAIMED, so the mux can fall through to the plane and the SPA.
func TestUnknownPathIsNotClaimed(t *testing.T) {
	s := surfacesShell(t, &fakeServers{})
	r := httptest.NewRequest(http.MethodGet, "/somewhere-else", nil)
	if s.handleOwnEndpoint(httptest.NewRecorder(), r) {
		t.Error("the dispatcher claimed a path outside every surface")
	}
}
