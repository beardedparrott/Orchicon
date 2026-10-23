package desktop

// control.go — PLANE CONTROL OVER THE SHELL'S OWN ORIGIN.
//
// # WHY THIS IS HTTP AND NOT A WAILS BINDING
//
// The SPA is served from the shell's loopback origin, not from Wails' own
// document, and it has to be: the HttpOnly refresh cookie needs a real http
// origin, and a Connect server-stream must not be buffered. Wails injects its
// JS bindings into ITS document only, so on the loopback origin
// `window.go.planebind` does not exist at all — which is exactly what the
// operator saw: "Connecting / dev / port unknown / contacting the plane…", with
// every control inert, because planeControl() resolved to null and the status
// probe never ran.
//
// The two requirements are incompatible on one origin, so plane control moves to
// the transport that already works: same-origin HTTP to this shell, which
// delegates straight into internal/planebind — the same package, and therefore
// the same planecontrol, the CLI and the TUI drive. No second mechanism.
//
// # THE THREAT THIS TOKEN ADDRESSES
//
// The shell's origin is loopback-only and otherwise unauthenticated. Exposing
// Start/Stop on it means anything that can POST here can bounce a plane. Two
// distinct risks, handled differently:
//
//   - A MALICIOUS WEB PAGE cannot: it cannot read our responses cross-origin, so
//     it cannot learn the token, and without it every request is 403. This is the
//     real reason the token exists.
//   - A LOCAL PROCESS as the same user can (it can read the served HTML and
//     scrape the token). That is NOT a privilege escalation and is not pretended
//     to be blocked: the same user can already run `orchicon plane start dev`.
//     The loopback bind is the boundary, and it is the same boundary the CLI has.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/planebind"
)

// controlPath is the prefix every plane-control endpoint lives under. It is
// namespaced so it can never be mistaken for a plane route (see proxyPrefixes):
// a plane path is forwarded, this one is answered here.
const controlPath = "/__orchicon_desktop/"

// controlTokenHeader carries the per-run token the shell injects into the SPA.
const controlTokenHeader = "X-Orchicon-Desktop"

// Control is the shell's view of plane control. internal/planebind.Service
// satisfies it exactly, and a test drives the endpoints through a fake.
type Control interface {
	Instances() ([]planebind.StatusDTO, error)
	Status(instance string) (planebind.StatusDTO, error)
	ResolveControlPort(port int) (planebind.ResolutionDTO, error)
	Start(instance string) (planebind.ActionDTO, error)
	Stop(instance string) (planebind.ActionDTO, error)
	Restart(instance string) (planebind.ActionDTO, error)
}

// SetControl installs the plane-control surface and its token. Called once at
// startup, before the window opens.
func (s *Shell) SetControl(c Control, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.control = c
	s.controlToken = token
}

// controlTokenValue is read under the lock because SetControl may run while the
// origin is already serving.
func (s *Shell) controlTokenValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlToken
}

