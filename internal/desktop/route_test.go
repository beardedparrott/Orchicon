package desktop

// route_test.go — a launch argument must not become an open redirect.

import "testing"

func TestSafeRouteAcceptsKnownRoutes(t *testing.T) {
	for _, in := range []string{"/manage-server", " /manage-server ", "/manage-server/", "/MANAGE-SERVER", "/manage-server?tab=add"} {
		if got := SafeRoute(in); got != "/manage-server" {
			t.Errorf("SafeRoute(%q) = %q, want /manage-server", in, got)
		}
	}
}

func TestSafeRouteRefusesAnythingElse(t *testing.T) {
	// Every one of these is either an open-redirect vector or simply not a page we ship. All must fall
	// back to "" (the app's own start page) rather than being "cleaned up" into something loadable.
	for _, in := range []string{
		"",
		"   ",
		"/",
		"/settings",
		"https://example.invalid",
		"//example.invalid/manage-server",
		"/\\example.invalid",
		"/../manage-server",
		"/manage-server/../admin",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"manage-server", // no leading slash
	} {
		if got := SafeRoute(in); got != "" {
			t.Errorf("SafeRoute(%q) = %q, want \"\" — a launch argument must not navigate anywhere but a "+
				"known app route (an open redirect in a window carrying this app's icon is a phishing "+
				"surface)", in, got)
		}
	}
}
