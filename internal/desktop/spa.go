package desktop

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	assets "github.com/beardedparrott/orchicon"
)

// desktopGlobal is the ONE global the shell injects and the ONLY thing the SPA
// may use to discover it is running in the desktop app. It is injected here and
// NOWHERE ELSE: the plane's withFrontend (cmd/orchicon/serve_state.go) never
// emits it, so in a browser it is simply undefined.
const desktopGlobal = "__ORCHICON_DESKTOP__"

// indexHTMLName is the SPA entry point inside the embedded bundle.
const indexHTMLName = "index.html"

// noFrontendMessage is the fatal diagnostic for a binary built without the SPA.
//
// THIS IS THE SILENT-EMBED TRAP, and here it is FATAL rather than a warning.
// `frontend/dist/.gitkeep` is tracked so `go:embed all:frontend/dist` always has
// a target, which means a build that skipped `npm run build` compiles perfectly
// and embeds only the placeholder. In the plane that produced a
// credentials-shaped error from a healthy server (see withFrontend's warning);
// in a desktop app it would open an empty window with no explanation at all, so
// the shell refuses to launch instead.
const noFrontendMessage = "THIS BINARY EMBEDS NO FRONTEND — the desktop shell would open an empty window. " +
	"It was built without `cd frontend && npm run build` (make desktop-build does both, in that order)."

// embeddedSPA returns the embedded SPA filesystem, the same call the plane makes
// (cmd/orchicon/serve_state.go: fs.Sub(assets.FrontendFS, assets.FrontendDir)).
func embeddedSPA() (fs.FS, error) {
	return fs.Sub(assets.FrontendFS, assets.FrontendDir)
}

// desktopScript renders the injection: one frozen, non-configurable object.
//
// THE PROPERTY THAT MATTERS: page script must not be able to turn the flag on
// or change what it says. `Object.defineProperty` with `writable:false` and
// `configurable:false`, plus `Object.freeze`, means a second defineProperty
// throws and assignment is a silent no-op — so "the browser can never pretend
// to be the desktop app" holds against the SPA's own code, not just by
// convention. The payload is produced by encoding/json so a URL containing
// quotes cannot break out of the script tag.
func desktopScript(info Info) string {
	payload, err := json.Marshal(struct {
		Desktop bool `json:"desktop"`
		Info
	}{Desktop: true, Info: info})
	if err != nil {
		// Info is all strings and a bool: marshalling cannot fail. Blanking the
		// object would be worse than a broken script, so keep going with the
		// fields we know are safe.
		payload = []byte(`{"desktop":true}`)
	}
	return `<script>(function(){Object.defineProperty(window,"` + desktopGlobal + `",{value:Object.freeze(` +
		string(payload) + `),writable:false,configurable:false});})();</script>`
}

// injectDesktopScript places the injection immediately before </head> so it runs
// before any SPA module. If the bundle has no </head> (never true of a Vite
// build, but a hand-rolled one could), the script is prefixed instead: the flag
// must exist, whatever the document looks like.
func injectDesktopScript(indexHTML []byte, info Info) []byte {
	script := desktopScript(info)
	if i := bytes.Index(indexHTML, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(indexHTML)+len(script))
		out = append(out, indexHTML[:i]...)
		out = append(out, script...)
		out = append(out, indexHTML[i:]...)
		return out
	}
	return append([]byte(script), indexHTML...)
}

// spaHandler serves the EMBEDDED SPA — never a fetched one, and never the
// plane's UI.
//
// THE TWO SERVING RULES MIRROR withFrontend (cmd/orchicon/serve_state.go): real
// files are served as files (hashed assets may be cached); index.html and the
// client-routing fallback are served from the injected bytes with no-cache, so a
// rebuilt app never runs a stale bundle. The one difference is the injection,
// which exists only here.
func (s *Shell) spaHandler() http.Handler {
	fileServer := http.StripPrefix("/", http.FileServer(http.FS(s.spaFS)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// The entry point is always the injected document: serving index.html
		// from the file server would ship the SPA with no desktop flag at all.
		if path == "/" || path == "/"+indexHTMLName {
			writeIndex(w, s.indexHTML)
			return
		}
		clean := strings.TrimPrefix(path, "/")
		if f, err := s.spaFS.Open(clean); err == nil {
			f.Close()
			if strings.HasSuffix(path, ".html") || strings.HasSuffix(path, "/") {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// Client-side route (TanStack Router): hand back the app document.
		writeIndex(w, s.indexHTML)
	})
}

// writeIndex writes the injected document with the plane's no-cache rule.
func writeIndex(w http.ResponseWriter, body []byte) {
	if len(body) == 0 {
		http.Error(w, noFrontendMessage, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(body)
}

// readIndexHTML reads the embedded entry point and injects the desktop flag.
// A missing index.html is the no-frontend build: the caller turns it into the
// fatal launch error.
func readIndexHTML(spaFS fs.FS, info Info) ([]byte, error) {
	b, err := fs.ReadFile(spaFS, indexHTMLName)
	if err != nil || len(b) == 0 {
		return nil, nil
	}
	return injectDesktopScript(b, info), nil
}
