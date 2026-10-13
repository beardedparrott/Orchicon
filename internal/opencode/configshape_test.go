package opencode

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestMain PINS THE CONFIG SHAPE to v1 for every test in this package that does
// not say otherwise.
//
// The shape is a property of the opencode BINARY a machine has installed, and
// this package's tests assert the injected config document. Without a pin, the
// suite asserts v1 on a 1.x developer machine and fails on a 2.x one — the same
// non-hermetic dependence the binary-resolver seam exists to avoid. The v2 tests
// below set the resolver to v2 explicitly (withShape), so both shapes are covered
// and neither depends on the host.
func TestMain(m *testing.M) {
	SetConfigShapeResolver(func(*slog.Logger) ConfigShape { return ConfigShapeV1 })
	os.Exit(m.Run())
}

// withShape pins the config-shape resolver for one test and restores the package
// default afterwards.
func withShape(t *testing.T, shape ConfigShape, fn func()) {
	t.Helper()
	SetConfigShapeResolver(func(*slog.Logger) ConfigShape { return shape })
	t.Cleanup(func() {
		SetConfigShapeResolver(func(*slog.Logger) ConfigShape { return ConfigShapeV1 })
	})
	fn()
}

// workerOptions is the worker serve's config input, the shape every real
// execution and Ask session uses.
func workerOptions() ConfigOptions {
	return ConfigOptions{
		AgentName:    workerAgent,
		AgentPrompt:  sessionToolShell,
		DefaultAgent: workerAgent,
	}
}

func decodeDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	return m
}

// TestConfigShapeV1KeepsTheV1Vocabulary is the "1.x MUST NOT CHANGE" guard. The
// v1 document must carry the v1 keys and NONE of the v2 spellings: a v2 key
// leaking into a v1 document is a config field opencode 1.x will ignore, which is
// how a working 1.x install quietly loses its sandbox.
func TestConfigShapeV1KeepsTheV1Vocabulary(t *testing.T) {
	doc := decodeDoc(t, BuildConfigContent(workerOptions()))

	for _, k := range []string{"agent", "default_agent", "permission", "compaction"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("v1 document is missing %q", k)
		}
	}
	for _, k := range []string{"agents", "permissions", "providers", "media", "snapshots"} {
		if _, ok := doc[k]; ok {
			t.Errorf("v1 document carries the V2-only key %q — opencode 1.x would ignore it", k)
		}
	}
	comp, _ := doc["compaction"].(map[string]any)
	if _, ok := comp["prune"]; !ok {
		t.Error("v1 compaction must keep `prune`")
	}
	agent, _ := doc["agent"].(map[string]any)
	entry, _ := agent[workerAgent].(map[string]any)
	if _, ok := entry["prompt"]; !ok {
		t.Error("v1 agent must keep the `prompt` field")
	}
}

