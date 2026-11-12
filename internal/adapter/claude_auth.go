package adapter

import "strings"

// Anthropic credential provisioning is HOST-SIDE, never Orchicon-side.

// ClaudeAuthRequiredMessage is the single actionable auth-failure string for the claude adapter
// kind.
const ClaudeAuthRequiredMessage = "Claude is not authenticated on the host — run `claude` and sign in on the host, " +
	"or set ANTHROPIC_API_KEY on the host, then re-run. " +
	"Orchicon never signs in for you and stores no Anthropic credential of its own; in CONTAINER mode the " +
	"credential you create on the host (~/.claude, or the API key) is mounted into the runtime container, " +
	"so signing in on the host is all that is needed — there is no separate sign-in inside the container, " +
	"and none is possible."

// claudeAuthSignals are the case-insensitive substrings that mark a claude-CLI / transport failure
// as an AUTHENTICATION failure.
var claudeAuthSignals = []string{
	"not authenticated",
	"invalid api key",
	"api key is invalid",
	"api key is missing",
	"authentication_error",
	"authentication failed",
	"oauth token has expired",
	"oauth token expired",
	"please run `claude`",
	"please run claude",
	"unauthorized",
	"401",
}

// The claude bridge MUST route auth failures through this function so an expired/absent host login
// surfaces the sign-in instruction everywhere.
func ClaudeAuthFailure(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, sig := range claudeAuthSignals {
		if strings.Contains(lower, sig) {
			return ClaudeAuthRequiredMessage, true
		}
	}
	return "", false
}
