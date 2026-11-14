// Package desktop is the Orchicon desktop shell: it serves the EMBEDDED SPA from its own loopback
// origin and reverse-proxies the plane's API paths to the plane the app is configured with.
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

// UnknownInstance is the instance reported when the configured plane URL's port is not one of the
// installer's three control ports.
const UnknownInstance = "unknown"

// PlaneUnreachable is the plane tag reported when /versionz could not be read.
const PlaneUnreachable = "unreachable"

// Info is the payload the shell hands the SPA (window.__ORCHICON_DESKTOP__) and the source of the
// window title.
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
	// VersionMismatch is true only when BOTH tags are known and differ.
	VersionMismatch bool `json:"versionMismatch"`
}

// BuildInfo assembles the shell's Info. Pure, so the mismatch rule is testable
// without a plane.
func BuildInfo(planeURL, appTag, planeTag string) Info {
	instance := UnknownInstance
	if i, ok := controlport.InstanceForURL(planeURL); ok {
		instance = i
	}
	// A LEADING `v` IS NOT A VERSION DIFFERENCE.
	//
	// Both tags come from the same `internal/version.gitTag`, which is `v0.6.1` when the
	// release stamps it — but one build path once stamped `0.6.1`, and the app then
	// announced a mismatch against ITSELF ("this window's UI is v0.6.1 but the plane is
	// 0.6.1"). That is one character, not an API incompatibility, and warning a user about
	// it is worse than saying nothing: the banner exists to signal a real drift.
	//
	// The stamp is fixed at its source, but this comparison is the second half of the
	// repair. It is deliberately narrow — only the leading `v`, only on both sides — so a
	// genuine difference ("0.6.1" vs "0.6.2", or "dev" vs "v0.6.1") still reports.
	mismatch := appTag != "" && planeTag != "" &&
		planeTag != PlaneUnreachable && normalizeTag(appTag) != normalizeTag(planeTag)
	return Info{
		PlaneURL:        planeURL,
		Instance:        instance,
		AppTag:          appTag,
		PlaneTag:        planeTag,
		VersionMismatch: mismatch,
	}
}

// normalizeTag strips a single leading `v` so `v0.6.1` and `0.6.1` compare equal.
//
// It is NOT a general version normalizer: it does not touch case, pre-release suffixes or
// build metadata. Those are real differences and the banner should report them.
func normalizeTag(tag string) string {
	return strings.TrimPrefix(tag, "v")
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
