package desktop

// servers.go — the SERVERS surface: the four verbs plane control did not have.
//
// /manage-server already answers start/stop/restart/status/instances (control.go). This adds what a person
// managing Orchicon on their own machine actually needs beyond that: LIST the servers richly, INSTALL one,
// UNINSTALL one, and watch a setup in progress.
//
// IT IS ADDITIVE ON PURPOSE. The existing endpoints are exercised by the shipped page and by their own
// guards; re-pointing them at a new contract would be a rewrite of working code for no gain, and "bug free
// first round" is mostly a promise about not disturbing what already works. So this is a second, disjoint
// set of paths — and both delegate to the same single implementation underneath (internal/instancectl,
// which is the thin client of the CLI), so the two cannot disagree about what an instance IS.
//
// THE TOKEN IS THE SAME ONE. These endpoints can uninstall an instance and destroy data, which is strictly
// more dangerous than bouncing a plane, so they are gated by exactly the same per-run token and the same
// loopback-only origin. There is deliberately no weaker path.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/beardedparrott/orchicon/internal/instancectl"
)

// Servers is the shell's view of instance management. internal/instancectl.Service satisfies it exactly,
// and a test drives the endpoints through a fake — the same shape Control already uses.
type Servers interface {
	List() ([]instancectl.Instance, error)
	Install(name string, opts instancectl.Options) (instancectl.Result, error)
	Uninstall(name string, opts instancectl.Options) (instancectl.Result, error)
	Start(name string, opts instancectl.Options) (instancectl.Result, error)
	Stop(name string, opts instancectl.Options) (instancectl.Result, error)
	Restart(name string, opts instancectl.Options) (instancectl.Result, error)
	SetupProgress(name string) (*instancectl.SetupProgress, error)
}

// SetServers installs the servers surface. Called once at startup, before the window opens.
//
// nil is accepted and makes every servers endpoint answer 501, so a shell without the surface (a test, or
// a build that predates it) says so instead of nil-panicking on a request.
func (s *Shell) SetServers(sv Servers) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers = sv
}

// serversPath is the prefix for the servers surface. Disjoint from controlPath so a control route and a
// servers route can never be confused, and namespaced the same way so neither can be mistaken for a plane
// route.
const serversPath = "/__orchicon_desktop/servers/"

// handleServers answers the servers surface. It returns false when the path is not ours, so the caller can
// fall through — exactly like handleControl.
func (s *Shell) handleServers(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, serversPath) {
		return false
	}
	// SAME GATE AS CONTROL, and it runs BEFORE the nil check so an unauthenticated caller learns nothing
	// about which surfaces this build has.
	if !s.controlTokenOK(r) {
		writeJSON(w, http.StatusForbidden, errorBody{"this endpoint needs the desktop app's own token"})
		return true
	}
	if s.servers == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{"this build has no server-management surface"})
		return true
	}

	route := strings.TrimPrefix(r.URL.Path, serversPath)
	switch route {
	case "list":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{"GET required"})
			return true
		}
		got, err := s.servers.List()
		writeJSON(w, http.StatusOK, serversResult{Instances: got, Error: errText(err)})
		return true

	case "setup":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{"GET required"})
			return true
		}
		name := r.URL.Query().Get("instance")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, errorBody{"instance is required: a setup record belongs to one server"})
			return true
		}
		got, err := s.servers.SetupProgress(name)
		writeJSON(w, http.StatusOK, setupResult{Setup: got, Error: errText(err)})
		return true
	}

	// The verb endpoints all take the same body and differ only in which method they call.
	var verb func(name string, opts instancectl.Options) (instancectl.Result, error)
	switch route {
	case "install":
		verb = s.servers.Install
	case "uninstall":
		verb = s.servers.Uninstall
	case "start":
		verb = s.servers.Start
	case "stop":
		verb = s.servers.Stop
	case "restart":
		verb = s.servers.Restart
	default:
		http.NotFound(w, r)
		return true
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"POST required"})
		return true
	}

	var body serversRequest
	// A malformed body is refused rather than treated as empty: "uninstall with no options" and "uninstall
	// with options I could not parse" must never be the same request, because one of them can destroy data.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"body must be JSON: " + err.Error()})
		return true
	}
	// PURGE IS REFUSED WITHOUT AN EXPLICIT CONFIRMATION, here as well as in the CLI.
	//
	// The CLI's --purge IS its confirmation; on this surface a checkbox in a dialog is not, so the caller
	// must say so in the request. Encoding it at the boundary means a UI bug cannot destroy a database by
	// falling through a code path it forgot to guard.
	if body.Purge && !body.ConfirmPurge {
		writeJSON(w, http.StatusBadRequest, errorBody{
			"destroying data requires confirmPurge: the request must say that it means to destroy the database and the secrets key"})
		return true
	}

	got, err := verb(body.Instance, instancectl.Options{
		Purge:         body.Purge,
		RuntimeImages: body.RuntimeImages,
		Ports:         body.Ports,
	})
	writeJSON(w, http.StatusOK, serversAction{Result: got, Error: errText(err)})
	return true
}

// serversRequest is the body the verb endpoints accept.
type serversRequest struct {
	// Instance is required for every verb except an all-instances uninstall, where "" means "all".
	Instance string `json:"instance"`
	// Purge destroys data on uninstall.
	Purge bool `json:"purge"`
	// ConfirmPurge must be set alongside Purge. See handleServers.
	ConfirmPurge bool `json:"confirmPurge"`
	// RuntimeImages selects optional runtime images at install: "none", "all" or "1,3".
	RuntimeImages string `json:"runtimeImages"`
	// Ports pins specific ports. OMIT THEM for a reinstall: pinning renumbers an existing instance's
	// database, which is the 5433 bug.
	Ports map[string]int `json:"ports,omitempty"`
}

// serversResult is the list response. Error is a STRING rather than a 5xx so a partial list (one unreadable
// instance among several) still renders its rows — the list is the whole page.
type serversResult struct {
	Instances []instancectl.Instance `json:"instances"`
	Error     string                 `json:"error,omitempty"`
}

// setupResult carries one server's setup record.
type setupResult struct {
	Setup *instancectl.SetupProgress `json:"setup"`
	Error string                     `json:"error,omitempty"`
}

// serversAction is one verb's outcome.
type serversAction struct {
	Result instancectl.Result `json:"result"`
	Error  string             `json:"error,omitempty"`
}

// errText renders an error, or "" — so a JSON field is absent rather than "null: null".
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
