// Package main — serve state + frontend serving helpers.
//
// These used to live in dev.go (the compose dev subcommand); since the
// single-container deployment replaced the compose workflow, the detached
// serve helpers and the embedded-SPA wrapper belong to the serve command
// itself.
package main

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	assets "github.com/beardedparrott/orchicon"
)

// servePIDFile / serveLogFile locate the detached `serve --detach` state.
const (
	servePIDFile = ".dev/pids/orchicon.pid"
	serveLogFile = ".dev/logs/orchicon.log"
)

// procRunning reports whether the process named in pidFile is alive.
func procRunning(pidFile string) (string, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return "", false
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		return "", false
	}
	p, err := strconv.Atoi(pid)
	if err != nil {
		return pid, false
	}
	proc, err := os.FindProcess(p)
	if err != nil {
		return pid, false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return pid, false
	}
	return pid, true
}

// withFrontend wraps the API handler so that non-API requests serve the
// embedded SPA. API routes (starting with /orchicon.api.v1) and
// health/version endpoints are passed through to the API handler. All
// other paths serve from the embedded frontend/dist, falling back to
// index.html for client-side routing (docs/10 §9).
func withFrontend(apiHandler http.Handler, log *slog.Logger) http.Handler {
	spaFS, err := fs.Sub(assets.FrontendFS, assets.FrontendDir)
	if err != nil {
		log.Warn("frontend embed unavailable, serving API only", "error", err)
		return apiHandler
	}
	fileServer := http.StripPrefix("/", http.FileServer(http.FS(spaFS)))
	indexHTML, _ := fs.ReadFile(spaFS, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/orchicon.api.v1") ||
			strings.HasPrefix(path, "/auth/") ||
			strings.HasPrefix(path, "/grafana") ||
			path == "/healthz" || path == "/versionz" ||
			// Embedded OpenID Provider exact paths (internal/auth/op) —
			// without passthrough the SPA fallback below would swallow
			// them (they are not /auth/*-prefixed).
			path == "/.well-known/openid-configuration" ||
			path == "/authorize" || path == "/authorize/callback" ||
			path == "/token" || path == "/userinfo" || path == "/jwks" {
			apiHandler.ServeHTTP(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		f, err := spaFS.Open(cleanPath)
		if err == nil {
			f.Close()
			// Hashed assets (JS, CSS, images) have unique filenames and
			// can be cached. HTML files must never be cached so the SPA
			// picks up new JS bundles on deployment.
			if strings.HasSuffix(path, ".html") || strings.HasSuffix(path, "/") || !strings.Contains(path, ".") {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		if len(indexHTML) > 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Write(indexHTML)
			return
		}

		apiHandler.ServeHTTP(w, r)
	})
}
