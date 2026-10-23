package desktop

// route.go — which page the window may OPEN on.
//
// D7: the installer's Finish page launches `orchicon-desktop.exe --route /manage-server`, so a first run
// lands on the page that finishes setting up rather than on the home page of a plane that is still booting.
//
// A LAUNCH ARGUMENT IS NOT A NAVIGATION CHANNEL. The value arrives from a command line written by an
// installer, a shortcut, or anything else that can spawn a process, and it is used to build the URL the
// webview loads. Left unvalidated it would be an open redirect: `--route https://example.invalid` would
// open a remote page inside a window that carries this app's icon and title, which is a phishing surface.
//
// So the value must be a KNOWN APP ROUTE. An allowlist rather than a sanitiser, because a sanitiser has to
// anticipate every escape (%2e%2e, backslashes, encoded slashes, a scheme in an unexpected position) and an
// allowlist has to anticipate nothing. Anything unrecognised falls back to the app's own start page — a
// typo must land somewhere sensible, never on a blank window.

import "strings"

// openableRoutes are the pages a launch may target.
//
// DELIBERATELY SHORT. These exist because an installer hands over to them; adding a route is a decision
// about what a launch argument may do, not a convenience.
var openableRoutes = []string{
	"/manage-server", // the handover target: servers, setup progress, and the verbs
}

// SafeRoute returns route when it is an openable app route, and "" otherwise.
//
// "" means "the app's own start page", which is what every caller already gets with no --route at all.
func SafeRoute(route string) string {
	r := strings.TrimSpace(route)
	if r == "" {
		return ""
	}
	// A query string or fragment is not part of the route's IDENTITY; compare on the path only, and only
	// when the whole value is a path (no scheme, no host, no authority).
	if i := strings.IndexAny(r, "?#"); i >= 0 {
		r = r[:i]
	}
	// NO SCHEME, NO HOST, NO TRAVERSAL. A leading "//" is a protocol-relative URL (a real open-redirect
	// vector), and ".." is a path escape; neither is a route we ship.
	if !strings.HasPrefix(r, "/") || strings.HasPrefix(r, "//") ||
		strings.Contains(r, "..") || strings.Contains(r, `\`) || strings.Contains(r, ":") {
		return ""
	}
	r = strings.TrimSuffix(r, "/")
	for _, known := range openableRoutes {
		if strings.EqualFold(r, known) {
			return known
		}
	}
	return ""
}
