package authmethod_test

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

// The defaults are a CONTRACT (fresh tenant = oauth-personal), not an implementation
// detail: an accidental change here silently opts every tenant into a different
// Google sign-in path.
func TestDefaults(t *testing.T) {
	if authmethod.Default != authmethod.MethodOAuthPersonal {
		t.Fatalf("Default = %q, want oauth-personal", authmethod.Default)
	}
	if got := authmethod.Normalize(""); got != authmethod.MethodOAuthPersonal {
		t.Fatalf("Normalize(\"\") = %q, want oauth-personal", got)
	}
	if authmethod.APIKeySecretName != "GEMINI_API_KEY" {
		t.Fatalf("APIKeySecretName = %q", authmethod.APIKeySecretName)
	}
}

func TestValidAndValidate(t *testing.T) {
	for _, m := range authmethod.Methods() {
		if !authmethod.Valid(m) {
			t.Errorf("Valid(%q) = false, want true", m)
		}
		if err := authmethod.Validate(m); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", m, err)
		}
	}
	// Empty is VALID-but-unset everywhere: the DB layer normalizes it to the default and
	// the RPC treats it as "leave unchanged".
	if err := authmethod.Validate(""); err != nil {
		t.Errorf("Validate(\"\") = %v, want nil", err)
	}
	for _, bad := range []string{"personal", "oauth", "OAUTH-PERSONAL", "api-key", "gemini_api_key"} {
		if authmethod.Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
		err := authmethod.Validate(bad)
		if err == nil {
			t.Fatalf("Validate(%q) = nil, want an error naming the allowed values", bad)
		}
		for _, want := range append(authmethod.Methods(), "not valid") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Validate(%q) error %q does not mention %q", bad, err, want)
			}
		}
	}
}

func TestNeedsAPIKeyAndMethodID(t *testing.T) {
	if !authmethod.NeedsAPIKey(authmethod.MethodGeminiAPIKey) {
		t.Fatal("gemini-api-key must need the tenant API-key secret")
	}
	for _, m := range []string{"", authmethod.MethodOAuthPersonal, authmethod.MethodOAuthBusiness, authmethod.MethodAgentPlatform} {
		if authmethod.NeedsAPIKey(m) {
			t.Errorf("NeedsAPIKey(%q) = true, want false", m)
		}
		if got := authmethod.MethodID(m); got == "" {
			t.Errorf("MethodID(%q) is empty; the ACP authenticate call would carry no method", m)
		}
	}
	if got := authmethod.MethodID(""); got != authmethod.MethodOAuthPersonal {
		t.Fatalf("MethodID(\"\") = %q, want the default", got)
	}
}

// The two operator-facing strings are asserted VERBATIM: the ToS text is a legal
// notice shown before an operator accepts Google's terms, and the refusal is quoted in
// acceptance criteria. A "clarifying" edit silently changes both.
func TestOperatorFacingText(t *testing.T) {
	const wantToS = "Antigravity personal sign-in is used through Google's own `agy` binary. " +
		"Google's Antigravity terms restrict using the service with third-party products; " +
		"review https://antigravity.google/terms before enabling. " +
		"Orchicon never reads or stores your Google credentials."
	if authmethod.ToSNotice != wantToS {
		t.Errorf("ToSNotice changed.\n got: %s\nwant: %s", authmethod.ToSNotice, wantToS)
	}
	const wantRefusal = "personal Google sign-in lives in the host keyring and cannot be used " +
		"inside a runtime container — set the Antigravity auth method to Gemini API key, " +
		"or run this item locally"
	if authmethod.ContainerOAuthPersonalRefusal != wantRefusal {
		t.Errorf("ContainerOAuthPersonalRefusal changed.\n got: %s\nwant: %s",
			authmethod.ContainerOAuthPersonalRefusal, wantRefusal)
	}
}
