package opencode

import (
	"encoding/json"
	"testing"
)

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
