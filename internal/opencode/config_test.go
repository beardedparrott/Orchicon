package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildConfigContentRegistersOrchiconMCP(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		AgentName:   "orchicon-assistant",
		AgentPrompt: "you are orchicon",
		ModelRef:    "opencode/deepseek-v4-flash-free",
		TenantID:    "tnt_abc",
		OrchiconMCP: true,
	})

	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block, got %#v", cfg["mcp"])
	}
	oc, ok := mcp["orchicon"].(map[string]any)
	if !ok {
		t.Fatalf("expected built-in orchicon MCP entry, got %#v", mcp["orchicon"])
	}
	if oc["type"] != "local" {
		t.Errorf("orchicon MCP type = %#v, want %q", oc["type"], "local")
	}
	cmd, ok := oc["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[1] != "mcp" {
		t.Fatalf("orchicon MCP command = %#v, want [<orchicon-binary>, mcp]", oc["command"])
	}
	env, ok := oc["environment"].(map[string]any)
	if !ok || env["ORCHICON_MCP_TENANT_ID"] != "tnt_abc" {
		t.Errorf("orchicon MCP environment = %#v, want ORCHICON_MCP_TENANT_ID=tnt_abc", oc["environment"])
	}
}

func TestBuildConfigContentSkipsOrchiconMCP(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		AgentName:   "orchicon-worker",
		AgentPrompt: "you are a worker",
		ModelRef:    "opencode/deepseek-v4-flash-free",
		TenantID:    "tnt_abc",
		OrchiconMCP: false,
	})
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	if mcp, ok := cfg["mcp"].(map[string]any); ok {
		if _, exists := mcp["orchicon"]; exists {
			t.Fatalf("orchicon MCP registered despite OrchiconMCP=false: %s", out)
		}
	}
	if !strings.Contains(out, `"mcp"`) {
		// The user's own MCP servers may still be merged in; that's fine.
		// This just confirms the doc is well-formed.
	}
}

func TestBuildConfigContentMCPEnvAndBinaryPath(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		TenantID:      "tnt_dev",
		OrchiconMCP:   true,
		MCPEnv:        map[string]string{"ORCHICON_POSTGRES_DSN": "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"},
		MCPBinaryPath: "/usr/local/bin/orchicon",
	})
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	oc := cfg["mcp"].(map[string]any)["orchicon"].(map[string]any)
	cmd, _ := oc["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "/usr/local/bin/orchicon" || cmd[1] != "mcp" {
		t.Fatalf("orchicon MCP command = %#v, want [/usr/local/bin/orchicon, mcp]", oc["command"])
	}
	env, _ := oc["environment"].(map[string]any)
	if env["ORCHICON_MCP_TENANT_ID"] != "tnt_dev" {
		t.Errorf("tenant env = %#v, want tnt_dev", env["ORCHICON_MCP_TENANT_ID"])
	}
	if env["ORCHICON_POSTGRES_DSN"] == "" {
		t.Errorf("expected ORCHICON_POSTGRES_DSN in MCP environment, got %#v", env)
	}
}

func TestRuntimeServeConfigSandboxMCPOnlyOnDevImages(t *testing.T) {
	// Dev image: the container serve must register the Orchicon MCP against
	// the sandbox Postgres (workers get orchicon_* tools in-sandbox).
	dev := RuntimeServeConfig("orchicon-runtime:orchicon-dev")
	var devCfg map[string]any
	if err := json.Unmarshal([]byte(dev), &devCfg); err != nil {
		t.Fatalf("dev config not valid JSON: %v", err)
	}
	devMCP, ok := devCfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block on dev image config: %s", dev)
	}
	oc, ok := devMCP["orchicon"].(map[string]any)
	if !ok {
		t.Fatalf("expected orchicon MCP entry on dev image config: %s", dev)
	}
	cmd, _ := oc["command"].([]any)
	if len(cmd) != 2 || cmd[0] != runtimeContainerBinaryPath || cmd[1] != "mcp" {
		t.Fatalf("dev image MCP command = %#v, want [%s, mcp]", oc["command"], runtimeContainerBinaryPath)
	}
	env, _ := oc["environment"].(map[string]any)
	if env["ORCHICON_POSTGRES_DSN"] != "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable" {
		t.Errorf("sandbox DSN env = %#v", env["ORCHICON_POSTGRES_DSN"])
	}

	// Base/gui image: no sandbox plane, no MCP — behavior identical to today.
	for _, tag := range []string{"ghcr.io/beardedparrott/orchicon-runtime:latest", "orchicon-runtime:gui-latest"} {
		base := RuntimeServeConfig(tag)
		var baseCfg map[string]any
		if err := json.Unmarshal([]byte(base), &baseCfg); err != nil {
			t.Fatalf("base config not valid JSON: %v", err)
		}
		if m, ok := baseCfg["mcp"].(map[string]any); ok {
			if _, exists := m["orchicon"]; exists {
				t.Errorf("orchicon MCP registered on non-dev image %s: %s", tag, base)
			}
		}
	}
}

