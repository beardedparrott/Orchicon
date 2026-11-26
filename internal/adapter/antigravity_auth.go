package adapter

import "strings"

// Google credential provisioning is HOST-SIDE, never Orchicon-side.

// AntigravityAuthRequiredMessage is the single actionable auth-failure string for the antigravity
// adapter kind.
const AntigravityAuthRequiredMessage = "Antigravity is not signed in on the host — run `agy` and sign in, " +
	"or configure a Gemini API key (GEMINI_API_KEY) on the host, then re-run. " +
	"Orchicon never signs in for you and stores no Google credential of its own. " +
	"IN CONTAINER MODE this is stricter than the claude adapter: agy's personal OAuth credential lives " +
	"in the HOST OS KEYRING, which cannot be bind-mounted into a runtime container — so a container " +
	"session needs GEMINI_API_KEY supplied to the plane (or to this run's secrets), and signing in on " +
	"the host alone is NOT enough."

// antigravityAuthSignals are the case-insensitive substrings that mark an agy-CLI / transport
// failure as an AUTHENTICATION failure.
//
// `-32000` IS DELIBERATELY INCLUDED AND IS NOT AN AUTH CODE. It is JSON-RPC's generic "server error"
// code, so it also covers unrelated failures; the antigravity bridge (a stub today, children 5/6
// later) is required to pass ONLY messages it has already decided are auth-shaped here. It is kept
// because child 1's auth_required fixture emits it, and a mapper that misses it would turn the one
// actionable failure into a generic one.
var antigravityAuthSignals = []string{
	"authentication required",
	"-32000",
	"not signed in",
	"please sign in",
	"please run `agy`",
	"please run agy",
	"invalid api key",
	"unauthenticated",
	"unauthorized",
}

// The antigravity bridge MUST route auth failures through this function so an absent/expired host
// login surfaces the sign-in instruction everywhere, instead of a hang or a generic error.
func AntigravityAuthFailure(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, sig := range antigravityAuthSignals {
		if strings.Contains(lower, sig) {
			return AntigravityAuthRequiredMessage, true
		}
	}
	return "", false
}
