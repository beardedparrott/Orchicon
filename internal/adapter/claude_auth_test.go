package adapter

import (
	"strings"
	"testing"
)

// TestClaudeAuthFailureClassifiesAuthErrors pins the canonical vocabulary: an auth-looking claude
// CLI/transport failure is rewritten to the ONE actionable host sign-in message.
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

// TestClaudeAuthRequiredMessageActionable pins the message contents: the host-side remedy (run
// `claude` and sign in), the ANTHROPIC_API_KEY fallback, AND the container-mode explanation.
func TestClaudeAuthRequiredMessageActionable(t *testing.T) {
	for _, want := range []string{
		"not authenticated", "host", "claude", "ANTHROPIC_API_KEY",
		// The container-mode half.
		"container", "mounted", "never signs in for you",
	} {
		if !strings.Contains(ClaudeAuthRequiredMessage, want) {
			t.Errorf("ClaudeAuthRequiredMessage %q is missing %q", ClaudeAuthRequiredMessage, want)
		}
	}
}