// TestBuildConfigContentCompactionPruneEnabled verifies every serve config —
// host serve AND runtime-container serve — enables opencode context
// compaction pruning, so a long step does not keep re-sending every past
// tool output (read/grep/bash results) on every turn. Prune is
// capability-safe: it removes stale tool RESULTS, not tool definitions,
// prompts, or decisions; it does not enable lossy auto-compaction.
func TestBuildConfigContentCompactionPruneEnabled(t *testing.T) {
	// Host serve path.
	host := BuildConfigContent(ConfigOptions{AgentName: workerAgent})
	var hostCfg map[string]any
	if err := json.Unmarshal([]byte(host), &hostCfg); err != nil {
		t.Fatalf("host config not valid JSON: %v", err)
	}
	comp, ok := hostCfg["compaction"].(map[string]any)
	if !ok {
		t.Fatalf("host serve config missing compaction block: %s", host)
	}
	if comp["prune"] != true {
		t.Errorf("host serve compaction.prune = %#v, want true", comp["prune"])
	}
	if comp["auto"] == true {
		t.Errorf("host serve compaction.auto should stay at opencode default (lossy); got true")
	}

	// Runtime-container serve (dev image, the SDLC runs) + base image.
	for _, tag := range []string{"orchicon-runtime:orchicon-dev", "ghcr.io/beardedparrott/orchicon-runtime:latest"} {
		out := RuntimeServeConfig(tag)
		var cfg map[string]any
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("runtime config not valid JSON: %v", err)
		}
		comp, ok := cfg["compaction"].(map[string]any)
		if !ok {
			t.Fatalf("runtime config %s missing compaction block: %s", tag, out)
		}
		if comp["prune"] != true {
			t.Errorf("runtime %s compaction.prune = %#v, want true", tag, comp["prune"])
		}
	}
}

// TestBuildConfigContentWorkerDefaultAgent verifies worker serves register a
// minimal `orchicon-worker` agent prompt AND set it as default_agent, so
// sessions do not run under opencode's large built-in `build` prompt. This
// is the per-turn token win: Orchicon's real system prompt still rides the
// per-message `system` field; the agent prompt is just a tool-guideline shell.
func TestBuildConfigContentWorkerDefaultAgent(t *testing.T) {
	out := RuntimeServeConfig("orchicon-runtime:orchicon-dev")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("runtime config not valid JSON: %v", err)
	}
	if cfg["default_agent"] != workerAgent {
		t.Fatalf("default_agent = %#v, want %q", cfg["default_agent"], workerAgent)
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatalf("expected agent block, got %#v", cfg["agent"])
	}
	wa, ok := agents[workerAgent].(map[string]any)
	if !ok {
		t.Fatalf("expected %q agent entry, got %#v", workerAgent, agents[workerAgent])
	}
	prompt, ok := wa["prompt"].(string)
	if !ok || prompt == "" {
		t.Fatalf("expected a non-empty %q prompt, got %#v", workerAgent, wa["prompt"])
	}
	// The prompt must be the minimal tool-guideline shell, not the full
	// Orchicon system prompt (which rides the per-message system field).
	if len(prompt) > 2000 {
		t.Fatalf("worker agent prompt should be a short shell, got %d chars", len(prompt))
	}
}

func TestStripJSONC(t *testing.T) {
	in := `{
  // line comment
  "mcp": {
    "server": { "command": "npx", /* inline */ "args": ["-y"] },
  },
  "provider": { "opencode": {} },  // trailing comment
}`
	got := string(stripJSONC([]byte(in)))
	// The stripped doc must parse as strict JSON.
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("stripped JSONC did not parse: %v\n%s", err, got)
	}
	mcp, ok := m["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp key, got %#v", m["mcp"])
	}
	if _, ok := mcp["server"]; !ok {
		t.Fatalf("expected mcp.server, got %#v", mcp)
	}
	// String contents must survive (URLs with // and /* in them).
	u := `{"url": "https://example.com/a/*/b"}`
	got = string(stripJSONC([]byte(u)))
	var mm map[string]string
	if err := json.Unmarshal([]byte(got), &mm); err != nil {
		t.Fatalf("url with slashes broke: %v", err)
	}
	if mm["url"] != "https://example.com/a/*/b" {
		t.Fatalf("url mangled: %q", mm["url"])
	}
}
