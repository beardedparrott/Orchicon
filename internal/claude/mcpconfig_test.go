package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// runtimeSandboxDSNForTest keeps the assertion honest without importing the
// constant into the test body (so a change to it is a change in one place).
const runtimeSandboxDSNForTest = runtime.SandboxPostgresDSN

// The document shape claude accepts. Verified against the real CLI, which
// reported mcp_servers:[{"name":"orchicon","status":"connected"}] for exactly
// this JSON.
func TestMCPConfigJSONShape(t *testing.T) {
	cfg := MCPConfigJSON([]MCPServer{
		{Name: "orchicon", Command: "/usr/local/bin/orchicon", Args: []string{"mcp"},
			Env: map[string]string{"ORCHICON_MCP_TENANT_ID": "tnt_dev"}},
		{Name: "remote", URL: "https://example.com/mcp", Headers: map[string]string{"X-K": "v"}},
	})
	if cfg == "" {
		t.Fatal("MCPConfigJSON returned empty for two usable servers")
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("not valid JSON (%s): %v", cfg, err)
	}
	if len(doc.MCPServers) != 2 {
		t.Fatalf("got %d servers, want 2: %s", len(doc.MCPServers), cfg)
	}
	oc := doc.MCPServers["orchicon"]
	if oc["command"] != "/usr/local/bin/orchicon" {
		t.Errorf("command = %v, want the sidecar path", oc["command"])
	}
	if args, _ := oc["args"].([]any); len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", oc["args"])
	}
	if env, _ := oc["env"].(map[string]any); env["ORCHICON_MCP_TENANT_ID"] != "tnt_dev" {
		t.Errorf("env = %v, want the tenant id", oc["env"])
	}
	// A remote entry is typed; claude needs the discriminator.
	if doc.MCPServers["remote"]["type"] != "http" {
		t.Errorf("a url entry must be type=http, got %v", doc.MCPServers["remote"])
	}
}

// An entry with no command AND no url is not startable, and a server claude
// tries and fails to start is a startup it WAITS on — so it is dropped rather
// than emitted.
func TestMCPConfigJSONDropsUnusableEntries(t *testing.T) {
	if got := MCPConfigJSON([]MCPServer{{Name: "empty"}, {Command: "x"}}); got != "" {
		t.Fatalf("MCPConfigJSON = %q, want empty — neither entry is startable", got)
	}
	// And an all-empty set yields no flag at all, rather than an empty server map.
	if args := MCPArgs(nil); args != nil {
		t.Fatalf("MCPArgs(nil) = %v, want nil (no flag when there is nothing to register)", args)
	}
}

// The built-in entry: this binary's `mcp` subcommand, tenant-scoped.
func TestOrchiconMCPServer(t *testing.T) {
	s := OrchiconMCPServer("/usr/local/bin/orchicon", "tnt_x", map[string]string{
		"ORCHICON_POSTGRES_DSN": "postgres://sandbox",
		"":                      "dropped", // empty key
		"ALSO_EMPTY":            "",
	})
	if s.Name != "orchicon" {
		t.Errorf("name = %q, want orchicon (the prefix claude derives tool names from)", s.Name)
	}
	if len(s.Args) != 1 || s.Args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", s.Args)
	}
	if s.Env[MCPTenantEnv] != "tnt_x" {
		t.Errorf("tenant env = %q, want tnt_x", s.Env[MCPTenantEnv])
	}
	if s.Env["ORCHICON_POSTGRES_DSN"] != "postgres://sandbox" {
		t.Error("the transport's extra env was dropped")
	}
	if _, ok := s.Env[""]; ok {
		t.Error("an empty env key must not be emitted")
	}
	if _, ok := s.Env["ALSO_EMPTY"]; ok {
		t.Error("an empty env value must not be emitted")
	}
}

// THE ASK ARGV must carry the MCP config, or the session has no orchicon_* tools
// at all — the reported bug.
func TestAskArgvRegistersTheOrchiconMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	s := newAskSession(New(quietLogger()), "conv-mcp", filepath.Join(t.TempDir(), "ask"))
	s.tenantID = "tnt_argv"

	argv := s.argv()
	var cfg string
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			cfg = argv[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("the Ask argv has no --mcp-config — the session would have no orchicon_* tools")
	}
	if !strings.Contains(cfg, `"orchicon"`) {
		t.Errorf("mcp config does not register the orchicon server: %s", cfg)
	}
	if !strings.Contains(cfg, "tnt_argv") {
		t.Errorf("mcp config does not carry the tenant: %s", cfg)
	}
	// --strict-mcp-config would SUPPRESS the operator's own MCP servers. We must
	// never pass it: `claude mcp add` entries are meant to keep working.
	for _, a := range argv {
		if a == "--strict-mcp-config" {
			t.Fatal("--strict-mcp-config is set; that ignores every other MCP source, including the operator's own")
		}
	}
}

