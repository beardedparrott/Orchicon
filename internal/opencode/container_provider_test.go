package opencode

// container_provider_test.go — the config a container's opencode serve BOOTS with
// must carry the tenant's providers at addresses the container can dial.
//
// This is the outermost consumer of the transposition: a provider block that is
// absent (the pre-fix state) leaves the container reading a mounted opencode.jsonc
// that no user is required to hand-edit, and one with a loopback base URL points the
// worker at its own container.
import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// The generated container serve config carries a provider block with the
// transposed base URL — the thing a worker's serve will actually read.
func TestTheContainerServeConfigCarriesTransposedProviders(t *testing.T) {
	cfg := RuntimeServeConfig("orchicon:base", "/proj", "run-1", nil, mcpclient.Resolution{}, []runtime.ProviderConfig{
		{ID: "local-gufo", NPM: "@ai-sdk/openai-compatible", BaseURL: "http://172.17.0.1:8741/v1"},
		{ID: "halogen", NPM: "@ai-sdk/openai-compatible", BaseURL: "http://172.17.0.1:8731/v1"},
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(cfg), &m); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	prov, ok := m["provider"].(map[string]any)
	if !ok {
		t.Fatalf("no provider block in the generated config:\n%s", cfg)
	}
	t.Logf("provider block: %v", prov)
	gufo := prov["local-gufo"].(map[string]any)
	if got := gufo["options"].(map[string]any)["baseURL"]; got != "http://172.17.0.1:8741/v1" {
		t.Fatalf("gufo baseURL = %v", got)
	}
	if gufo["npm"] != "@ai-sdk/openai-compatible" {
		t.Fatalf("gufo npm = %v", gufo["npm"])
	}
	// The rest of the config must survive — this is the SAME config the run needs.
	for _, k := range []string{"$schema", "agent", "permission", "compaction"} {
		if _, ok := m[k]; !ok {
			t.Errorf("generated config lost %q", k)
		}
	}
}
