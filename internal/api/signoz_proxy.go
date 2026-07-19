package api

import (
	"strings"
)

// rewriteSigNozHTML rewrites the SigNoz SPA's document so all asset
// URLs become absolute /signoz/... paths. The SigNoz v0.132 index.html
// looks like:
//
//	<base href="/" />
//	<script src="./assets/index-XXXXX.js"></script>
//	<link href="css/uPlot.min.css" />
//
// Relative paths like ./assets/... and css/... would normally resolve
// against <base href="/signoz/">, but Firefox's sandboxed iframe
// ignores the base href for relative URL resolution. So we rewrite
// every asset reference to an absolute /signoz/... path. The proxy
// at /signoz/ strips the prefix and forwards to SigNoz as /assets/...,
// /css/..., etc.
func rewriteSigNozHTML(body, proxyPrefix string) string {
	if proxyPrefix == "" {
		// Unmounted case: nothing to do.
		return body
	}

	// 1. Rewrite the document base href so non-asset relative URLs
	// (e.g. internal SPA navigation links) still work.
	body = strings.ReplaceAll(body, `href="/"`, `href="`+proxyPrefix+`/"`)

	// 2. Rewrite all relative asset paths to absolute /signoz/... paths.
	// This bypasses Firefox's sandboxed-iframe base-href bug.
	// Script src: ./assets/... -> /signoz/assets/...
	body = strings.ReplaceAll(body, `src="./assets/`, `src="`+proxyPrefix+`/assets/`)
	body = strings.ReplaceAll(body, `src='./assets/`, `src='`+proxyPrefix+`/assets/`)
	// Link href: ./assets/... -> /signoz/assets/...
	body = strings.ReplaceAll(body, `href="./assets/`, `href="`+proxyPrefix+`/assets/`)
	body = strings.ReplaceAll(body, `href='./assets/`, `href='`+proxyPrefix+`/assets/`)
	// Link href: css/... -> /signoz/css/...
	body = strings.ReplaceAll(body, `href="css/`, `href="`+proxyPrefix+`/css/`)
	body = strings.ReplaceAll(body, `href='css/`, `href='`+proxyPrefix+`/css/`)
	// Favicon and other root-relative assets
	body = strings.ReplaceAll(body, `href="favicon.ico`, `href="`+proxyPrefix+`/favicon.ico`)
	body = strings.ReplaceAll(body, `href='favicon.ico`, `href='`+proxyPrefix+`/favicon.ico`)

	return body
}
