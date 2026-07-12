// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// clients are mounted here onto a single mux. v0.1 scaffolds the
// ProjectService; later phases add the remaining services.
package api

import (
	"net/http"

	"github.com/beardedparrott/orchicon/internal/version"
)

// Mount returns an http.Handler serving the Orchicon API on the given
// mux. Generated connect-go handlers are registered as they are added.
func Mount(mux *http.ServeMux) http.Handler {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})
	return mux
}