// THE MCP NAME MAPPING IS THE BOUNDARY. claude prefixes MCP tools
// (`mcp__orchicon__create_work_item`); askmode names them bare. Without the
// translation the mode gate is inert for exactly the tools it governs, and
// silently — no error, no refusal, just no enforcement.
func TestClaudeToolToPolicyNameMapsMCPTools(t *testing.T) {
	cases := map[string]string{
		"mcp__orchicon__create_work_item":   "create_work_item",
		"mcp__orchicon__schedule_work_item": "schedule_work_item",
		"mcp__orchicon__write":              "write",
		"mcp__other__something":             "something",
		// Non-MCP names are unchanged.
		"Write": "write",
		"Bash":  "bash",
	}
	for in, want := range cases {
		if got := claudeToolToPolicyName(in); got != want {
			t.Errorf("claudeToolToPolicyName(%q) = %q, want %q", in, got, want)
		}
	}
}

// END TO END through the gate, and the mapping above is what makes it possible.
//
// WHICH MODE REFUSES WHAT, because I got this backwards first and the test is
// what corrected me: the policy is per-mode, not one denylist.
//
//	brainstorm  denies theWorkTools  {write, edit, batch_write, bash}
//	            — it is WHERE YOU PLAN, so it MAY create and schedule work items
//	iteration   denies thePlanTools  {create_work_item, update_work_item,
//	            schedule_work_item, reorder_work_items, control_sequence, …}
//	            — it is the HANDS, so authoring and dispatch are the other modes' job
//
// So a planner tool is refused in ITERATION and allowed in brainstorm, and a
// work tool is the reverse. Asserting only one direction would pass against an
// implementation that hard-coded a single denylist.
func TestModeGateAppliesThePlannerDenyToMCPTools(t *testing.T) {
	dir := t.TempDir()
	iterPath := filepath.Join(dir, "iter.mode.json")
	brainPath := filepath.Join(dir, "brain.mode.json")
	if err := writeAskModeFile(iterPath, askmode.Iteration); err != nil {
		t.Fatalf("writeAskModeFile(iteration): %v", err)
	}
	if err := writeAskModeFile(brainPath, askmode.Brainstorm); err != nil {
		t.Fatalf("writeAskModeFile(brainstorm): %v", err)
	}

	// The planner deny, via an MCP-named tool. Without the name mapping this is
	// ALLOWED and the boundary is inert.
	denied, msg := askModeDenial(iterPath, "mcp__orchicon__create_work_item")
	if !denied {
		t.Fatal("iteration did NOT refuse mcp__orchicon__create_work_item — the MCP name mapping is not applied, so the planner deny is silently inert")
	}
	if !strings.Contains(msg, "REFUSED BY THE PLATFORM") {
		t.Errorf("refusal does not read as a platform refusal: %q", msg)
	}
	if !strings.Contains(msg, askmode.Brainstorm) {
		t.Errorf("the refusal does not direct the model to the mode that DOES plan: %q", msg)
	}

	// ...and brainstorm ALLOWS it, because planning is what brainstorm is for.
	if denied, m := askModeDenial(brainPath, "mcp__orchicon__create_work_item"); denied {
		t.Fatalf("brainstorm refused a planner tool it is supposed to have: %q", m)
	}
	// A read-only orchicon tool is refused by neither mode.
	if denied, _ := askModeDenial(brainPath, "mcp__orchicon__get_current_conversation"); denied {
		t.Error("a read-only orchicon tool was refused by the mode gate")
	}
	// And a WORK tool (a normal claude tool name) is refused by brainstorm, which
	// is the other direction of the same table.
	if denied, _ := askModeDenial(brainPath, "Write"); !denied {
		t.Error("brainstorm did not refuse Write")
	}
	if denied, _ := askModeDenial(iterPath, "Write"); denied {
		t.Error("iteration refused Write, which is what iteration does")
	}
}

// The Ask profile must not ask consent for the platform's OWN tools — the mode
// gate already governs them. A card in front of get_current_conversation would
// make the session unusable for the thing the operator asked for.
func TestAskProfileAllowsOrchiconMCPTools(t *testing.T) {
	h := HookInput{ToolName: "mcp__orchicon__get_current_conversation", ToolInput: map[string]any{}}
	if got := DecideToolForAsk(h, "/tmp/ask", ""); got.Decision() != DecisionAllow {
		t.Fatalf("an orchicon MCP tool = %s, want allow (the mode gate governs these, not a per-call card)", got.Decision())
	}
	// The operator's OWN MCP servers keep the ask-by-default treatment: they are
	// third-party tools, and consent is the point.
	other := HookInput{ToolName: "mcp__github__create_issue", ToolInput: map[string]any{}}
	if got := DecideToolForAsk(other, "/tmp/ask", ""); got.Decision() != DecisionAsk {
		t.Fatalf("a third-party MCP tool = %s, want ask", got.Decision())
	}
}

