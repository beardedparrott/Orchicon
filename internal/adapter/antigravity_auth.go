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
	// agy's own wording when the gemini-api-key method is selected with no key configured:
	// `-32602 "GEMINI_API_KEY is not set; pass an API key in _meta.apiKey or set the GEMINI_API_KEY
	// environment variable."` (testdata/acp/auth_required.jsonl:12). It matches every OTHER signal in
	// this list in no way at all — no "authentication", no "sign in", no "api key" in the
	// "invalid api key" sense — so without this entry the ONE failure whose fix is exactly "set the
	// key" fell through to a generic error.
	"gemini_api_key is not set",
	"unauthenticated",
	"unauthorized",
}

// `-32602` ITSELF IS DELIBERATELY NOT A SIGNAL, and the reason generalises: it is JSON-RPC "invalid
// params", which agy also returns for `authenticate{methodId: "no-such-auth-method"}` ("Unsupported auth
// method") and for any malformed request. Matching the CODE would report a client bug as a credential
// problem. The specific MESSAGE above is what is matched instead, so the classification stays a statement
// about the credential.
//
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
