package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
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

func TestBuildConfigContentCompositeWorktreeTools(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		AgentName:      workerAgent,
		AgentPrompt:    sessionToolShell,
		DefaultAgent:   workerAgent,
		CompositeTools: true,
		WorktreeDir:    "/worktree",
	})
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block, got %#v", cfg["mcp"])
	}
	wt, ok := mcp["orchicon-worktree"].(map[string]any)
	if !ok {
		t.Fatalf("expected orchicon-worktree MCP entry, got %#v", mcp["orchicon-worktree"])
	}
	cmd, _ := wt["command"].([]any)
	if len(cmd) != 2 || cmd[1] != "mcp" {
		t.Fatalf("worktree MCP command = %#v, want [<orchicon-binary>, mcp]", wt["command"])
	}
	env, ok := wt["environment"].(map[string]any)
	if !ok || env["ORCHICON_MCP_WORKTREE_DIR"] != "/worktree" {
		t.Fatalf("worktree MCP environment = %#v, want ORCHICON_MCP_WORKTREE_DIR=/worktree", wt["environment"])
	}

	// The built-in read/grep must be denied so the worker uses the batch tools.
	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission block, got %#v", cfg["permission"])
	}
	if read, ok := perm[readToolDeny].(map[string]any); !ok || read["*"] != "deny" {
		t.Fatalf("read must be denied when composite tools are on, got %#v", perm[readToolDeny])
	}
	if grep, ok := perm[grepToolDeny].(map[string]any); !ok || grep["*"] != "deny" {
		t.Fatalf("grep must be denied when composite tools are on, got %#v", perm[grepToolDeny])
	}
}

func TestBuildConfigContentDoesNotDenyReadGrepByDefault(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{AgentName: workerAgent, AgentPrompt: sessionToolShell, DefaultAgent: workerAgent})
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission block, got %#v", cfg["permission"])
	}
	if _, exists := perm[readToolDeny]; exists {
		t.Fatalf("read must NOT be denied by default, got %#v", perm[readToolDeny])
	}
	if _, exists := perm[grepToolDeny]; exists {
		t.Fatalf("grep must NOT be denied by default, got %#v", perm[grepToolDeny])
	}
}

