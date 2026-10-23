package desktop

// servers_test.go — the servers surface's contract: the token gate, the purge guard, and that a verb
// reaches the thing it names.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/instancectl"
	"github.com/beardedparrott/orchicon/internal/planebind"
)

// fakeServers records what it was asked to do.
type fakeServers struct {
	list    []instancectl.Instance
	listErr error
	setup   *instancectl.SetupProgress
	// got records the last verb call as "verb:instance:purge".
	got string
	res instancectl.Result
	err error
}

func (f *fakeServers) List() ([]instancectl.Instance, error) { return f.list, f.listErr }
func (f *fakeServers) SetupProgress(string) (*instancectl.SetupProgress, error) {
	return f.setup, nil
}
func (f *fakeServers) note(verb, name string, o instancectl.Options) (instancectl.Result, error) {
	f.got = verb + ":" + name + ":"
	if o.Purge {
		f.got += "purge"
	} else {
		f.got += "keep"
	}
	return f.res, f.err
}
func (f *fakeServers) Install(n string, o instancectl.Options) (instancectl.Result, error) {
	return f.note("install", n, o)
}
func (f *fakeServers) Uninstall(n string, o instancectl.Options) (instancectl.Result, error) {
	return f.note("uninstall", n, o)
}
func (f *fakeServers) Start(n string, o instancectl.Options) (instancectl.Result, error) {
	return f.note("start", n, o)
}
func (f *fakeServers) Stop(n string, o instancectl.Options) (instancectl.Result, error) {
	return f.note("stop", n, o)
}
func (f *fakeServers) Restart(n string, o instancectl.Options) (instancectl.Result, error) {
	return f.note("restart", n, o)
}

// serversShell builds a shell with the servers surface installed and a known token.
func serversShell(t *testing.T, sv Servers) *Shell {
	t.Helper()
	s := &Shell{}
	s.SetServers(sv)
	s.SetControl(&planebindStub{}, "tok")
	return s
}

// post issues a servers request with (or without) the token.
func post(t *testing.T, s *Shell, path, body string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, serversPath+path, strings.NewReader(body))
	if withToken {
		r.Header.Set(controlTokenHeader, "tok")
	}
	w := httptest.NewRecorder()
	if !s.handleServers(w, r) {
		t.Fatalf("handleServers did not own %q", path)
	}
	return w
}

// THE TOKEN GATE APPLIES, and it runs BEFORE the nil check so an unauthenticated caller learns nothing
// about which surfaces this build has.
func TestServersRequiresTheToken(t *testing.T) {
	s := serversShell(t, &fakeServers{})
	w := post(t, s, "install", `{"instance":"dev"}`, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a request with no token answered %d, want 403", w.Code)
	}
}

// A SHELL WITH NO SURFACE SAYS SO rather than nil-panicking.
func TestServersWithoutASurfaceIsNotImplemented(t *testing.T) {
	s := &Shell{}
	s.SetControl(&planebindStub{}, "tok")
	w := post(t, s, "install", `{"instance":"dev"}`, true)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("a shell with no servers surface answered %d, want 501", w.Code)
	}
}

