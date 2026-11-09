// Package main — serve state + frontend serving helpers.
package main

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	assets "github.com/beardedparrott/orchicon"
)

// serveStateDir returns the detached serve's state root.
func serveStateDir() string {
	// THE INSTANCE DECIDES, NOT THE SHELL.
	if inst := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE")); inst != "" {
		return hostDataDirFor(inst) + "/serve"
	}
	if d := os.Getenv(serveStateDirEnv); d != "" {
		return d
	}
	return ".dev"
}

// servePIDFile / serveLogFile locate the detached `serve --detach` state.
func servePIDFile() string { return serveStateDir() + "/pids/orchicon.pid" }
func serveLogFile() string { return serveStateDir() + "/logs/orchicon.log" }

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
	return pid, pidAlive(p)
}

// withFrontend wraps the API handler so that non-API requests serve the embedded SPA.
func withFrontend(apiHandler http.Handler, log *slog.Logger) http.Handler {
	spaFS, err := fs.Sub(assets.FrontendFS, assets.FrontendDir)
	if err != nil {
		log.Warn("frontend embed unavailable, serving API only", "error", err)
		return apiHandler
	}
	fileServer := http.StripPrefix("/", http.FileServer(http.FS(spaFS)))
	indexHTML, _ := fs.ReadFile(spaFS, "index.html")
	// A BINARY WITH NO FRONTEND MUST SAY SO, because the symptom points nowhere near the cause.
	if len(indexHTML) == 0 {
		log.Warn("THIS BINARY EMBEDS NO FRONTEND — only the API is served, so the UI at / will report an " +
			"auth error instead of loading. It was built without `cd frontend && npm run build`; the release " +
			"and canary workflows both do that (pinned by TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst).")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/orchicon.api.v1") ||
			strings.HasPrefix(path, "/auth/") ||
			// A REAL ENDPOINT THAT THE UI CANNOT READ IS WORSE THAN A MISSING ONE: the fetch succeeds, the
			// status is 200, and the only symptom is "unknown".
			path == "/healthz" || path == "/versionz" || path == "/enginez" ||
			// Embedded OpenID Provider exact paths (internal/auth/op) — without passthrough the SPA fallback
			// below would swallow them (they are not /auth/*-prefixed).
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
			// Hashed assets (JS, CSS, images) have unique filenames and can be cached.
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
