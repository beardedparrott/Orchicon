package runtime

// antigravity_gate_test.go — a container dispatch that cannot authenticate must be refused UP
// FRONT, with the message that tells the operator what to do.
//
// WHY THIS IS A TEST AND NOT A COMMENT. The preflight runs before a container exists, so getting it
// wrong is invisible in a healthy-looking create: an oauth-personal run would start a container,
// fail its `authenticate` call with agy's generic -32000 "Authentication required", and surface as
// an opaque session failure. The gate is also the ONLY place the container rule lives, which is what
// makes it testable without an engine.

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

func antigravityRequest(method string, secrets map[string]string) CreateRequest {
	return CreateRequest{
		AdapterKinds:          []string{adapter.KindAntigravity},
		AntigravityAuthMethod: method,
		Secrets:               secrets,
	}
}

// AC3: oauth-personal in container mode fails fast with the EXACT actionable message.
func TestAntigravityPreflightRefusesPersonalOAuthInContainer(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "") // the host keyring is not reachable from a container anyway
	d := &Daemon{}

	for name, method := range map[string]string{
		"explicit oauth-personal": authmethod.MethodOAuthPersonal,
		// An older plane that does not send the field is treated as the default — which IS
		// oauth-personal — so it must be refused rather than allowed through unchecked.
		"empty (old client)": "",
	} {
		err := d.antigravityPreflight(antigravityRequest(method, map[string]string{"GEMINI_API_KEY": "present-but-useless"}))
		if err == nil {
			t.Fatalf("%s: preflight allowed oauth-personal into a container", name)
		}
		if err.Error() != authmethod.ContainerOAuthPersonalRefusal {
			t.Fatalf("%s: error = %q, want the exact actionable refusal", name, err.Error())
		}
		// The message has to name the way out, not just the failure.
		for _, want := range []string{"host keyring", "runtime container", "Gemini API key", "run this item locally"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q does not mention %q", name, err.Error(), want)
			}
		}
	}
}

// The API-key method is the container-capable one: with the secret on the request it passes.
func TestAntigravityPreflightAllowsAPIKeyWithSecret(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	d := &Daemon{}
	if err := d.antigravityPreflight(antigravityRequest(authmethod.MethodGeminiAPIKey,
		map[string]string{authmethod.APIKeySecretName: "key"})); err != nil {
		t.Fatalf("preflight refused gemini-api-key with the secret on the request: %v", err)
	}
	// …and without one it is refused with the shared actionable message, not silently started.
	err := d.antigravityPreflight(antigravityRequest(authmethod.MethodGeminiAPIKey, nil))
	if err == nil {
		t.Fatal("preflight allowed gemini-api-key with no key")
	}
	if err.Error() != adapter.AntigravityAuthRequiredMessage {
		t.Fatalf("error = %q, want AntigravityAuthRequiredMessage", err.Error())
	}
}

// A run that does not demand antigravity is untouched by any of this.
func TestAntigravityPreflightIgnoresOtherKinds(t *testing.T) {
	d := &Daemon{}
	req := CreateRequest{AdapterKinds: []string{"claude"}, AntigravityAuthMethod: authmethod.MethodOAuthPersonal}
	if err := d.antigravityPreflight(req); err != nil {
		t.Fatalf("preflight refused a non-antigravity run: %v", err)
	}
	if err := d.antigravityPreflight(CreateRequest{}); err != nil {
		t.Fatalf("preflight refused an empty boot profile: %v", err)
	}
}

// AC3, the "fails fast" half: the refusal happens BEFORE the container engine is touched at all.
// A gate that ran after `engine run` would still fail, but only after creating (or half-creating) a
// container, which is the difference between an actionable error and a leaked runtime.
func TestAntigravityPreflightRunsBeforeAnyEngineCall(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	engineCalls := 0
	d := &Daemon{
		Paths: NewPathMapper("linux", ""),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		engineFn: func(args ...string) (string, error) {
			engineCalls++
			return "", nil
		},
	}
	_, err := d.createContainerInner("probe", antigravityRequest(authmethod.MethodOAuthPersonal, nil))
	if err == nil {
		t.Fatal("createContainerInner created a container for an oauth-personal antigravity run")
	}
	if err.Error() != authmethod.ContainerOAuthPersonalRefusal {
		t.Fatalf("error = %q, want the exact actionable refusal", err.Error())
	}
	if engineCalls != 0 {
		t.Fatalf("the container engine was called %d time(s) before the refusal; the gate must run first", engineCalls)
	}
}
