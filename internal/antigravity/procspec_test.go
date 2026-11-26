package antigravity_test

// procspec_test.go — the spawn spec is where the auth method, the API key and the telemetry
// preference become a real child invocation, so it is where they must be PROVEN correct:
//
//   AC2  the key reaches the child as GEMINI_API_KEY in its ENVIRONMENT and appears nowhere
//        on the command line, and nothing that formats or logs the spec can print it.
//   AC4  --disable-telemetry is in the argv by default and absent when the tenant opted in.

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

const testAPIKey = "sekret-gemini-key-123"

func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

// AC4: telemetry is OFF unless the tenant opted in.
func TestSpawnSpecDisablesTelemetryByDefault(t *testing.T) {
	for _, method := range []string{"", authmethod.MethodOAuthPersonal, authmethod.MethodGeminiAPIKey} {
		spec, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: method}, testAPIKey, nil)
		if err != nil {
			t.Fatalf("BuildSpawnSpec(%q): %v", method, err)
		}
		if !hasArg(spec.Argv, "--disable-telemetry") {
			t.Fatalf("argv %v is missing --disable-telemetry (method %q); telemetry must ship OFF", spec.Argv, method)
		}
		if !hasArg(spec.Argv, "acp") {
			t.Fatalf("argv %v is missing the acp subcommand", spec.Argv)
		}
	}
	// The opt-in removes the flag — it must not merely add a second, contradictory one.
	spec, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: authmethod.MethodOAuthPersonal, ShareTelemetry: true}, "", nil)
	if err != nil {
		t.Fatalf("BuildSpawnSpec(share telemetry): %v", err)
	}
	if hasArg(spec.Argv, "--disable-telemetry") {
		t.Fatalf("argv %v still carries --disable-telemetry after the tenant opted IN to telemetry", spec.Argv)
	}
}

// AC2: the key is in the ENVIRONMENT, never on argv.
func TestSpawnSpecPassesAPIKeyOnlyInEnv(t *testing.T) {
	baseEnv := []string{"PATH=/usr/bin", "HOME=/root", authmethod.APIKeySecretName + "=stale-inherited-key"}
	spec, err := antigravity.BuildSpawnSpec("/usr/local/bin/agy", antigravity.SpawnSettings{
		AuthMethod: authmethod.MethodGeminiAPIKey,
	}, testAPIKey, baseEnv)
	if err != nil {
		t.Fatalf("BuildSpawnSpec: %v", err)
	}
	if got, ok := envValue(spec.Env, authmethod.APIKeySecretName); !ok || got != testAPIKey {
		t.Fatalf("child env GEMINI_API_KEY = %q (present=%v), want the tenant secret", got, ok)
	}
	// The inherited value must be GONE, not merely shadowed.
	count := 0
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, authmethod.APIKeySecretName+"=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("child env has %d GEMINI_API_KEY entries, want exactly 1", count)
	}
	for _, arg := range spec.Argv {
		if strings.Contains(arg, testAPIKey) || strings.Contains(arg, "stale-inherited-key") {
			t.Fatalf("argv element %q carries the API key; it must travel only in the environment", arg)
		}
	}
	if spec.AuthMethod != authmethod.MethodGeminiAPIKey {
		t.Fatalf("AuthMethod = %q, want gemini-api-key (this is what authenticate sends)", spec.AuthMethod)
	}
	// The client's spawn request must carry the same env, and the same absence on argv.
	proc := spec.ProcSpec()
	if got, ok := envValue(proc.Env, authmethod.APIKeySecretName); !ok || got != testAPIKey {
		t.Fatalf("acp.ProcSpec env lost the key")
	}
	for _, arg := range proc.Argv {
		if strings.Contains(arg, testAPIKey) {
			t.Fatalf("acp.ProcSpec argv carries the key %q", arg)
		}
	}
}

// The API key must not be printable through ANY of the ways a value reaches a log.
func TestSpawnSpecRedactsAPIKeyEverywhere(t *testing.T) {
	spec, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: authmethod.MethodGeminiAPIKey}, testAPIKey, nil)
	if err != nil {
		t.Fatalf("BuildSpawnSpec: %v", err)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", spec),
		fmt.Sprintf("%+v", spec),
		fmt.Sprintf("%s", spec),
		fmt.Sprint(spec),
		spec.String(),
	} {
		if strings.Contains(rendered, testAPIKey) {
			t.Fatalf("formatting the spec leaked the key: %q", rendered)
		}
	}
	// …and through a real structured logger, which is what the plane writes.
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("spawning agy", "spec", spec, "auth", spec.AuthMethod)
	if strings.Contains(buf.String(), testAPIKey) {
		t.Fatalf("slog output leaked the key: %s", buf.String())
	}
	// The redaction must not be so total that the log is useless: the variable NAME and the
	// argv are what an operator needs.
	if !strings.Contains(buf.String(), authmethod.APIKeySecretName) || !strings.Contains(buf.String(), "acp") {
		t.Fatalf("slog output dropped the env NAMES or argv, which the redaction must keep: %s", buf.String())
	}
}

// A non-API-key method must not inherit a key from the environment: an env key present on the
// plane would otherwise silently authenticate a method the operator did not choose.
func TestSpawnSpecStripsKeyForOAuthMethods(t *testing.T) {
	baseEnv := []string{"PATH=/usr/bin", authmethod.APIKeySecretName + "=" + testAPIKey}
	for _, method := range []string{"", authmethod.MethodOAuthPersonal, authmethod.MethodOAuthBusiness, authmethod.MethodAgentPlatform} {
		spec, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: method}, testAPIKey, baseEnv)
		if err != nil {
			t.Fatalf("BuildSpawnSpec(%q): %v", method, err)
		}
		if _, ok := envValue(spec.Env, authmethod.APIKeySecretName); ok {
			t.Fatalf("method %q inherited GEMINI_API_KEY; only gemini-api-key may carry it", method)
		}
		if spec.AuthMethod != authmethod.MethodID(method) {
			t.Fatalf("AuthMethod = %q, want %q", spec.AuthMethod, authmethod.MethodID(method))
		}
	}
}

func TestSpawnSpecRequiresKeyForAPIKeyMethod(t *testing.T) {
	_, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: authmethod.MethodGeminiAPIKey}, "   ", nil)
	if err == nil {
		t.Fatal("BuildSpawnSpec accepted gemini-api-key with no secret; the child would fail its authenticate call")
	}
	if !strings.Contains(err.Error(), authmethod.APIKeySecretName) {
		t.Fatalf("error %q must name the %s secret to create", err, authmethod.APIKeySecretName)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("error %q leaked a key value", err)
	}
}

func TestSpawnSpecRejectsUnknownMethod(t *testing.T) {
	if _, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: "personal"}, "", nil); err == nil {
		t.Fatal("BuildSpawnSpec accepted an unknown auth method")
	}
}
