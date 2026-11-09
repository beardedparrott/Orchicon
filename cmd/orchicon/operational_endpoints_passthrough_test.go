package main

// operational_endpoints_passthrough_test.go — EVERY exact-path API handler must be in the SPA
// passthrough list, or the browser silently receives index.html instead of JSON.

import (
	"regexp"
	"strings"
	"testing"
)

// exactPathMounts pulls every literal exact-path handler out of internal/api/api.go.
func exactPathMounts(t *testing.T, src string) []string {
	t.Helper()
	// mux.HandleFunc("/healthz", …)  and  mux.Handle("/enginez", …)
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("(/[a-zA-Z0-9_./-]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		p := m[1]
		if strings.Contains(p, "orchicon.api.v1") {
			continue // a service prefix, matched by prefix in the passthrough
		}
		out = append(out, p)
	}
	return out
}

func TestEveryExactPathAPIRouteIsPassthroughFromTheSPA(t *testing.T) {
	apiSrc := readFile(t, "../../internal/api/api.go")
	serveSrc := readFile(t, "serve_state.go")

	// The passthrough set, as written in withFrontend.
	i := strings.Index(serveSrc, "func withFrontend(")
	if i < 0 {
		t.Fatal("withFrontend is gone")
	}
	body := serveSrc[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}

	mounts := exactPathMounts(t, apiSrc)
	if len(mounts) == 0 {
		t.Fatal("found no exact-path handlers in internal/api/api.go — the extraction pattern is " +
			"stale, so this test would pass while proving nothing")
	}
	for _, p := range mounts {
		if !strings.Contains(body, `"`+p+`"`) {
			t.Errorf("internal/api mounts %q but withFrontend's passthrough list does not name it.\n"+
				"  The SPA fallback at the bottom of withFrontend answers anything not listed here, so\n"+
				"  the browser receives index.html with status 200 for %s — the fetch succeeds, the JSON\n"+
				"  parse fails, and the UI reports the plane as not answering. That is the reported\n"+
				"  'Container availability unknown — the plane did not answer /enginez'.", p, p)
		}
	}
}

// TestEnginezReturnsJSONNotTheSPA pins the reported endpoint by name, because it is the one that was
// actually broken and a regression should say so in the message an operator would search for.
func TestEnginezReturnsJSONNotTheSPA(t *testing.T) {
	serveSrc := readFile(t, "serve_state.go")
	if !strings.Contains(serveSrc, `path == "/enginez"`) {
		t.Error(`withFrontend no longer passes /enginez through to the API. The project page's runtime ` +
			`section will go back to reporting "Container availability unknown" while the endpoint ` +
			`answers 200 with HTML.`)
	}
	// And the handler itself still exists and still writes JSON.
	apiSrc := readFile(t, "../../internal/api/engine_status.go")
	if !strings.Contains(apiSrc, "application/json") {
		t.Error("the /enginez handler no longer sets an application/json content type")
	}
}
