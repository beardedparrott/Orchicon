package desktop

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	assets "github.com/beardedparrott/orchicon"
)

// desktopGlobal is the ONE global the shell injects and the ONLY thing the SPA may use to discover
// it is running in the desktop app.
const desktopGlobal = "__ORCHICON_DESKTOP__"

// indexHTMLName is the SPA entry point inside the embedded bundle.
const indexHTMLName = "index.html"

// noFrontendMessage is the fatal diagnostic for a binary built without the SPA.
const noFrontendMessage = "THIS BINARY EMBEDS NO FRONTEND — the desktop shell would open an empty window. " +
	"It was built without `cd frontend && npm run build` (make desktop-build does both, in that order)."

// embeddedSPA returns the embedded SPA filesystem, the same call the plane makes
// (cmd/orchicon/serve_state.go: fs.Sub(assets.FrontendFS, assets.FrontendDir)).
func embeddedSPA() (fs.FS, error) {
	return fs.Sub(assets.FrontendFS, assets.FrontendDir)
}

// desktopScript renders the injection: one frozen, non-configurable object.
func desktopScript(info Info, controlToken string) string {
	payload, err := json.Marshal(struct {
		Desktop bool `json:"desktop"`
		// ControlToken is the credential the SPA must send to the shell's own plane-control endpoints
		// (control.go).
		ControlToken string `json:"controlToken"`
		Info
	}{Desktop: true, ControlToken: controlToken, Info: info})
	if err != nil {
		// Info is all strings and a bool: marshalling cannot fail.
		payload = []byte(`{"desktop":true,"controlToken":""}`)
	}
	return `<script>(function(){Object.defineProperty(window,"` + desktopGlobal + `",{value:Object.freeze(` +
		string(payload) + `),writable:false,configurable:false});})();</script>`
}

// injectDesktopScript places the injection immediately before </head> so it runs before any SPA
// module.
func injectDesktopScript(indexHTML []byte, info Info, controlToken string) []byte {
	script := desktopScript(info, controlToken)
	if i := bytes.Index(indexHTML, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(indexHTML)+len(script))
		out = append(out, indexHTML[:i]...)
		out = append(out, script...)
		out = append(out, indexHTML[i:]...)
		return out
	}
	return append([]byte(script), indexHTML...)
}

// spaHandler serves the EMBEDDED SPA — never a fetched one, and never the plane's UI.
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
func readIndexHTML(spaFS fs.FS, info Info, controlToken string) ([]byte, error) {
	b, err := fs.ReadFile(spaFS, indexHTMLName)
	if err != nil || len(b) == 0 {
		return nil, nil
	}
	return injectDesktopScript(b, info, controlToken), nil
}

// bootstrapHTML is the document Wails' own origin answers with: it NAVIGATES the window to the
// shell's real http origin and renders nothing.
func BootstrapHTML(origin string) string {
	esc := htmlEscape(origin)
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Orchicon</title>
<script>location.replace(` + goStringLiteral(origin) + `);</script>
<style>body{background:#0F1F1A;color:#EDEDE6;font:14px system-ui,sans-serif;margin:0;display:grid;place-items:center;height:100vh}a{color:#5CBFA2}</style>
</head><body><p>Starting Orchicon… <a href="` + esc + `">open the app</a></p></body></html>`
}

// htmlEscape escapes the few characters that matter inside an attribute.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// goStringLiteral renders s as a JavaScript/Go string literal, so a URL bearing
// a quote cannot break out of the inline script.
func goStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
