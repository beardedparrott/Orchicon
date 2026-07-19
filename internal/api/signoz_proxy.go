package api

import "strings"

// rewriteSigNozHTML rewrites the SigNoz SPA's document base href so the
// page resolves its relative URLs against the proxied prefix instead of
// the SigNoz origin. The SigNoz v0.132 index.html looks like:
//
//	<base href="/" />
//	<script src="./assets/index-XXXXX.js"></script>
//	<link href="css/uPlot.min.css" />
//
// All asset tags use relative paths, so a single base-href rewrite is
// sufficient — every relative URL in the document (./assets/..., css/...,
// favicon.ico) resolves against /signoz/ and lands back at
// /signoz/assets/..., /signoz/css/..., /signoz/favicon.ico which the
// reverse proxy already strips-and-forwards to SigNoz.
//
// A previous implementation also rewrote /assets/ -> /signoz/assets/
// and /css/ -> /signoz/css/ directly, but that operation matched the
// substring inside the already-relative ./assets/ paths and produced
// ./signoz/assets/... which the base href then double-prefixed into
// /signoz/signoz/assets/... — the SPA ended up recursively fetching
// its own HTML instead of the JS bundle, leaving the iframe blank.
//
// proxyPrefix is the URL prefix the SPA is mounted under (e.g. "/signoz"
// in the embedded-Iframe case, "" for the un-proxied origin). The base
// href in the rewritten HTML is set to "{prefix}/" so all relative
// URLs resolve from the proxied root.
func rewriteSigNozHTML(body, proxyPrefix string) string {
	baseHref := "href=\"/\""
	rewrittenBaseHref := "href=\"" + proxyPrefix + "/\""
	if proxyPrefix == "" {
		// Unmounted case: the document already has href="/"; nothing to do.
		return body
	}
	return strings.ReplaceAll(body, baseHref, rewrittenBaseHref)
}
