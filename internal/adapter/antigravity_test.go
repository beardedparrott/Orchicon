package adapter

// antigravity_test.go — the antigravity kind's registration, model-ref grammar and auth mapper.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAntigravityKindRegistered is AC 1: the kind is in BOTH the built-in catalog (which drives the
// ref-canonicalization cut and every totality guard) and the catalog-sourced list (which decides
// whether its model tier comes from the providers view rather than opencode-CLI discovery).
func TestAntigravityKindRegistered(t *testing.T) {
	if _, ok := BuiltinAdapterKinds()[KindAntigravity]; !ok {
		t.Fatalf("BuiltinAdapterKinds() does not contain %q — a kind absent from the catalog is invisible to "+
			"the mount guard, the bundle-conformance gate and the dev-seed guard", KindAntigravity)
	}
	found := false
	for _, k := range CatalogSourcedAdapterKinds() {
		if k == KindAntigravity {
			found = true
		}
	}
	if !found {
		t.Fatalf("CatalogSourcedAdapterKinds() = %v, want %q (agy's model tier resolves from the providers "+
			"catalog, not opencode-CLI discovery)", CatalogSourcedAdapterKinds(), KindAntigravity)
	}
	c := NewBuiltinProviderCatalog()
	if !c.IsKnownAdapter(KindAntigravity) {
		t.Fatalf("the built-in catalog does not know adapter kind %q", KindAntigravity)
	}
	if got, want := c.Providers(KindAntigravity), []string{"google"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Providers(%q) = %v, want %v — agy serves Gemini AND claude-* models under ONE Google "+
			"login, so `google` is its only provider", KindAntigravity, got, want)
	}
}

// TestAntigravityParseModelRef is AC 2: the grammar needs no change, only the kind to be KNOWN.
func TestAntigravityParseModelRef(t *testing.T) {
	reg := NewBuiltinProviderCatalog()
	got, err := ParseModelRef("antigravity/google/gemini-3.8-flash-medium", reg)
	if err != nil {
		t.Fatalf("ParseModelRef(antigravity/google/gemini-3.8-flash-medium) = error %v — the pinned grammar "+
			"needs no change for this kind, so an error means the kind is not known", err)
	}
	want := ModelRef{Adapter: KindAntigravity, Provider: "google", Model: "gemini-3.8-flash-medium"}
	if got != want {
		t.Fatalf("ParseModelRef(...) = %+v, want %+v", got, want)
	}
	// The kind is also reachable through the routing view (no validation) the dispatcher uses.
	if k := AdapterKind("antigravity/google/gemini-3.8-flash-medium"); k != KindAntigravity {
		t.Errorf("AdapterKind(...) = %q, want %q", k, KindAntigravity)
	}
	// A claude-* model under the same kind STAYS provider google: agy is the account, not Anthropic.
	if got, err := ParseModelRef("antigravity/google/claude-sonnet-4", reg); err != nil || got.Provider != "google" {
		t.Errorf("a claude-* model under antigravity must stay provider google (agy is the account); got %+v err=%v", got, err)
	}
}

// TestAntigravityAuthFailure is AC 6: the actionable message for auth-shaped text, false for
// everything else.
func TestAntigravityAuthFailure(t *testing.T) {
	positive := []string{
		"Error: Authentication required. Run `agy` and sign in.",
		`{"jsonrpc":"2.0","error":{"code":-32000,"message":"authentication required"}}`,
		"agy: not signed in",
		"invalid api key provided",
		"UNAUTHENTICATED",
	}
	for _, in := range positive {
		msg, ok := AntigravityAuthFailure(in)
		if !ok {
			t.Errorf("AntigravityAuthFailure(%q) = false, want the actionable message", in)
			continue
		}
		if msg != AntigravityAuthRequiredMessage {
			t.Errorf("AntigravityAuthFailure(%q) returned a message other than AntigravityAuthRequiredMessage", in)
		}
		for _, want := range []string{"agy", "GEMINI_API_KEY", "never signs in", "stores no Google credential"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the message must name %q — the operator needs the remedy, not just the verdict; got: %s", want, msg)
			}
		}
	}

	negative := []string{
		"",
		"file not found: /tmp/x",
		"session completed successfully",
		"rate limit exceeded, retry in 5s",
	}
	for _, in := range negative {
		if msg, ok := AntigravityAuthFailure(in); ok || msg != "" {
			t.Errorf("AntigravityAuthFailure(%q) = (%q, true), want (\"\", false) — an unrelated failure "+
				"must not be reported as a sign-in problem", in, msg)
		}
	}
}

