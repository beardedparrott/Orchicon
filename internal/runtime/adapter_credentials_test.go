package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeAuthStore writes an opencode credential store into a temp home. Values
// are DELIBERATELY fake: this asserts the TRANSLATION (provider id -> env var),
// which is the part that can be wrong, not any real credential.
func writeAuthStore(t *testing.T, home, body string) {
	t.Helper()
	p := openCodeAuthPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAdapterCredentialEnvTranslatesTheStore pins the translation of the
// operator's opencode store into container environment variables: api-key
// entries with a known provider become KEY=VALUE, and everything else is
// REPORTED rather than silently dropped (an oauth entry, an id with no known
// env var, an empty key). The ordering is asserted because a map-ordered argv
// would make the emitted docker arguments non-deterministic.
func TestAdapterCredentialEnvTranslatesTheStore(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{
  "openrouter":       {"type": "api",   "key": "sk-or-FAKE"},
  "deepseek":         {"type": "api",   "key": "sk-ds-FAKE"},
  "ollama-cloud":     {"type": "api",   "key": "ol-FAKE"},
  "some-oauth":       {"type": "oauth", "access": "a", "refresh": "r"},
  "mystery-provider": {"type": "api",   "key": "mk-FAKE"},
  "empty-key":        {"type": "api",   "key": ""}
}`)

	got, unusable := adapterCredentialEnv(home, "opencode")
	want := []string{
		"DEEPSEEK_API_KEY=sk-ds-FAKE",
		"OLLAMA_API_KEY=ol-FAKE",
		"OPENROUTER_API_KEY=sk-or-FAKE",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("credential env = %v, want %v", got, want)
	}
	wantUnusable := []string{"empty-key", "mystery-provider", "some-oauth"}
	if !reflect.DeepEqual(unusable, wantUnusable) {
		t.Errorf("unusable = %v, want %v (a provider that cannot be delivered through the "+
			"environment must be REPORTED, never silently dropped)", unusable, wantUnusable)
	}
}

// TestAdapterCredentialEnvIsGatedAndForgiving pins the three cases that must be
// a quiet no-op, because each is a NORMAL state rather than a fault: a native
// only run (its adapter does not read these), a host that has never signed in,
// and a store that is not valid JSON. Failing loudly on any of them would turn
// an ordinary native-only run into an error.
func TestAdapterCredentialEnvIsGatedAndForgiving(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{"openrouter": {"type": "api", "key": "sk-or-FAKE"}}`)

	if env, bad := adapterCredentialEnv(home, "orchicon"); env != nil || bad != nil {
		t.Errorf("a native-only demand must carry no adapter credentials, got env=%v unusable=%v", env, bad)
	}
	if env, bad := adapterCredentialEnv(t.TempDir(), "opencode"); env != nil || bad != nil {
		t.Errorf("a host with no opencode store must yield nothing, got env=%v unusable=%v", env, bad)
	}
	if env, bad := adapterCredentialEnv("", "opencode"); env != nil || bad != nil {
		t.Errorf("no host home must yield nothing, got env=%v unusable=%v", env, bad)
	}
	broken := t.TempDir()
	writeAuthStore(t, broken, "not json at all")
	if env, bad := adapterCredentialEnv(broken, "opencode"); env != nil || bad != nil {
		t.Errorf("an unparseable store must yield nothing, got env=%v unusable=%v", env, bad)
	}
}

// TestStandardHostMountArgsCarryAdapterCredentials is the DAEMON-level half:
// the pairs the resolver produces must actually reach `docker run` as `-e` for
// an opencode-demanding run, and must NOT appear for a native-only one. This is
// the edge that would otherwise be easy to get right in isolation and forget to
// wire — credentials resolved but never passed.
func TestStandardHostMountArgsCarryAdapterCredentials(t *testing.T) {
	home, d := bootProfileTestHome(t)
	writeAuthStore(t, home, `{"deepseek": {"type": "api", "key": "sk-ds-FAKE"}}`)

	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})
	if !envValue(args, "DEEPSEEK_API_KEY", "sk-ds-FAKE") {
		t.Errorf("an opencode run must receive the provider credential as env (args %v)", args)
	}

	native := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"orchicon"}, GitStrategy: "local"})
	for i, a := range native {
		if a == "-e" && i+1 < len(native) && len(native[i+1]) > len("DEEPSEEK_API_KEY=") &&
			native[i+1][:len("DEEPSEEK_API_KEY=")] == "DEEPSEEK_API_KEY=" {
			t.Errorf("a native-only run must NOT receive opencode provider credentials: %v", native)
		}
	}
}