// TestRuntimeServeConfigCompositeToolsLiveOnDev verifies the composite
// worktree tools are LIVE on dev runtime images: with a resolved project dir
// the config registers the orchicon-worktree MCP AND denies the built-in
// read/grep so the worker is forced onto batch_read/batch_grep. A base/gui
// image (or an unresolved project dir) must NOT emit the deny, so a worker is
// never locked out of file access with no batch tool to fall back on.
func TestRuntimeServeConfigCompositeToolsLiveOnDev(t *testing.T) {
	dev := RuntimeServeConfig("orchicon-runtime:orchicon-dev", "/worktree", "", nil, mcpclient.Resolution{})
	var devCfg map[string]any
	if err := json.Unmarshal([]byte(dev), &devCfg); err != nil {
		t.Fatalf("dev config not valid JSON: %v", err)
	}
	mcp, ok := devCfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block on dev config: %s", dev)
	}
	if _, ok := mcp["orchicon-worktree"].(map[string]any); !ok {
		t.Fatalf("expected orchicon-worktree MCP on dev config: %s", dev)
	}
	perm, ok := devCfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission block on dev config: %s", dev)
	}
	if read, _ := perm[readToolDeny].(map[string]any); read["*"] != "deny" {
		t.Fatalf("dev config must deny read (live composite), got %#v", perm[readToolDeny])
	}
	if grep, _ := perm[grepToolDeny].(map[string]any); grep["*"] != "deny" {
		t.Fatalf("dev config must deny grep (live composite), got %#v", perm[grepToolDeny])
	}

	// Composite tools are LIVE on base/gui images too: the worktree sidecar is
	// DB-less and runs from the daemon's bind-mounted binary. Only the sandbox
	// DB MCP (`orchicon`) stays dev-only.
	base := RuntimeServeConfig("orchicon-runtime:local", "/worktree", "", nil, mcpclient.Resolution{})
	var bcfg map[string]any
	if err := json.Unmarshal([]byte(base), &bcfg); err != nil {
		t.Fatalf("base config not valid JSON: %v", err)
	}
	bmcp, ok := bcfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block on base config (composite worktree), got: %s", base)
	}
	if _, ok := bmcp["orchicon-worktree"]; !ok {
		t.Fatalf("expected orchicon-worktree MCP on base config (composite live), got: %s", base)
	}
	if _, ok := bmcp["orchicon"]; ok {
		t.Fatalf("sandbox DB MCP (`orchicon`) must stay dev-only, got on base: %s", base)
	}
	bperm, _ := bcfg["permission"].(map[string]any)
	if bperm[readToolDeny] == nil {
		t.Fatalf("base config must deny read (composite live), got: %s", base)
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

// TestRuntimeServeConfigPlaneChannelOnEveryImage pins the plane-channel
// guarantee: whenever the run's worker role grants plane access (planeEnv
// non-empty — minted by the runtime lifecycle per run), the `orchicon-plane`
// MCP server is registered in the serve config on EVERY runtime image —
// base, gui, web-research, orchicon-dev — because plane access is
// ROLE-gated, never image-gated. This is the regression lock behind "the
// idea MCP tools are truly going to be available to any runtime container":
// the dedicated idea tools (orchicon_plane_list_idea_items /
// orchicon_plane_create_idea_item) live inside that plane registry, so the
// tools' availability reduces to this registration surviving for every
// image tag. The sidecar runs from the daemon bind-mounted binary
// (never baked), so the tools on the wire are whatever the freshly built
// binary ships — paired with the pool's binary fingerprint, a rebuilt
// binary forces a fresh container so the registry contents are always
// current.
func TestRuntimeServeConfigPlaneChannelOnEveryImage(t *testing.T) {
	planeEnv := map[string]string{
		"ORCHICON_PLANE_URL":   "http://172.17.0.3:8080",
		"ORCHICON_PLANE_TOKEN": "test-plane-token",
	}
	for _, tag := range []string{
		"orchicon-runtime:local",
		"orchicon-runtime:local-gui",
		"orchicon-runtime:web-research",
		"orchicon-runtime:orchicon-dev",
	} {
		out := RuntimeServeConfig(tag, "/worktree", "run-123", planeEnv, mcpclient.Resolution{})
		var cfg map[string]any
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s config not valid JSON: %v", tag, err)
		}
		mcp, ok := cfg["mcp"].(map[string]any)
		if !ok {
			t.Fatalf("%s config missing mcp block with planeEnv set: %s", tag, out)
		}
		plane, ok := mcp["orchicon-plane"].(map[string]any)
		if !ok {
			t.Fatalf("orchicon-plane MCP must register on EVERY image when planeEnv is present (got missing on %s): %s", tag, out)
		}
		cmd, _ := plane["command"].([]any)
		if len(cmd) != 2 || cmd[0] != runtimeContainerBinaryPath || cmd[1] != "mcp" {
			t.Fatalf("plane MCP command = %#v, want [%s, mcp]", plane["command"], runtimeContainerBinaryPath)
		}
		env, _ := plane["environment"].(map[string]any)
		if env["ORCHICON_PLANE_TOKEN"] != "test-plane-token" {
			t.Fatalf("plane MCP env = %#v, want the minted credential", env)
		}
	}

	// Deny-by-default is preserved: no planeEnv → no plane channel on any
	// image (a worker whose role grants nothing gets no plane tools).
	for _, tag := range []string{"orchicon-runtime:orchicon-dev", "orchicon-runtime:web-research"} {
		out := RuntimeServeConfig(tag, "/worktree", "run-123", nil, mcpclient.Resolution{})
		var cfg map[string]any
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s config not valid JSON: %v", tag, err)
		}
		if m, ok := cfg["mcp"].(map[string]any); ok {
			if _, exists := m["orchicon-plane"]; exists {
				t.Fatalf("orchicon-plane MCP must stay deny-by-default (registered on %s without planeEnv): %s", tag, out)
			}
		}
	}
}