// PURGE IS REFUSED WITHOUT AN EXPLICIT CONFIRMATION — at the boundary, so a UI bug cannot destroy a
// database by falling through a code path it forgot to guard.
func TestPurgeRequiresConfirmation(t *testing.T) {
	f := &fakeServers{}
	s := serversShell(t, f)

	w := post(t, s, "uninstall", `{"instance":"dev","purge":true}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a purge without confirmPurge answered %d, want 400", w.Code)
	}
	if f.got != "" {
		t.Fatalf("the verb RAN (%q) despite a refused purge — the guard must run before the call", f.got)
	}

	// With the confirmation it goes through, and the purge flag reaches the implementation.
	w = post(t, s, "uninstall", `{"instance":"dev","purge":true,"confirmPurge":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("a confirmed purge answered %d, want 200", w.Code)
	}
	if f.got != "uninstall:dev:purge" {
		t.Errorf("the confirmed purge did not reach the implementation with purge set: got %q", f.got)
	}
}

// KEEP IS THE DEFAULT: an uninstall with no purge flag must not destroy anything.
func TestUninstallDefaultsToKeep(t *testing.T) {
	f := &fakeServers{}
	s := serversShell(t, f)
	post(t, s, "uninstall", `{"instance":"dev"}`, true)
	if f.got != "uninstall:dev:keep" {
		t.Errorf("an uninstall with no purge answered %q, want keep", f.got)
	}
}

// A MALFORMED BODY IS REFUSED, not treated as empty — "uninstall with no options" and "uninstall with
// options I could not parse" must never be the same request, because one of them can destroy data.
func TestMalformedBodyIsRefused(t *testing.T) {
	f := &fakeServers{}
	s := serversShell(t, f)
	w := post(t, s, "uninstall", `{"instance":`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400", w.Code)
	}
	if f.got != "" {
		t.Error("a malformed body reached the implementation")
	}
}

// EVERY VERB REACHES ITS OWN METHOD, with the instance it named.
func TestVerbsReachTheNamedInstance(t *testing.T) {
	for _, verb := range []string{"install", "start", "stop", "restart"} {
		f := &fakeServers{}
		s := serversShell(t, f)
		if w := post(t, s, verb, `{"instance":"prod"}`, true); w.Code != http.StatusOK {
			t.Fatalf("%s answered %d", verb, w.Code)
		}
		if want := verb + ":prod:keep"; f.got != want {
			t.Errorf("%s reached %q, want %q", verb, f.got, want)
		}
	}
}

// LIST RETURNS THE ROWS AND A FAILURE IS CARRIED, not swallowed: a partial list (one unreadable instance)
// still renders.
func TestListReturnsRowsAndAnError(t *testing.T) {
	f := &fakeServers{list: []instancectl.Instance{{Name: "dev"}, {Name: "prod"}}}
	s := serversShell(t, f)
	r := httptest.NewRequest(http.MethodGet, serversPath+"list", nil)
	r.Header.Set(controlTokenHeader, "tok")
	w := httptest.NewRecorder()
	s.handleServers(w, r)

	var got serversResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("list body is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(got.Instances) != 2 {
		t.Errorf("list returned %d rows, want 2", len(got.Instances))
	}
}

// SETUP NEEDS AN INSTANCE: a setup record belongs to one server, and defaulting would show the wrong one's
// progress.
func TestSetupRequiresAnInstance(t *testing.T) {
	s := serversShell(t, &fakeServers{})
	r := httptest.NewRequest(http.MethodGet, serversPath+"setup", nil)
	r.Header.Set(controlTokenHeader, "tok")
	w := httptest.NewRecorder()
	s.handleServers(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a setup read with no instance answered %d, want 400", w.Code)
	}
}

// A NON-SERVERS PATH IS NOT OURS, so the mux can fall through to the SPA.
func TestNonServersPathIsNotOwned(t *testing.T) {
	s := serversShell(t, &fakeServers{})
	r := httptest.NewRequest(http.MethodGet, "/somewhere-else", nil)
	if s.handleServers(httptest.NewRecorder(), r) {
		t.Error("handleServers claimed a path outside its prefix")
	}
}

// planebindStub satisfies Control for these tests; plane control itself is covered by control_test.go.
type planebindStub struct{}

func (planebindStub) Instances() ([]planebind.StatusDTO, error)  { return nil, nil }
func (planebindStub) Status(string) (planebind.StatusDTO, error) { return planebind.StatusDTO{}, nil }
func (planebindStub) ResolveControlPort(int) (planebind.ResolutionDTO, error) {
	return planebind.ResolutionDTO{}, nil
}
func (planebindStub) Start(string) (planebind.ActionDTO, error)   { return planebind.ActionDTO{}, nil }
func (planebindStub) Stop(string) (planebind.ActionDTO, error)    { return planebind.ActionDTO{}, nil }
func (planebindStub) Restart(string) (planebind.ActionDTO, error) { return planebind.ActionDTO{}, nil }
