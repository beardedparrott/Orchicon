package runtime

import (
	"strings"
	"testing"
)

// TestServeEnvSummaryNeverRendersAValue is a CREDENTIAL-LEAK guard.
//
// The environment it summarises is the one the daemon hands a runtime serve, which
// carries provider credentials (OLLAMA_API_KEY and the rest of adapterCredentialEnv's
// output). The summary is logged at every serve start, permanently, so a value reaching
// it would put a live credential in the platform's logs.
func TestServeEnvSummaryNeverRendersAValue(t *testing.T) {
	env := []string{
		"HOME=/home/operator",
		"PATH=/usr/bin",
		"OLLAMA_API_KEY=sk-super-secret-ollama",
		"DEEPSEEK_API_KEY=sk-super-secret-deepseek",
		"OPENCODE_SERVER_PASSWORD=server-password-xyz",
		"OPENCODE_CONFIG_CONTENT={\"agents\":{\"a\":{\"system\":\"prompt\"}}}",
	}
	got := serveEnvSummary(env)
	for _, secret := range []string{"sk-super-secret-ollama", "sk-super-secret-deepseek", "server-password-xyz", "prompt"} {
		if strings.Contains(got, secret) {
			t.Errorf("serveEnvSummary leaked %q — a value must never be rendered: %s", secret, got)
		}
	}
	// The NAMES must all be there, or the diagnostic cannot answer "was this variable
	// set at all?", which is the question it exists for.
	for _, name := range []string{"HOME", "PATH", "OLLAMA_API_KEY", "DEEPSEEK_API_KEY", "OPENCODE_SERVER_PASSWORD", "OPENCODE_CONFIG_CONTENT"} {
		if !strings.Contains(got, name) {
			t.Errorf("serveEnvSummary dropped %q: %s", name, got)
		}
	}
}

// TestServeEnvSummaryDistinguishesConfigs pins the one value that IS represented — the
// serve config — and that it is represented in a form that distinguishes two different
// configs while reproducing neither.
func TestServeEnvSummaryDistinguishesConfigs(t *testing.T) {
	a := serveEnvSummary([]string{"OPENCODE_CONFIG_CONTENT={\"a\":1}"})
	b := serveEnvSummary([]string{"OPENCODE_CONFIG_CONTENT={\"a\":2}"})
	if a == b {
		t.Errorf("two different configs produced identical summaries: %s", a)
	}
	if !strings.Contains(a, "sha:") || !strings.Contains(a, "len:") {
		t.Errorf("the config summary must carry a digest and a length: %s", a)
	}
	if strings.Contains(a, "{\"a\":1}") {
		t.Errorf("the config BODY must not be rendered: %s", a)
	}
}

// TestServeEnvSummaryIsDeterministic pins that the order is stable, so two runs with the
// same environment produce comparable lines rather than a reshuffled one.
func TestServeEnvSummaryIsDeterministic(t *testing.T) {
	env := []string{"B=2", "A=1", "C=3"}
	if serveEnvSummary(env) != serveEnvSummary(env) {
		t.Error("serveEnvSummary must be deterministic")
	}
	if got := serveEnvSummary([]string{"B=2", "A=1"}); got != serveEnvSummary([]string{"A=1", "B=2"}) {
		t.Errorf("order must not depend on input order: %s", got)
	}
}
