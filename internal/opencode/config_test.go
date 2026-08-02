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