// TestAntigravityAuthFailureMatchesAgysMissingKeyText is the gap this brief called out in §2: the signal
// list did not match the exact text agy emits when the gemini-api-key method is chosen with no key
// configured — `-32602 "GEMINI_API_KEY is not set; pass an API key in _meta.apiKey or set the GEMINI_API_KEY
// environment variable."` (testdata/acp/auth_required.jsonl:12). That sentence shares no wording with any
// other signal, so without an entry the one failure whose remedy is literally "set the key" surfaced as a
// generic error.
func TestAntigravityAuthFailureMatchesAgysMissingKeyText(t *testing.T) {
	// The fixture line, verbatim from agy 1.5.13's recording.
	const fixtureMessage = `GEMINI_API_KEY is not set; pass an API key in _meta.apiKey or set the GEMINI_API_KEY environment variable.`
	const fixtureEnvelope = `{"jsonrpc":"2.0","error":{"code":-32602,"message":"` + fixtureMessage + `"},"id":4}`

	// The unused fixture line is read so the test fails loudly if the recording is edited out from under
	// it, rather than silently asserting against a string that no longer exists.
	raw, err := os.ReadFile(filepath.Join("..", "antigravity", "testdata", "acp", "auth_required.jsonl"))
	if err != nil {
		t.Fatalf("read the auth_required recording: %v", err)
	}
	if !strings.Contains(string(raw), "GEMINI_API_KEY is not set") {
		t.Fatal("the auth_required recording no longer carries agy's missing-key text; this test's premise is gone")
	}

	for _, in := range []string{fixtureMessage, fixtureEnvelope, strings.ToLower(fixtureEnvelope)} {
		msg, ok := AntigravityAuthFailure(in)
		if !ok {
			t.Errorf("AntigravityAuthFailure(%q) = false — agy's missing-key failure must classify as auth", in)
			continue
		}
		if msg != AntigravityAuthRequiredMessage {
			t.Errorf("AntigravityAuthFailure(%q) returned a message other than AntigravityAuthRequiredMessage", in)
		}
	}

	// The other -32602 agy emits is a CLIENT BUG, not a credential problem: an unsupported methodId.
	// Matching the CODE instead of the message would misclassify it as "sign in", so this is the guard
	// that keeps the new signal narrow.
	if msg, ok := AntigravityAuthFailure(`{"jsonrpc":"2.0","error":{"code":-32602,"message":"Unsupported auth method: \"no-such-auth-method\""},"id":3}`); ok {
		t.Errorf("AntigravityAuthFailure classified an unsupported-methodId -32602 as auth (%q) — -32602 is "+
			"JSON-RPC 'invalid params' and must not be treated as a credential signal", msg)
	}
}

// TestAntigravityBinaryDiscovery is AC 1's discovery half: the env override is named, and the
// DECLARED install location resolves — the kind's executable is `agy`, not `antigravity`.
func TestAntigravityBinaryDiscovery(t *testing.T) {
	if got := BinEnv(KindAntigravity); got != "ORCHICON_AGY_BIN" {
		t.Fatalf("BinEnv(%q) = %q, want ORCHICON_AGY_BIN", KindAntigravity, got)
	}

	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A PATH that cannot find `agy`, so the DECLARED location is what resolves.
	env := func(k string) string {
		if k == "PATH" {
			return t.TempDir()
		}
		return ""
	}
	if got := ResolveBinary("linux", home, KindAntigravity, env); got != bin {
		t.Fatalf("ResolveBinary(antigravity) = %q, want the declared install %q", got, bin)
	}
	// The override wins over the declared location.
	override := filepath.Join(home, "custom-agy")
	if err := os.WriteFile(override, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envWithOverride := func(k string) string {
		switch k {
		case "PATH":
			return t.TempDir()
		case BinEnv(KindAntigravity):
			return override
		}
		return ""
	}
	if got := ResolveBinary("linux", home, KindAntigravity, envWithOverride); got != override {
		t.Fatalf("ResolveBinary(antigravity) = %q, want the ORCHICON_AGY_BIN override %q", got, override)
	}
	// The Windows executable name is agy.exe, NOT antigravity.exe.
	if got := binaryName("windows", KindAntigravity); got != "agy.exe" {
		t.Fatalf("binaryName(windows, antigravity) = %q, want agy.exe", got)
	}
	// A POSIX host also probes the system locations, so an install outside the home dir is found.
	usr := "/usr/local/bin"
	if _, err := os.Stat(usr); err == nil {
		if dirs := extraBinDirs("linux", home, KindAntigravity, env); len(dirs) == 0 {
			t.Error("extraBinDirs(linux, antigravity) is empty — an agy installed in /usr/local/bin would be reported 'not found'")
		}
	}
	if dirs := extraBinDirs("linux", home, KindClaude, env); dirs != nil {
		t.Errorf("extraBinDirs(linux, claude) = %v, want nil — only antigravity gained POSIX system dirs", dirs)
	}
}
