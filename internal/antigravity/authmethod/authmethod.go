// Package authmethod is the single source of truth for the Antigravity (agy) ACP
// authentication methods, their validation, and the operator-facing text that goes
// with them.
//
// WHY A LEAF PACKAGE. The method value crosses four layers - the tenant_settings
// column and its CHECK constraint, the SettingsService RPC / MCP settings tool, the
// daemon's container preflight, and the `agy acp` spawn spec - and a string typed
// once per layer is a value that drifts silently. It imports NOTHING internal, so
// every one of those layers can depend on it without an import cycle.
//
// THE METHODS ARE AN OPERATOR CHOICE, NOT A CONSTANT, for three separate reasons:
//
//   - oauth-personal  reuses the CLI's cached interactive login. That credential is
//     the operator's personal Google sign-in, and Google's Antigravity terms restrict
//     using the service with third-party products - a per-operator decision this
//     product must not make on their behalf, hence the ToSNotice constant and a
//     DEFAULT of the method the operator most likely already has working.
//   - gemini-api-key  is the only method that works inside a runtime container: the
//     personal credential lives in the HOST OS KEYRING and cannot be bind-mounted.
//   - oauth-business / agent-platform are the enterprise paths (Google Workspace
//     tenancy, and the agent-platform credential plane respectively).
package authmethod

import "fmt"

// The four methods an operator may select. These strings are ALSO the ACP
// `authenticate {methodId}` values agy expects, so they must not be renamed to
// sound nicer.
const (
	MethodOAuthPersonal = "oauth-personal"
	MethodOAuthBusiness = "oauth-business"
	MethodGeminiAPIKey  = "gemini-api-key"
	MethodAgentPlatform = "agent-platform"
)

// Default is what a tenant that has never chosen gets. It matches agy's own default
// and is the method whose cached credential already works on an operator's machine.
const Default = MethodOAuthPersonal

// Methods lists the allowed values in the order the UI should present them.
func Methods() []string {
	return []string{MethodOAuthPersonal, MethodOAuthBusiness, MethodGeminiAPIKey, MethodAgentPlatform}
}

// APIKeySecretName is the tenant secret the gemini-api-key method reads. The name IS
// the environment variable agy itself reads, so the secret travels under the same
// name it is passed to the child as.
const APIKeySecretName = "GEMINI_API_KEY"

// ToSNotice is the single source of the Antigravity terms notice. It is exported and
// surfaced on the API (TenantSettings.antigravity_tos_notice) so the web app, the TUI
// and the desktop app render the SAME words rather than three paraphrases of them.
const ToSNotice = "Antigravity personal sign-in is used through Google's own `agy` binary. " +
	"Google's Antigravity terms restrict using the service with third-party products; " +
	"review https://antigravity.google/terms before enabling. " +
	"Orchicon never reads or stores your Google credentials."

// ContainerOAuthPersonalRefusal is the exact, actionable error a container-mode
// dispatch gets when the tenant selected oauth-personal. It names WHY (the credential
// is in the host keyring), WHAT to do (switch to the API-key method), and the
// alternative (run the item locally) - a refusal that names none of those is a dead
// end for the operator.
const ContainerOAuthPersonalRefusal = "personal Google sign-in lives in the host keyring and " +
	"cannot be used inside a runtime container — set the Antigravity auth method to " +
	"Gemini API key, or run this item locally"

// Valid reports whether m is one of the four allowed methods (case-sensitively: these
// are wire values, and "OAuth-Personal" is not one of them).
func Valid(m string) bool {
	for _, allowed := range Methods() {
		if m == allowed {
			return true
		}
	}
	return false
}

// Normalize maps the empty string - what an old client, a fresh INSERT, or a caller
// that has nothing to say about the method sends - onto Default. Every other value is
// returned untouched so Validate can reject it.
func Normalize(m string) string {
	if m == "" {
		return Default
	}
	return m
}

// Validate rejects a method that is not one of the four, and says which ones are.
func Validate(m string) error {
	if m == "" || Valid(m) {
		return nil
	}
	return fmt.Errorf("antigravity_auth_method %q is not valid; use one of %v (default %q)", m, Methods(), Default)
}

// MethodID is the value to send in the ACP `authenticate` request's methodId. It is
// the stored method itself, normalized; it exists so callers do not each write their
// own `if m == ""` fallback.
func MethodID(m string) string { return Normalize(m) }

// NeedsAPIKey reports whether the method authenticates with the tenant GEMINI_API_KEY
// secret (as opposed to a host-keyring/OAuth credential).
func NeedsAPIKey(m string) bool { return Normalize(m) == MethodGeminiAPIKey }