// TestConfigShapeV2UsesTheV2Vocabulary is the other half: every field we emit
// must be spelled the way 2.x reads it. Each expectation is a documented rename —
// `agents`, `system`, `permissions`, `mcp.servers`, `disabled` — and the two
// dropped keys are on 2.x's accepted-but-IGNORED list, so emitting them would
// only produce a warning.
func TestConfigShapeV2UsesTheV2Vocabulary(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		doc := decodeDoc(t, BuildConfigContent(workerOptions()))

		// agents.<name>.system — this is what displaces opencode's own `build`
		// prompt. Without it, `default_agent` has nothing to bind to.
		agents, ok := doc["agents"].(map[string]any)
		if !ok {
			t.Fatalf("v2 document is missing the `agents` block: %v", doc)
		}
		entry, ok := agents[workerAgent].(map[string]any)
		if !ok {
			t.Fatalf("v2 agents block does not define %q: %v", workerAgent, agents)
		}
		if got, _ := entry["system"].(string); got != sessionToolShell {
			t.Errorf("agents.%s.system = %q, want the tool shell — a non-empty `system` is what REPLACES the provider base prompt", workerAgent, got)
		}
		if _, gone := entry["prompt"]; gone {
			t.Error("v2 agent entry still carries `prompt`; 2.x reads `system`")
		}
		if _, ok := entry["model"]; ok {
			t.Error("an EMPTY model must be omitted: the v2 schema validates the reference pattern and \"\" does not match it")
		}
		if got, _ := doc["default_agent"].(string); got != workerAgent {
			t.Errorf("default_agent = %q, want %q", got, workerAgent)
		}

		for _, k := range []string{"agent", "permission"} {
			if _, ok := doc[k]; ok {
				t.Errorf("v2 document still carries the V1-only key %q", k)
			}
		}
		if _, ok := doc["permissions"].([]any); !ok {
			t.Errorf("v2 permissions must be an ordered ARRAY, got %T", doc["permissions"])
		}
		comp, _ := doc["compaction"].(map[string]any)
		if _, ok := comp["prune"]; ok {
			t.Error("v2 compaction must not carry `prune` — it is on the accepted-but-ignored list")
		}
		if auto, ok := comp["auto"].(bool); !ok || auto {
			t.Error("v2 compaction must keep auto:false — that is what stops opencode compacting behind Orchicon")
		}
		if _, ok := doc["experimental"]; ok {
			t.Error("v2 must omit `experimental.batch_tool` — it is on the accepted-but-ignored list")
		}
	})
}

// TestConfigShapeV2MCPBlock pins the MCP rename: servers grouped under
// `mcp.servers`, and `enabled` replaced by its INVERSE `disabled`.
func TestConfigShapeV2MCPBlock(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		doc := decodeDoc(t, BuildConfigContent(ConfigOptions{
			AgentName:    workerAgent,
			AgentPrompt:  sessionToolShell,
			DefaultAgent: workerAgent,
			OrchiconMCP:  true,
			TenantID:     "tnt_test",
		}))
		mcp, ok := doc["mcp"].(map[string]any)
		if !ok {
			t.Fatalf("no mcp block: %v", doc)
		}
		servers, ok := mcp["servers"].(map[string]any)
		if !ok {
			t.Fatalf("v2 must group servers under mcp.servers, got %v", mcp)
		}
		if _, ok := servers["orchicon"]; !ok {
			t.Fatalf("the built-in orchicon MCP server is missing: %v", servers)
		}
		srv, _ := servers["orchicon"].(map[string]any)
		if _, ok := srv["enabled"]; ok {
			t.Error("v2 entry must not carry `enabled`")
		}
		if disabled, ok := srv["disabled"].(bool); !ok || disabled {
			t.Error("a registered server must be emitted as disabled:false (the inverse of the v1 `enabled: true`)")
		}
	})
}

// TestV2PermissionRulesRenameActions pins the action vocabulary, which is the
// security-relevant half: an unrecognised action is a rule that NEVER MATCHES, so
// a rename missed here silently widens the worker's access instead of failing.
func TestV2PermissionRulesRenameActions(t *testing.T) {
	rules := v2PermissionRules(ProfileWorker, true)
	seen := map[string]bool{}
	for _, r := range rules {
		m := r.(map[string]any)
		seen[m["action"].(string)] = true
	}
	for _, want := range []string{"shell", "subagent", "external_directory", "read", "grep"} {
		if !seen[want] {
			t.Errorf("worker rules are missing action %q (got %v)", want, seen)
		}
	}
	for _, banned := range []string{"bash", "task", "write"} {
		if seen[banned] {
			t.Errorf("action %q is the V1 spelling and does not exist in v2 — it would be a rule that never matches", banned)
		}
	}
}