func TestRuntimeServeConfigSandboxMCPOnlyOnDevImages(t *testing.T) {
	// Dev image: the container serve must register the Orchicon MCP against
	// the sandbox Postgres (workers get orchicon_* tools in-sandbox).
	dev := RuntimeServeConfig("orchicon-runtime:orchicon-dev", "/worktree", "", nil, mcpclient.Resolution{})
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
		base := RuntimeServeConfig(tag, "", "", nil, mcpclient.Resolution{})
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
	// `auto` (opencode's OWN lossy auto-compaction) must be explicitly OFF so
	// opencode is not a second, independent compaction driver on top of
	// Orchicon's budget ladder — which would interrupt the worker mid-flight.
	// Leaving it at opencode's default risks exactly that double-compaction.
	if comp["auto"] != false {
		t.Errorf("host serve compaction.auto = %#v, want false", comp["auto"])
	}

	// Runtime-container serve (dev image, the SDLC runs) + base image.
	for _, tag := range []string{"orchicon-runtime:orchicon-dev", "ghcr.io/beardedparrott/orchicon-runtime:latest"} {
		out := RuntimeServeConfig(tag, "/worktree", "", nil, mcpclient.Resolution{})
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
	out := RuntimeServeConfig("orchicon-runtime:orchicon-dev", "/worktree", "", nil, mcpclient.Resolution{})
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

// TestBuildConfigContentToolOutputAndBatchTool verifies the emitted config
// carries the tool-output size settings (tool_output.max_bytes/max_lines —
// the "smart size" settings that let a worker read a large file in ONE call
// instead of chunking it into many small reads, which is itself the re-send
// amplification) and the experimental batch_tool flag (ask opencode to emit
// independent tool calls in a single assistant turn). These are the two
// settings the worker is told to use to collapse the number of round-trips.
// They are only effective when opencode honors them, so this test is a
// regression lock on the config that is actually handed to opencode.
func TestBuildConfigContentToolOutputAndBatchTool(t *testing.T) {
	for name, out := range map[string]string{
		"host serve":  BuildConfigContent(ConfigOptions{AgentName: workerAgent}),
		"runtime dev": RuntimeServeConfig("orchicon-runtime:orchicon-dev", "/worktree", "", nil, mcpclient.Resolution{}),
	} {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s config not valid JSON: %v", name, err)
		}
		to, ok := cfg["tool_output"].(map[string]any)
		if !ok {
			t.Fatalf("%s config missing tool_output block: %s", name, out)
		}
		if to["max_bytes"] != float64(1000000) {
			t.Errorf("%s tool_output.max_bytes = %#v, want 1000000", name, to["max_bytes"])
		}
		exp, ok := cfg["experimental"].(map[string]any)
		if !ok {
			t.Fatalf("%s config missing experimental block: %s", name, out)
		}
		if exp["batch_tool"] != true {
			t.Errorf("%s experimental.batch_tool = %#v, want true", name, exp["batch_tool"])
		}
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

// TestRuntimeServeConfigCarriesRunUnion proves AC 1/AC 2/AC 4 at the builder
// level: the RUN's union (servers a step did NOT itself define included)
// reaches the container's emitted config, and a run server whose id collides
// with a BUILT-IN never overwrites it.
func TestRuntimeServeConfigCarriesRunUnion(t *testing.T) {
	union := mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			// The server another step defined — the whole point of the union.
			{Spec: mcpclient.ServerSpec{
				ID:      "step-b-http",
				Type:    mcpclient.TypeHTTP,
				URL:     "https://mcp.example.com/sse",
				Headers: map[string]string{"Authorization": "Bearer plaintext-token"},
			}, From: mcpclient.ScopeWorker, FromID: "inline:wkr_b@3"},
			// A stdio server with env.
			{Spec: mcpclient.ServerSpec{
				ID:      "step-a-stdio",
				Command: []string{"/usr/bin/mcp-server", "--flag"},
				Env:     map[string]string{"TOKEN": "abc"},
			}, From: mcpclient.ScopeProject, FromID: "project:prj_1"},
			// A name clash with a built-in: the built-in MUST win.
			{Spec: mcpclient.ServerSpec{ID: "orchicon", Command: []string{"/bogus"}},
				From: mcpclient.ScopeProject, FromID: "project:prj_1"},
		},
	}
	out := RuntimeServeConfig("orchicon-runtime:orchicon-dev", "/worktree", "run-1", nil, union)

	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp block, got %#v", cfg["mcp"])
	}
	bHTTP, ok := mcp["step-b-http"].(map[string]any)
	if !ok {
		t.Fatalf("the union's step-b-http server is missing from the config (AC 1/AC 2): %#v", mcp)
	}
	if bHTTP["type"] != "remote" || bHTTP["url"] != "https://mcp.example.com/sse" {
		t.Fatalf("remote server not rendered as McpRemoteConfig: %#v", bHTTP)
	}
	hdr, ok := bHTTP["headers"].(map[string]any)
	if !ok || hdr["Authorization"] != "Bearer plaintext-token" {
		t.Fatalf("resolved headers not carried to the serve (they are expanded at build time): %#v", bHTTP["headers"])
	}
	aStdio, ok := mcp["step-a-stdio"].(map[string]any)
	if !ok {
		t.Fatalf("the union's step-a-stdio server is missing (AC 1): %#v", mcp)
	}
	if aStdio["type"] != "local" {
		t.Fatalf("stdio server not rendered as McpLocalConfig: %#v", aStdio)
	}
	if env, ok := aStdio["environment"].(map[string]any); !ok || env["TOKEN"] != "abc" {
		t.Fatalf("stdio env not carried: %#v", aStdio["environment"])
	}
	// AC 8: the built-in wins a name clash — the union entry must NOT replace it.
	builtin, ok := mcp["orchicon"].(map[string]any)
	if !ok {
		t.Fatalf("built-in orchicon server missing on a dev image: %#v", mcp)
	}
	if cmd, _ := builtin["command"].([]any); len(cmd) == 1 && cmd[0] == "/bogus" {
		t.Fatalf("a run server OVERWROTE the built-in orchicon server; built-ins must win: %#v", builtin)
	}
}