// isOrchiconMCPTool must not be fooled by a lookalike server name.
func TestIsOrchiconMCPTool(t *testing.T) {
	cases := map[string]bool{
		"mcp__orchicon__get_current_conversation": true,
		"mcp__orchicon__":                         true,
		"mcp__orchiconx__t":                       false, // prefix, not the server
		"mcp__github__orchicon__t":                false,
		"orchicon_get_current_conversation":       false, // unprefixed is not claude's name
		"":                                        false,
	}
	for in, want := range cases {
		if got := isOrchiconMCPTool(in); got != want {
			t.Errorf("isOrchiconMCPTool(%q) = %v, want %v", in, got, want)
		}
	}
}

// The WORKER path's transport-dependent env, which is the part that differs from
// Ask. A container must be pointed at the IN-CONTAINER sandbox Postgres: the DSN
// it inherits from the plane names the HOST's database, which inside the
// container is nothing.
func TestWorkerMCPExtraEnv(t *testing.T) {
	container := workerMCPExtraEnv(true, "run-1")
	if container["ORCHICON_POSTGRES_DSN"] != runtimeSandboxDSNForTest {
		t.Errorf("container DSN = %q, want the sandbox DSN", container["ORCHICON_POSTGRES_DSN"])
	}
	if container[MCPWorkflowRunEnv] != "run-1" {
		t.Errorf("container workflow run = %q, want run-1 (the sidecar injects its run_context)", container[MCPWorkflowRunEnv])
	}

	host := workerMCPExtraEnv(false, "run-2")
	if _, ok := host["ORCHICON_POSTGRES_DSN"]; ok {
		t.Error("a HOST worker must NOT be given the sandbox DSN — it inherits the plane's own")
	}
	if host[MCPWorkflowRunEnv] != "run-2" {
		t.Errorf("host workflow run = %q, want run-2", host[MCPWorkflowRunEnv])
	}

	// No run id → no var, rather than an empty one.
	if _, ok := workerMCPExtraEnv(false, "")[MCPWorkflowRunEnv]; ok {
		t.Error("an empty workflow run id must not be emitted")
	}
}

// THE WORKER ARGV must carry the MCP config too — the operator asked for the
// orchicon surface on worker executions, not only on Ask sessions.
func TestWorkerArgvRegistersTheOrchiconMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	s := &session{b: New(quietLogger())}
	s.tenantID = "tnt_worker"

	var cfg string
	argv := s.argv()
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			cfg = argv[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("the worker argv has no --mcp-config — a worker would have no orchicon_* tools")
	}
	if !strings.Contains(cfg, `"orchicon"`) || !strings.Contains(cfg, "tnt_worker") {
		t.Errorf("worker mcp config missing the server or the tenant: %s", cfg)
	}
	// The worker must never be handed --strict-mcp-config either.
	for _, a := range argv {
		if a == "--strict-mcp-config" {
			t.Fatal("--strict-mcp-config would suppress every other MCP source")
		}
	}
}

// A name collision between the built-in and an operator-configured entry resolves
// to the OPERATOR'S — the rule opencode applies too. Getting this backwards would
// silently override an operator's own connection with ours.
func TestMCPConfigJSONOperatorEntryWinsANameCollision(t *testing.T) {
	cfg := MCPConfigJSON([]MCPServer{
		// built-in first, exactly as resolveMCPServers orders them
		{Name: "orchicon", Command: "/usr/local/bin/orchicon", Args: []string{"mcp"}},
		// the operator's own, same name
		{Name: "orchicon", Command: "/opt/theirs/orchicon", Args: []string{"serve"}},
	})
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(doc.MCPServers) != 1 {
		t.Fatalf("a collision must collapse to ONE entry, got %d: %s", len(doc.MCPServers), cfg)
	}
	if got := doc.MCPServers["orchicon"]["command"]; got != "/opt/theirs/orchicon" {
		t.Fatalf("collision resolved to %v, want the operator's entry — theirs must not be silently overridden", got)
	}
}

// The neutral-spec renderer, which is the ONLY adapter-specific part of the MCP
// path. Codex will need its own version of this function and nothing else.
func TestMCPServersFromSpecs(t *testing.T) {
	got := MCPServersFromSpecs([]mcpclient.ServerSpec{
		{ID: "stdio", Command: []string{"/bin/s", "a", "b"}, Env: map[string]string{"K": "V"}},
		{ID: "http", URL: "https://h/mcp", Headers: map[string]string{"A": "B"}},
		{ID: ""},      // no id → dropped
		{ID: "empty"}, // nothing to start → dropped
		{ID: "urlwins", URL: "https://u", Command: []string{"/bin/x"}}, // url wins
	})
	if len(got) != 3 {
		t.Fatalf("got %d servers, want 3 (unusable dropped): %+v", len(got), got)
	}
	if got[0].Command != "/bin/s" || len(got[0].Args) != 2 {
		t.Errorf("stdio rendered as %+v, want command + 2 args", got[0])
	}
	if got[1].URL != "https://h/mcp" || got[1].Headers["A"] != "B" {
		t.Errorf("http rendered as %+v", got[1])
	}
	if got[2].URL != "https://u" {
		t.Errorf("a spec with both url and command must render as http, got %+v", got[2])
	}
}