// TestV2PermissionRulesOrderBroadRuleFirst pins ORDER, which is the semantics in
// v2 ("the last matching rule wins"). The worker's external_directory intent is
// "deny everything outside, EXCEPT scratch and the run-state dir" — so the `*`
// deny must come BEFORE the specific allows, or the exceptions deny themselves.
func TestV2PermissionRulesOrderBroadRuleFirst(t *testing.T) {
	rules := v2PermissionRules(ProfileWorker, true)
	var order []string
	for _, r := range rules {
		m := r.(map[string]any)
		if m["action"] == "external_directory" {
			order = append(order, m["resource"].(string))
		}
	}
	if len(order) < 2 {
		t.Fatalf("expected the external_directory rules, got %v", order)
	}
	if order[0] != "*" {
		t.Errorf("the broad rule must come FIRST for last-match-wins to allow the exceptions; got order %v", order)
	}
	if order[0] == "*" {
		for _, o := range order[1:] {
			if o == "*" {
				t.Errorf("a `*` rule must not appear after a specific one: %v", order)
			}
		}
	}
}

// TestV2PermissionRulesDedupeWriteAndEdit pins the non-injective rename: v1 has
// separate `edit` and `write` keys, v2 has ONE `edit` action, so the interactive
// profile's two rules must collapse to one rather than be emitted twice.
func TestV2PermissionRulesDedupeWriteAndEdit(t *testing.T) {
	rules := v2PermissionRules(ProfileInteractive, false)
	n := 0
	for _, r := range rules {
		m := r.(map[string]any)
		if m["action"] == "edit" && m["resource"] == "*" {
			n++
			if m["effect"] != "ask" {
				t.Errorf("interactive edit/* effect = %v, want ask", m["effect"])
			}
		}
	}
	if n != 1 {
		t.Errorf("edit/* appears %d times; v1's edit+write collapse into one v2 action", n)
	}
}

// TestV2PermissionActionsAreInTheDocumentedVocabulary is the DRIFT DETECTOR. The
// v2 action list is quoted from the permissions reference; if a future change
// adds a v1 permission key, the translation carries the name through unchanged
// (it does not invent), and this test is what says so.
func TestV2PermissionActionsAreInTheDocumentedVocabulary(t *testing.T) {
	// V2 built-in actions, per the permissions reference's Actions table.
	documented := map[string]bool{
		"read": true, "edit": true, "glob": true, "grep": true, "shell": true,
		"subagent": true, "skill": true, "question": true, "webfetch": true,
		"websearch": true, "external_directory": true, "execute": true,
	}
	for _, profile := range []struct {
		name string
		p    PermissionProfile
	}{{"worker", ProfileWorker}, {"interactive", ProfileInteractive}} {
		for _, composite := range []bool{false, true} {
			for _, r := range v2PermissionRules(profile.p, composite) {
				action := r.(map[string]any)["action"].(string)
				// An MCP tool action is `<server>_<tool>`; none are emitted today.
				if documented[action] {
					continue
				}
				if strings.Contains(action, "_") {
					continue // <server>_<tool>
				}
				t.Errorf("%s profile (composite=%v): action %q is not a documented v2 action — "+
					"v2 would ignore the rule, silently widening access", profile.name, composite, action)
			}
		}
	}
}

// TestAisdkPackageQualification pins the provider-package rename: "AI SDK
// packages receive the aisdk: prefix", so a scope is only added to a package
// that is actually an AI SDK one.
func TestAisdkPackageQualification(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"@ai-sdk/openai-compatible", "aisdk:@ai-sdk/openai-compatible"},
		{"aisdk:@ai-sdk/openai-compatible", "aisdk:@ai-sdk/openai-compatible"},
		{"@opencode/ai/providers/acme", "@opencode/ai/providers/acme"},
	} {
		if got := aisdkPackage(c.in); got != c.want {
			t.Errorf("aisdkPackage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSetConfigShapeResolverNilRestoresTheProbe pins the seam contract: a test
// that forgets to restore must not leave the process pointed at a stub.
func TestSetConfigShapeResolverNilRestoresTheProbe(t *testing.T) {
	SetConfigShapeResolver(func(*slog.Logger) ConfigShape { return ConfigShapeV2 })
	SetConfigShapeResolver(nil)
	t.Cleanup(func() {
		SetConfigShapeResolver(func(*slog.Logger) ConfigShape { return ConfigShapeV1 })
	})
	if configShapeResolver == nil {
		t.Fatal("SetConfigShapeResolver(nil) left the resolver nil — every config build would panic")
	}
}
