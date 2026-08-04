package opencode

import (
	"encoding/json"
	"testing"
)

func TestBuildConfigContentInjectPermissionDeny(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		AgentName:   "orchicon-worker",
		AgentPrompt: "you are a worker",
		ModelRef:    "opencode/deepseek-v4-flash-free",
	})

	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}

	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission block in config content, got %#v", cfg["permission"])
	}

	ext, ok := perm["external_directory"].(map[string]any)
	if !ok {
		t.Fatalf("expected external_directory rule map, got %#v", perm["external_directory"])
	}
	// The catch-all deny must be present and the scratch carve-out must be
	// the EXACT precise pattern — a sloppy "/tmp/orchicon*" (no "/**")
	// would also match the supervisor socket, guard shims, and the
	// opencode-data dirs that hold seeded model auth.json copies.
	if got, ok := ext["*"].(string); !ok || got != "deny" {
		t.Errorf("external_directory[\"*\"] = %#v, want %q", ext["*"], "deny")
	}
	if got, ok := ext[ScratchDir+"/**"].(string); !ok || got != "allow" {
		t.Errorf("external_directory[%q] = %#v, want %q", ScratchDir+"/**", ext[ScratchDir+"/**"], "allow")
	}
	for _, unsafe := range []string{"/tmp/orchicon*", "/tmp/orchicon", "/tmp/**", "/tmp", "/tmp/opencode-data-*"} {
		if _, ok := ext[unsafe]; ok {
			t.Errorf("external_directory must NOT contain a sloppy pattern %q (would widen the carve-out): got %#v", unsafe, ext[unsafe])
		}
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
