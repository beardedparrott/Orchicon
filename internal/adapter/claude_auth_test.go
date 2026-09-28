package adapter

import (
	"strings"
	"testing"
)

// TestClaudeAuthFailureClassifiesAuthErrors pins the canonical vocabulary:
// an auth-looking claude CLI/transport failure is rewritten to the ONE
// actionable host sign-in message, and a non-auth failure is left untouched
// (so a compile error or a rate limit is never misreported as "not signed in").
func TestClaudeAuthFailureClassifiesAuthErrors(t *testing.T) {
	authLooking := []string{
		"Invalid API key",
		"Please run `claude` to authenticate",
		"please run claude and sign in",
		"authentication_error",
		"API Error: 401 Unauthorized",
		"OAuth token has expired",
		"Authentication failed",
		"Your API key is invalid",
	}
	for _, in := range authLooking {
		got, ok := ClaudeAuthFailure(in)
		if !ok || got != ClaudeAuthRequiredMessage {
			t.Errorf("ClaudeAuthFailure(%q) = (%q, %v), want the canonical message + true", in, got, ok)
		}
	}

	benign := []string{
		"",
		"Compilation failed: undefined: foo",
		"Error: tool use exceeded max iterations",
		"rate limit exceeded",
		"the process exited with status 3",
	}
	for _, in := range benign {
		got, ok := ClaudeAuthFailure(in)
		if ok || got != "" {
			t.Errorf("ClaudeAuthFailure(%q) = (%q, %v), want (\"\", false)", in, got, ok)
		}
	}
}

// TestClaudeAuthRequiredMessageActionable pins the message contents the
// acceptance criteria require: it must name the host-side remedy (run
// `claude` and sign in) and the ANTHROPIC_API_KEY fallback.
func TestClaudeAuthRequiredMessageActionable(t *testing.T) {
	for _, want := range []string{"not authenticated", "host", "claude", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(ClaudeAuthRequiredMessage, want) {
			t.Errorf("ClaudeAuthRequiredMessage %q is missing %q", ClaudeAuthRequiredMessage, want)
		}
	}
}
