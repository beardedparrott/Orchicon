package api

import (
	"strings"
	"testing"
)

// TestRewriteSigNozHTML_BaseHrefOnly verifies the fix for the embedded
// SigNoz iframe bug. The earlier implementation also rewrote /assets/
// and /css/ substrings, which corrupted the already-relative ./assets/
// paths into ./signoz/assets/... — the document's base href then
// resolved them to /signoz/signoz/assets/... and the proxy returned
// the SPA HTML instead of the JS bundle, leaving the iframe blank.
func TestRewriteSigNozHTML_BaseHrefOnly(t *testing.T) {
	// Verbatim copy of the relative asset tags SigNoz v0.132 ships in
	// its index.html — this is what the browser actually receives.
	const input = `<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8" />
		<base href="/" />
		<link rel="preconnect" href="https://fonts.googleapis.com" />
		<link data-react-helmet="true" rel="shortcut icon" href="favicon.ico" />
		<script type="module" crossorigin src="./assets/index-CiBb5ZTE.js"></script>
		<link rel="stylesheet" crossorigin href="./assets/index-OjjtrzeA.css">
		<link rel="stylesheet" href="css/uPlot.min.css" />
		<meta property="og:image" content="/images/signoz-hero-image.webp" />
	</head>
	<body>
		<div id="root"></div>
	</body>
</html>`

	got := rewriteSigNozHTML(input, "/signoz")

	// Base href is rewritten. Asset paths are now rewritten to absolute
	// /signoz/... paths so the iframe loads them without relying on the
	// document <base> (which Firefox's sandboxed iframe ignores for
	// relative URL resolution).
	mustContain(t, got, `<base href="/signoz/" />`)
	mustContain(t, got, `src="/signoz/assets/index-CiBb5ZTE.js"`)
	mustContain(t, got, `href="/signoz/assets/index-OjjtrzeA.css"`)
	mustContain(t, got, `href="/signoz/css/uPlot.min.css"`)
	mustContain(t, got, `href="/signoz/favicon.ico"`)
	mustContain(t, got, `content="/images/signoz-hero-image.webp"`)

	// The previous (buggy) implementation would have produced these —
	// asserting they're absent guards against a regression.
	mustNotContain(t, got, `src="./signoz/assets/`)
	mustNotContain(t, got, `href="./signoz/assets/`)
	mustNotContain(t, got, `href="signoz/css/`)
	mustNotContain(t, got, `/signoz/signoz/`)
}

// TestRewriteSigNozHTML_NoPrefix confirms the un-proxied case (no
// rewrite needed) passes through unchanged.
func TestRewriteSigNozHTML_NoPrefix(t *testing.T) {
	const input = `<html><head><base href="/" /></head></html>`
	got := rewriteSigNozHTML(input, "")
	if got != input {
		t.Fatalf("empty proxy prefix should be a no-op, got %q", got)
	}
}

// TestRewriteSigNozHTML_NoBaseHref checks that documents without the
// base href pass through unchanged (e.g. SPA route responses served
// by SigNoz when navigated to /logs etc.).
func TestRewriteSigNozHTML_NoBaseHref(t *testing.T) {
	const input = `<html><head></head><body>nope</body></html>`
	got := rewriteSigNozHTML(input, "/signoz")
	if got != input {
		t.Fatalf("expected unchanged output, got %q", got)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected output to NOT contain %q, got:\n%s", needle, haystack)
	}
}