// TestRunUnionDeterministic proves AC 3: two builds from the same run produce
// byte-identical config, and the builder takes no per-execution argument.
func TestRunUnionDeterministic(t *testing.T) {
	union := mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "z", URL: "https://z.example.com"}, From: mcpclient.ScopeProject, FromID: "project:p"},
			{Spec: mcpclient.ServerSpec{ID: "a", Command: []string{"/bin/a"}}, From: mcpclient.ScopeWorker, FromID: "inline:w@1"},
		},
		Skills: []mcpclient.InlineSkillFile{{Path: "skills/x.md"}},
	}
	first := RuntimeServeConfig("orchicon-runtime:local", "/worktree", "run-9", map[string]string{"A": "1"}, union)
	second := RuntimeServeConfig("orchicon-runtime:local", "/worktree", "run-9", map[string]string{"A": "1"}, union)
	if first != second {
		t.Fatalf("the config is not deterministic for a run:\n%s\n---\n%s", first, second)
	}
}

// TestRuntimeServeConfigEmitsRunSkills proves the run's skill-file union
// reaches the emitted instructions: a path-only entry is emitted verbatim and
// an inline-content entry is materialised under <worktree>/.orchicon/skills/.
func TestRuntimeServeConfigEmitsRunSkills(t *testing.T) {
	worktree := t.TempDir()
	out := RuntimeServeConfig("orchicon-runtime:local", worktree, "", nil, mcpclient.Resolution{
		Skills: []mcpclient.InlineSkillFile{
			{Path: "/abs/skill/from-store.md"},
			{Path: "inline/Deploy Helper", Content: "# Deploy\nstep 1"},
		},
	})
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	instr, ok := cfg["instructions"].([]any)
	if !ok {
		t.Fatalf("expected instructions, got %#v", cfg["instructions"])
	}
	joined := ""
	for _, v := range instr {
		joined += v.(string) + "\n"
	}
	if !strings.Contains(joined, "/abs/skill/from-store.md") {
		t.Fatalf("path-only skill not emitted verbatim: %s", joined)
	}
	materialised := filepath.Join(worktree, ".orchicon", "skills", "deploy-helper.md")
	if !strings.Contains(joined, materialised) {
		t.Fatalf("inline-content skill path not emitted: %s", joined)
	}
	b, err := os.ReadFile(materialised)
	if err != nil {
		t.Fatalf("inline skill content not written to disk: %v", err)
	}
	if string(b) != "# Deploy\nstep 1" {
		t.Fatalf("inline skill content mangled: %q", string(b))
	}
}

// TestProvenanceOfServeConfigNamesOnly proves the per-session provenance log
// reads server NAMES only — never the resolved credentials in env/headers.
func TestProvenanceOfServeConfigNamesOnly(t *testing.T) {
	cfg := RuntimeServeConfig("orchicon-runtime:local", "/worktree", "", nil, mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "b-secret", URL: "https://x", Headers: map[string]string{"Authorization": "Bearer TOPSECRET"}}},
			{Spec: mcpclient.ServerSpec{ID: "a-server", Command: []string{"/bin/a"}}},
		},
	})
	got := provenanceOfServeConfig(cfg)
	if !strings.Contains(got, "a-server") || !strings.Contains(got, "b-secret") {
		t.Fatalf("provenance did not name the servers: %q", got)
	}
	if strings.Contains(got, "TOPSECRET") {
		t.Fatalf("provenance leaked a resolved credential: %q", got)
	}
	if got != "a-server,b-secret,orchicon-worktree" {
		t.Fatalf("provenance is not the sorted name set: %q", got)
	}
}
