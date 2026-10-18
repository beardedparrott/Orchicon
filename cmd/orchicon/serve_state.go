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

	assets "github.com/beardedparrott/orchicon"
)

// serveStateDir returns the detached serve's state root.
func serveStateDir() string {
	// THE INSTANCE DECIDES, NOT THE SHELL. The state dir is always <instance data dir>/serve, so when an
	// instance is named there is nothing to read from the environment and nothing for an ambient value to
	// get wrong.
	//
	// THIS IS THE FIX FOR A REAL CROSS-INSTANCE COLLISION. `plane start dev` run from a shell that carried
	// a PROD ORCHICON_SERVE_STATE_DIR produced a dev plane whose stdout was
	// ~/.local/share/orchicon-prod/serve/logs/orchicon.log: dev's own log stopped being written, so the
	// failure under investigation was invisible while a sibling instance's log filled up. Stripping the
	// variable at the spawn site was not enough — `getenv` returns the FIRST match, so any path that reads
	// it before the override lands still sees the ambient value. Deriving it from the instance removes the
	// question.
	if inst := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE")); inst != "" {
		return hostDataDirFor(inst) + "/serve"
	}
	if d := os.Getenv(serveStateDirEnv); d != "" {
		return d
	}
	return ".dev"
}

// servePIDFile / serveLogFile locate the detached `serve --detach` state.
//
// THEY ARE FUNCTIONS, NOT VARS, AND THAT IS LOAD-BEARING. They used to be package
// vars, which Go resolves BEFORE any init() runs — so a test could not influence
// them even deliberately: by the time a test binary can unset anything, an
// operator's ambient ORCHICON_SERVE_STATE_DIR (exported by scripts/container.sh
// for their live plane) is already baked into both paths. TestServeStatePathsShape
// asserts the DEFAULT `.dev` shape, so it failed on that machine and passed in CI.
//
// Reading the environment at CALL time is what makes the value testable, and it is
// the same shape the codebase already uses for exactly this reason — internal/tui
// resolves its collapse prefs and config dir through functions, not vars, because a
// package var captured at init cannot be isolated by a test.
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
	// A BINARY WITH NO FRONTEND MUST SAY SO, because the symptom points nowhere near the cause.
	//
	// `frontend/dist/.gitkeep` is tracked precisely so `go:embed all:frontend/dist` always has a target, which
	// means a build that skipped `npm run build` compiles PERFECTLY and embeds only the placeholder. Every UI
	// request then falls through the `len(indexHTML) > 0` branch below to the API handler, where the auth
	// middleware answers `{"error":"missing credentials"}`.
	//
	// OBSERVED EXACTLY THAT: a canary install replaced a working host plane with such a binary, and the
	// operator's browser reported "missing credentials" — a credentials-shaped message for a build problem,
	// with a server that was entirely healthy. This line makes the next occurrence a one-line answer.
	if len(indexHTML) == 0 {
		log.Warn("THIS BINARY EMBEDS NO FRONTEND — only the API is served, so the UI at / will report an " +
			"auth error instead of loading. It was built without `cd frontend && npm run build`; the release " +
			"and canary workflows both do that (pinned by TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst).")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/orchicon.api.v1") ||
			strings.HasPrefix(path, "/auth/") ||
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