// handleControl answers one plane-control request. It returns true when it owned
// the request, so the caller can fall through to the SPA otherwise.
func (s *Shell) handleControl(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, controlPath) {
		return false
	}
	s.mu.Lock()
	ctl := s.control
	s.mu.Unlock()

	// 404 (not 403) when no control surface is installed: the endpoint does not
	// exist on a shell that has none, and a 403 would advertise it.
	if ctl == nil {
		http.NotFound(w, r)
		return true
	}
	// SECRET COMPARISON IS NOT TIMING-SAFE HERE ON PURPOSE: the token authenticates
	// a same-machine drive-by, not a network peer, and a timing side channel over
	// loopback to a process that can already read the file is not the boundary.
	if !s.controlTokenOK(r) {
		writeJSON(w, http.StatusForbidden, errorBody{"this endpoint needs the desktop app's own token"})
		return true
	}

	switch strings.TrimPrefix(r.URL.Path, controlPath) {
	case "instances":
		got, err := ctl.Instances()
		writeJSON(w, http.StatusOK, result(got, err))
	case "status":
		got, err := ctl.Status(r.URL.Query().Get("instance"))
		writeJSON(w, http.StatusOK, result(got, err))
	case "resolve":
		port, perr := strconv.Atoi(r.URL.Query().Get("port"))
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{"port must be a number"})
			return true
		}
		got, err := ctl.ResolveControlPort(port)
		writeJSON(w, http.StatusOK, result(got, err))
	case "start", "stop", "restart":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{"POST required"})
			return true
		}
		var body struct {
			Instance string `json:"instance"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
		if strings.TrimSpace(body.Instance) == "" {
			writeJSON(w, http.StatusBadRequest, errorBody{"instance is required: the app must never start \"the plane\" without saying which"})
			return true
		}
		var (
			got planebind.ActionDTO
			err error
		)
		switch strings.TrimPrefix(r.URL.Path, controlPath) {
		case "start":
			got, err = ctl.Start(body.Instance)
		case "stop":
			got, err = ctl.Stop(body.Instance)
		default:
			got, err = ctl.Restart(body.Instance)
		}
		writeJSON(w, http.StatusOK, result(got, err))
	case "plane":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{"POST required"})
			return true
		}
		var body struct {
			Instance string `json:"instance"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
		info, err := s.UsePlane(body.Instance)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
			return true
		}
		writeJSON(w, http.StatusOK, info)
	default:
		http.NotFound(w, r)
	}
	return true
}

// UsePlane retargets the window at another instance's CONTROL PORT.
//
// It exists because "which instance am I talking to" was previously fixed at
// launch, and a person running dev and prod had no way to say. The proxy reads
// s.planeURL per request (see newProxy), so swapping it here moves every
// subsequent API call; the SPA reloads to re-read desktopInfo.
//
// The port comes from the shared controlport table — never a literal — so the
// app cannot invent a fifth port that no instance uses.
func (s *Shell) UsePlane(instance string) (Info, error) {
	instance = strings.TrimSpace(instance)
	port, ok := controlport.Defaults()[instance]
	if !ok || instance == "" {
		return Info{}, errors.New("unknown instance " + strconv.Quote(instance) + " — expected dev, prod or test")
	}
	u, err := planeURLFromString("http://127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		return Info{}, err
	}

	// Refetch the version OUTSIDE the lock: it is a network call with a bound, and
	// holding the mutex through it would stall every in-flight request.
	planeTag, tagErr := FetchPlaneTag(context.Background(), u.String(), nil)
	if tagErr != nil {
		planeTag = PlaneUnreachable
	}
	info := BuildInfo(u.String(), s.binaryTag(), planeTag)

	s.mu.Lock()
	s.planeURL = u
	s.info = info
	prefsPath := s.prefsPath
	s.mu.Unlock()

	// Best-effort: a read-only config dir must not undo the switch the operator
	// just asked for. The in-memory switch already happened.
	if err := SaveChosenInstance(prefsPath, instance); err != nil {
		s.logMessage("desktop: could not remember the chosen instance: %v", err)
	}
	return info, nil
}

// PlaneURL is the plane this window currently points at.
func (s *Shell) PlaneURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planeURL.String()
}

// errorBody is the shape every failure uses, so the SPA has one thing to read.
type errorBody struct {
	Error string `json:"error"`
}

// result wraps a control call so a caller can tell "the action failed" from "the
// call never happened" — the same distinction the page draws between a bad state
// and a failed read.
func result[T any](v T, err error) any {
	if err != nil {
		return errorBody{Error: err.Error()}
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The shell is same-origin with the SPA, so no CORS header belongs here; the
	// full answer must not be cached by the webview.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// controlTokenOK reports whether r carries this run's control token.
//
// EXTRACTED SO THE SERVERS SURFACE USES THE SAME RULE, not a copy of it. A second implementation of "is
// this caller allowed" is how one surface ends up weaker than the other — and these endpoints can destroy
// a database, so the weaker one would be the dangerous one.
func (s *Shell) controlTokenOK(r *http.Request) bool {
	s.mu.Lock()
	want := s.controlToken
	s.mu.Unlock()
	return want == "" || r.Header.Get(controlTokenHeader) == want
}
