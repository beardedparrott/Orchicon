package desktop

// surfaces.go — WHICH of the shell's own endpoints owns a request.
//
// THE DEFECT THIS EXISTS FOR. The two surfaces were dispatched in the order they were written:
//
//	if s.handleControl(w, r) { return }
//	if s.handleServers(w, r) { return }
//
// and controlPath ("/__orchicon_desktop/") is a PREFIX of serversPath ("/__orchicon_desktop/servers/").
// So every servers request was claimed by handleControl, missed every case in its switch, and answered 404:
//
//	Could not read this machine's servers: desktop shell answered 404
//
// The comment above that code already said "ORDER IS THE CONTRACT, most specific first" — and then listed the
// LESS specific surface first. A comment cannot enforce an ordering; this file computes it instead.
//
// SPECIFICITY IS NOW DERIVED, NOT REMEMBERED. The table is sorted by prefix LENGTH descending, so adding a
// surface cannot silently shadow an existing one, and reordering the literal below changes nothing.

import (
	"net/http"
	"sort"
)

// ownSurface is one of the shell's own endpoint namespaces.
type ownSurface struct {
	prefix string
	handle func(http.ResponseWriter, *http.Request) bool
}

// surfaces returns the shell's own endpoint namespaces, MOST SPECIFIC FIRST.
func (s *Shell) surfaces() []ownSurface {
	out := []ownSurface{
		{prefix: controlPath, handle: s.handleControl},
		{prefix: serversPath, handle: s.handleServers},
	}
	// LONGEST PREFIX FIRST. Stable, so two equal-length prefixes keep their declared order rather than
	// shuffling between requests.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].prefix) > len(out[j].prefix) })
	return out
}

// handleOwnEndpoint answers a request that belongs to one of the shell's surfaces, and reports whether it was
// one of them (so the caller can fall through to the plane's routes and then the SPA).
func (s *Shell) handleOwnEndpoint(w http.ResponseWriter, r *http.Request) bool {
	for _, surface := range s.surfaces() {
		if surface.handle(w, r) {
			return true
		}
	}
	return false
}
