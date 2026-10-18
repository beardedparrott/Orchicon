// Package desktop is the Orchicon desktop shell: it serves the EMBEDDED SPA
// from its own loopback origin and reverse-proxies the plane's API paths to the
// plane the app is configured with.
//
// WHY AN ORIGIN AT ALL (the decision the whole package rests on). The SPA
// already speaks Connect-ES to `window.location.origin` with
// `credentials: "include"` (frontend/src/api/clients.ts), because its auth is a
// bearer token plus an HttpOnly refresh cookie. A webview custom scheme
// (wails://) is NOT a cookie-bearing origin: the refresh cookie is dropped, the
// interceptor's 401 retry never recovers, and the user is bounced to /login in
// a loop. A real `http://127.0.0.1:<port>` origin keeps the cookie jar, keeps
// `window.location.origin` correct (so the SPA needs no base-URL knob and
// frontend/vite.config.ts is untouched), and needs NO CORS on the plane and NO
// new unauthenticated route in internal/api — the webview is same-origin with
// the shell, and the shell is the only thing that talks to the plane.
//
// CONSEQUENCE, deliberately: the shell never proxies `/` to the plane, so a
// plane of a different version cannot silently supply the UI, and the shell
// draws no chrome — the SPA's own top bar IS the app chrome.
//
// THIS PACKAGE IMPORTS NO PLANE CODE (see imports_test.go): it is testable
// without a webview, which is what makes the transport verifiable at all.
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/controlport"
)

// UnknownInstance is the instance reported when the configured plane URL's port
// is not one of the installer's three control ports. It is deliberately not
// "dev": the Manage Server page has to be able to tell "not one of ours" from
// "prod", and guessing would make the app's title bar lie.
const UnknownInstance = "unknown"

// PlaneUnreachable is the plane tag reported when /versionz could not be read.
// It is NOT a version mismatch — a plane that is down is a different failure,
// already visible as a failing RPC from the SPA.
const PlaneUnreachable = "unreachable"

// Info is the payload the shell hands the SPA (window.__ORCHICON_DESKTOP__) and
// the source of the window title. Every field is resolved ONCE, at startup, so
// the title and the Manage Server page cannot disagree about which plane this
// window is attached to.
type Info struct {
	// PlaneURL is the plane this window is configured with, exactly as passed
	// in (not the shell's loopback origin — see Shell.OriginURL for that).
	PlaneURL string `json:"planeURL"`
	// Instance is resolved from PlaneURL's control port via the shared table
	// (internal/controlport), never from a second notion of instance.
	Instance string `json:"instance"`
	// AppTag is this binary's own version (internal/version).
	AppTag string `json:"appTag"`
	// PlaneTag is the version the plane reports at /versionz.
	PlaneTag string `json:"planeTag"`
	// VersionMismatch is true only when BOTH tags are known and differ. It is
	// about coherence, not semver ordering: the SPA rendered here was built
	// with this binary, and the plane it talks to was built from a different
	// tree, so the UI may be speaking an API the plane does not have.
	VersionMismatch bool `json:"versionMismatch"`
}

// BuildInfo assembles the shell's Info. Pure, so the mismatch rule is testable
// without a plane.
func BuildInfo(planeURL, appTag, planeTag string) Info {
	instance := UnknownInstance
	if i, ok := controlport.InstanceForURL(planeURL); ok {
		instance = i
	}
	mismatch := appTag != "" && planeTag != "" &&
		planeTag != PlaneUnreachable && appTag != planeTag
	return Info{
		PlaneURL:        planeURL,
		Instance:        instance,
		AppTag:          appTag,
		PlaneTag:        planeTag,
		VersionMismatch: mismatch,
	}
}

// planeVersionTimeout bounds the startup /versionz read. A slow plane must not
// hold the window closed; the tag is diagnostic, not load-bearing.
const planeVersionTimeout = 2 * time.Second

// FetchPlaneTag reads the plane's version from /versionz
// (internal/api/api.go: plain {"version":"<tag>"} JSON next to /healthz).
func FetchPlaneTag(ctx context.Context, planeURL string, client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, planeVersionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(planeURL, "/")+"/versionz", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s/versionz: %s", planeURL, resp.Status)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode /versionz: %w", err)
	}
	if payload.Version == "" {
		return "", fmt.Errorf("GET %s/versionz: empty version", planeURL)
	}
	return payload.Version, nil
}
