package adapter

import "strings"

// Anthropic credential provisioning is HOST-SIDE, never Orchicon-side.
//
// The Claude adapter kind (provider "anthropic") authenticates with the
// OPERATOR'S OWN host account: the operator signs in on the host with the
// `claude` CLI (first-run browser login, `/login`, or `claude auth login`) or
// sets ANTHROPIC_API_KEY in the host environment. Orchicon never presents a
// claude.ai login, never stores/bakes/redistributes an Anthropic credential,
// and never caps or resells Anthropic rate limits. Nothing in the Orchicon UI
// or API is an Anthropic auth surface.
//
// This file owns the ONE canonical, actionable auth-failure vocabulary, so
// every surface (the daemon's create preflight and, later, the claude bridge's
// execution-time errors) reports the same instruction instead of a silent hang
// or a misleading generic transport error.

// ClaudeAuthRequiredMessage is the single actionable auth-failure string for
// the claude adapter kind. It names the exact host-side remedy so the operator
// is never left guessing.
const ClaudeAuthRequiredMessage = "Claude is not authenticated on the host — run `claude` and sign in on the host, or set ANTHROPIC_API_KEY on the host, then re-run"

// claudeAuthSignals are the case-insensitive substrings that mark a
// claude-CLI / transport failure as an AUTHENTICATION failure. They are
// deliberately narrow: a generic failure (a compile error, a tool error, a
// rate limit) must NOT be rewritten into the sign-in instruction, which would
// mislead the operator.
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

// ClaudeAuthFailure classifies text (a claude CLI or transport failure string)
// as an authentication failure and, when it is one, returns the canonical
// actionable message. A non-auth (or empty) text returns ("", false), so the
// caller surfaces the original error unchanged.
//
// The claude bridge MUST route auth failures through this function so an
// expired/absent host login surfaces the sign-in instruction everywhere —
// never as a silent hang and never as a wrong generic error.
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
