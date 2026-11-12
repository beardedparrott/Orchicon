package desktop

// route.go — which page the window may OPEN on.

import "strings"

// openableRoutes are the pages a launch may target.
var openableRoutes = []string{
	"/manage-server", // the handover target: servers, setup progress, and the verbs
}

// SafeRoute returns route when it is an openable app route, and "" otherwise.
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
