package opencode

import (
	"encoding/json"
	"testing"
)

func TestBuildConfigContentInjectPermissionDeny(t *testing.T) {
	out := BuildConfigContent("orchicon-worker", "you are a worker", "opencode/deepseek-v4-flash-free")

	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}

	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission block in config content, got %#v", cfg["permission"])
	}

	if ext, ok := perm["external_directory"].(string); !ok || ext != "deny" {
		t.Errorf("external_directory = %#v, want %q", perm["external_directory"], "deny")
	}

	bash, ok := perm["bash"].(map[string]any)
	if !ok {
		t.Fatalf("expected bash rules in permission block, got %#v", perm["bash"])
	}

	for _, rule := range []string{"rm", "rm *", "rm -rf *", "rm -rf /", "rm -rf /*", "rm -fr /", "sudo", "sudo *", "(*rm*", "{*rm*", "mkfs*", "fdisk*", "shred *", "dd * of=/dev/*", "chmod -R 777 /*", "chown -R * /*", "curl * | sh"} {
		if got, ok := bash[rule].(string); !ok || got != "deny" {
			t.Errorf("bash rule %q = %#v, want %q", rule, bash[rule], "deny")
		}
	}
}
