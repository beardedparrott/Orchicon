// Package main — serve state + frontend serving helpers.
package main

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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


// compressibleExt lists the asset types worth compressing. Images and fonts are already compressed, so
// re-encoding them spends CPU to add bytes.
var compressibleExt = map[string]bool{
	".js": true, ".css": true, ".html": true, ".json": true, ".svg": true, ".map": true,
}

// serveCompressed serves an embedded asset gzipped when the client accepts it.
//
// WITHOUT THIS THE PLANE SHIPPED THE RAW BUNDLE, and the UI is one 2.8 MB JavaScript file: a refresh on any
// network that is not loopback sat on a blank canvas for seconds while it downloaded. Gzip takes that file
// to ~707 KB (75% smaller, 80% with brotli), which is the difference between "slow" and "loading".
//
// http.FileServer over an embed.FS never compresses — it has no encoder and no way to know the content type
// before reading — so the encoding happens here, where the file is already open and its bytes are in memory.
//
// The encoded form is cached in memory, keyed by path: these are content-hashed release assets that never
// change for the life of the process, so the compression cost is paid once per asset rather than per request.
func serveCompressed(w http.ResponseWriter, r *http.Request, spaFS fs.FS, cleanPath string, fallback http.Handler) {
	if !compressibleExt[strings.ToLower(filepath.Ext(cleanPath))] ||
		!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
		r.Method == http.MethodHead {
		fallback.ServeHTTP(w, r)
		return
	}
	body, cached := gzipCacheLoad(cleanPath)
	if !cached {
		raw, err := fs.ReadFile(spaFS, cleanPath)
		if err != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(raw); err != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		if err := zw.Close(); err != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		body = buf.Bytes()
		gzipCacheStore(cleanPath, body)
	}
	// Vary is REQUIRED: without it a shared cache may hand the gzipped bytes to a client that did not ask
	// for them, which decodes as garbage.
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", contentTypeFor(cleanPath))
	http.ServeContent(w, r, cleanPath, time.Time{}, bytes.NewReader(body))
}

// gzipCache is a plain map: the set of embedded assets is small and fixed, and concurrent writes to
// distinct keys are safe behind the mutex without costing a lock per request in the steady state.
var (
	gzipMu    sync.Mutex
	gzipByKey = map[string][]byte{}
)

func gzipCacheLoad(key string) ([]byte, bool) {
	gzipMu.Lock()
	defer gzipMu.Unlock()
	b, ok := gzipByKey[key]
	return b, ok
}

func gzipCacheStore(key string, body []byte) {
	gzipMu.Lock()
	defer gzipMu.Unlock()
	gzipByKey[key] = body
}

// contentTypeFor answers from the extension rather than sniffing gzipped bytes — http.DetectContentType on
// compressed data reports application/x-gzip, and a browser then refuses to execute the script.
func contentTypeFor(cleanPath string) string {
	switch strings.ToLower(filepath.Ext(cleanPath)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	}
	return "application/octet-stream"
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
			serveCompressed(w, r, spaFS, cleanPath, fileServer)
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
