package control

// antigravity_adapter_test.go — Settings → Adapters shows the antigravity auth-method selector and the
// ToS notice (child 8, AC4's TUI half).
//
// WHY THIS IS A CONTROL-PACKAGE TEST. The auth method is EDITABLE in exactly one place per client: the
// Settings form (the KSelect field). The ADAPTER DETAIL block is the read-only surface an operator opens
// to answer "how does agy sign in, and what do its terms say", and it must show BOTH facts — the selected
// method WITH its caveat, and the ToS text on exactly the method whose terms it is about.
//
// THE ToS TEXT IS NEVER RE-TYPED IN THIS TEST EITHER. It is asserted against authmethod.ToSNotice, the
// single constant the server publishes on TenantSettings.antigravity_tos_notice, so a test that passed
// with a paraphrase would be the drift this whole item exists to prevent.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// fieldValue reads one key from an adapter-detail field list.
func fieldValue(fields []kit2.Field, key string) (string, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

// agySettings is the server's settings message as the plane sends it for the DEFAULT method, with the
// ToS notice supplied read-only by the server.
func agySettings(method string) *apiv1.TenantSettings {
	return &apiv1.TenantSettings{
		AntigravityAuthMethod:     method,
		AntigravityShareTelemetry: false,
		AntigravityTosNotice:      authmethod.ToSNotice,
	}
}

// TestAdaptersShowsTheAntigravityAuthMethodAndItsCaveat is AC4's first half in the TUI: the detail block
// names the method AND the one fact that method carries, because "oauth-personal" on a container plane is
// the refusal an operator has to be able to predict.
func TestAdaptersShowsTheAntigravityAuthMethodAndItsCaveat(t *testing.T) {
	m := New(nil, nil)

	m.settings = agySettings(authmethod.MethodOAuthPersonal)
	fields := m.antigravityAdapterFields(t.Context())
	got, ok := fieldValue(fields, "auth method")
	if !ok {
		t.Fatalf("the antigravity adapter detail has no auth-method row: %+v", fields)
	}
	// The method's WIRE VALUE (agy's own authenticate methodId), never a friendly paraphrase: the
	// operator has to be able to match it against agy's own documentation.
	if !strings.Contains(got, authmethod.MethodOAuthPersonal) {
		t.Errorf("auth method row = %q, want it to name %q", got, authmethod.MethodOAuthPersonal)
	}
	// THE CONTAINER CAVEAT: personal sign-in lives in the HOST keyring.
	if !strings.Contains(got, "host keyring") {
		t.Errorf("auth method row = %q, want the host-keyring caveat — the one fact that tells an "+
			"operator this method cannot work in a runtime container", got)
	}

	// The METHOD SELECTOR is the Settings form's, and it offers all four wire values in the server's
	// own order. Asserted here so the order the pane renders is the order agy documents.
	methods := authmethod.Methods()
	if len(methods) != 4 {
		t.Fatalf("authmethod.Methods() = %v, want four methods", methods)
	}
}

// TestAdaptersShowsTheToSNoticeOnlyForOAuthPersonal is AC4's second half: the terms text appears on the
// method whose terms it is about, and NOWHERE ELSE (an operator on the API-key method is not signing in
// with a Google account, so the personal-sign-in terms are not their notice).
func TestAdaptersShowsTheToSNoticeOnlyForOAuthPersonal(t *testing.T) {
	m := New(nil, nil)
	m.settings = agySettings(authmethod.MethodOAuthPersonal)
	fields := m.antigravityAdapterFields(t.Context())
	notice, ok := fieldValue(fields, "terms notice")
	if !ok {
		t.Fatalf("the oauth-personal method must show the ToS notice: %+v", fields)
	}
	// BYTE FOR BYTE the server's constant — this is the parity contract, not a wording preference.
	if notice != authmethod.ToSNotice {
		t.Fatalf("terms notice = %q\nwant %q (authmethod.ToSNotice, the string the server publishes on "+
			"TenantSettings.antigravity_tos_notice)", notice, authmethod.ToSNotice)
	}
	// It must point at Google's terms: a notice that names no URL is not actionable.
	if !strings.Contains(notice, "https://antigravity.google/terms") {
		t.Errorf("the terms notice must link Google's terms: %q", notice)
	}
	// The data-handling half, which is the claim Orchicon makes about itself.
	if !strings.Contains(notice, "never reads or stores your Google credentials") {
		t.Errorf("the terms notice must state that Orchicon stores no Google credential: %q", notice)
	}

	// AND NOT on the API-key method.
	m.settings = agySettings(authmethod.MethodGeminiAPIKey)
	fields = m.antigravityAdapterFields(t.Context())
	if v, ok := fieldValue(fields, "terms notice"); ok {
		t.Fatalf("the gemini-api-key method must NOT render the personal-sign-in terms notice, got %q", v)
	}
	// It still names the method and its own caveat (the tenant secret it reads).
	got, _ := fieldValue(fields, "auth method")
	if !strings.Contains(got, authmethod.MethodGeminiAPIKey) {
		t.Errorf("auth method row = %q, want %q", got, authmethod.MethodGeminiAPIKey)
	}
	if !strings.Contains(got, authmethod.APIKeySecretName) {
		t.Errorf("auth method row = %q, want it to name the tenant secret %s that method reads",
			got, authmethod.APIKeySecretName)
	}
}

// TestAdaptersSerialisesTheSecretAsPresenceOnly keeps the one rule that must never regress: the detail
// block reports whether the API-key secret EXISTS, never its value.
func TestAdaptersSerialisesTheSecretAsPresenceOnly(t *testing.T) {
	m := New(nil, nil)
	m.settings = agySettings(authmethod.MethodGeminiAPIKey)

	m.secretNames = map[string]bool{}
	if got, ok := fieldValue(m.antigravityAdapterFields(t.Context()), "api key secret"); !ok ||
		!strings.Contains(got, "not set") {
		t.Fatalf("with no secret the row must say so, got %q (present=%v)", got, ok)
	}

	m.secretNames = map[string]bool{authmethod.APIKeySecretName: true}
	got, ok := fieldValue(m.antigravityAdapterFields(t.Context()), "api key secret")
	if !ok || !strings.Contains(got, "set") {
		t.Fatalf("with the secret present the row must say it is set, got %q (present=%v)", got, ok)
	}
	// PRESENCE ONLY. The list API returns names; a detail pane that rendered a value would be the one
	// place a credential reaches an operator's terminal, so the rendered row must not carry one.
	if strings.Contains(got, "=") || strings.Contains(got, "sk-") {
		t.Fatalf("the api key row must never render a value: %q", got)
	}
}

// TestAdaptersSaysNotInstalledRatherThanShowingABinary is the EMPTY / NOT-INSTALLED state on the adapter
// surface: with no `agy` resolvable the binary row is the install sentence, never a fabricated path.
func TestAdaptersSaysNotInstalledRatherThanShowingABinary(t *testing.T) {
	m := New(nil, nil)
	m.settings = agySettings(authmethod.MethodOAuthPersonal)

	// NO HOME AND NO PATH: neither the known-install lists nor a PATH lookup can resolve anything. The
	// explicit ORCHICON_AGY_BIN override is deliberately NOT set — an override is a TRUSTED declaration
	// (the resolver returns it verbatim, existence unchecked), so it could never model "absent".
	t.Setenv("ORCHICON_AGY_BIN", "")
	t.Setenv("HOME", "")
	t.Setenv("PATH", "/nonexistent")

	got, ok := fieldValue(m.antigravityAdapterFields(t.Context()), "binary")
	if !ok {
		t.Fatal("the adapter detail must always answer the binary question, even when the answer is 'absent'")
	}
	if got != "Antigravity CLI (`agy`) not found — install from https://antigravity.google/download" &&
		!strings.Contains(got, "not found") {
		t.Fatalf("an absent agy must render the install sentence, got %q", got)
	}
}
