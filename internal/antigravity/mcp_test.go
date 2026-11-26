package antigravity

// mcp_test.go — AC4: the resolved owner-scoped MCP servers (with secrets EXPANDED) travel as ACP `mcpServers`
// on `session/new`, and NOTHING is written under a Google/agent config directory.
//
// THE SECOND HALF IS THE POINT. claude takes its servers as an argv JSON document; ACP takes them on the wire.
// Both are fine. What is NOT fine — and what this test exists to prevent — is a bridge that "helps" by writing
// `~/.gemini/config/mcp_config.json` or a `.agents/mcp_config.json` in the worktree: that would put an
// operator-scoped credential into a file on disk, where a later run (or a screenshot of the workspace) can read
// it, and it is the exact class internal/antigravity/notokens_test.go exists to keep this package out of.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// fakeScopeResolver returns a fixed resolution and records the scope ref it was asked for.
type fakeScopeResolver struct {
	res  mcpclient.Resolution
	err  error
	refs []mcpclient.ScopeRef
}

func (f *fakeScopeResolver) ResolveScope(ctx context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	f.refs = append(f.refs, ref)
	return f.res, f.err
}

// TestBridge_MCPServersRenderedOnSessionNewWithExpandedSecrets is AC4's wire half.
func TestBridge_MCPServersRenderedOnSessionNewWithExpandedSecrets(t *testing.T) {
	h := newHarness(t)

	// The operator's servers, exactly as the resolver would return them: one stdio server whose env carries a
	// ${SECRET} reference, and one remote server with an Authorization header reference.
	resolver := &fakeScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{
				Spec: mcpclient.ServerSpec{
					ID:      "github",
					Command: []string{"npx", "-y", "@modelcontextprotocol/server-github"},
					Env:     map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${GH_TOKEN}"},
				},
				From:   mcpclient.ScopeProject,
				FromID: "project:prj_1",
			},
			{
				Spec: mcpclient.ServerSpec{
					ID:      "internal",
					URL:     "https://mcp.internal.example/sse",
					Headers: map[string]string{"Authorization": "Bearer ${API_TOKEN}"},
				},
				From:   mcpclient.ScopeWorker,
				FromID: "worker:wrk_1@3",
			},
		},
	}}
	h.b.SetScopeResolver(resolver)
	// The SECRET RESOLVER expands the references. It is the same closure the server wires for claude.
	h.b.SetMCPSecretResolver(func(_ context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
		expand := func(m map[string]string) map[string]string {
			out := map[string]string{}
			for k, v := range m {
				switch v {
				case "${GH_TOKEN}":
					out[k] = "ghp_expanded_secret"
				case "Bearer ${API_TOKEN}":
					out[k] = "Bearer expanded_api_token"
				default:
					out[k] = v
				}
			}
			return out
		}
		return expand(env), expand(headers), nil
	})

	// A TEMP HOME, so "nothing was written under ~/.gemini" is a real assertion rather than a hope about this
	// machine.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))
	t.Setenv("USERPROFILE", tmpHome)

	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	// THE SCOPE IS THE WORKER SCOPE, the same ref claude builds.
	if len(resolver.refs) != 1 {
		t.Fatalf("ResolveScope was called %d times, want 1", len(resolver.refs))
	}
	ref := resolver.refs[0]
	if ref.Kind != mcpclient.ScopeWorker {
		t.Errorf("resolved scope kind = %v, want ScopeWorker", ref.Kind)
	}
	if ref.ProjectID != "prj_1" || ref.WorkerID != "wrk_1" {
		t.Errorf("scope ref = {ProjectID:%q WorkerID:%q}, want {prj_1 wrk_1}", ref.ProjectID, ref.WorkerID)
	}

	params := h.proc().sessionNewParams()
	if len(params) == 0 {
		t.Fatal("session/new carried no params")
	}
	var wire struct {
		MCPServers []struct {
			Name    string          `json:"name"`
			Command string          `json:"command"`
			Args    []string        `json:"args"`
			Env     json.RawMessage `json:"env"`
			URL     string          `json:"url"`
			Type    string          `json:"type"`
			Headers json.RawMessage `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(params[0], &wire); err != nil {
		t.Fatalf("decode session/new params: %v (%s)", err, params[0])
	}
	if len(wire.MCPServers) == 0 {
		t.Fatal("session/new carried no mcpServers — the worker would have no MCP tools at all")
	}
	// THE PLATFORM'S OWN SERVER IS FIRST: a worker must always be able to call `orchicon_*`.
	first := wire.MCPServers[0]
	if first.Name != "orchicon" {
		t.Errorf("mcpServers[0].name = %q, want \"orchicon\" (the platform's own sidecar is unconditional)",
			first.Name)
	}
	if !strings.HasSuffix(first.Command, "orchicon") && first.Command == "" {
		t.Errorf("mcpServers[0].command = %q, want the orchicon binary's `mcp` sidecar", first.Command)
	}
	var orchEnv []map[string]string
	if len(first.Env) > 0 {
		if err := json.Unmarshal(first.Env, &orchEnv); err != nil {
			t.Errorf("mcpServers[0].env is not a {name,value} list: %v (%s)", err, first.Env)
		}
	}
	var sawTenant bool
	for _, e := range orchEnv {
		if e["name"] == "ORCHICON_MCP_TENANT_ID" && e["value"] == "tnt_test" {
			sawTenant = true
		}
	}
	if !sawTenant {
		t.Errorf("the platform sidecar's env = %v, want ORCHICON_MCP_TENANT_ID=tnt_test — without it the "+
			"sidecar has no tenant scope and every orchicon_* call fails", orchEnv)
	}

	// THE RESOLVED SERVERS FOLLOW, WITH THEIR SECRETS EXPANDED.
	var github, internal map[string]any
	for _, s := range wire.MCPServers[1:] {
		switch s.Name {
		case "github":
			github = map[string]any{"command": s.Command, "args": s.Args, "env": s.Env}
		case "internal":
			internal = map[string]any{"url": s.URL, "type": s.Type, "headers": s.Headers}
		}
	}
	if github == nil {
		t.Fatalf("the resolved stdio server never reached session/new; servers: %+v", wire.MCPServers)
	}
	if github["command"] != "npx" {
		t.Errorf("github server command = %v, want npx", github["command"])
	}
	var githubEnv []map[string]string
	raw, _ := github["env"].(json.RawMessage)
	if err := json.Unmarshal(raw, &githubEnv); err != nil {
		t.Fatalf("github env is not a {name,value} list: %v (%s)", err, raw)
	}
	var token string
	for _, e := range githubEnv {
		if e["name"] == "GITHUB_PERSONAL_ACCESS_TOKEN" {
			token = e["value"]
		}
	}
	if token != "ghp_expanded_secret" {
		t.Errorf("github env token = %q, want the EXPANDED secret \"ghp_expanded_secret\" — a ${SECRET} "+
			"reference reaching agy unexpanded means the server cannot authenticate", token)
	}
	if strings.Contains(token, "${") {
		t.Errorf("an unexpanded ${...} reference reached the wire: %q", token)
	}

	if internal == nil {
		t.Fatalf("the resolved remote server never reached session/new; servers: %+v", wire.MCPServers)
	}
	if internal["type"] != "http" {
		t.Errorf("remote server type = %v, want \"http\"", internal["type"])
	}
	var headers []map[string]string
	hraw, _ := internal["headers"].(json.RawMessage)
	if err := json.Unmarshal(hraw, &headers); err != nil {
		t.Fatalf("remote headers are not a {name,value} list: %v (%s)", err, hraw)
	}
	var authz string
	for _, hv := range headers {
		if hv["name"] == "Authorization" {
			authz = hv["value"]
		}
	}
	if authz != "Bearer expanded_api_token" {
		t.Errorf("remote Authorization header = %q, want the expanded token", authz)
	}

	// AND NOTHING WAS WRITTEN TO A CONFIG DIRECTORY. This is the half of AC4 that is about what the bridge
	// must NOT do.
	assertNoConfigWrites(t, tmpHome, h.worktree)
}

// TestBridge_NoGoogleOrAgentConfigWrites is AC4's negative half, on its own: a full run against a temp HOME
// leaves that HOME and the worktree untouched.
func TestBridge_NoGoogleOrAgentConfigWrites(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))
	t.Setenv("USERPROFILE", tmpHome)

	h := newHarness(t)
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateMessage("wrote a file"),
		updateUsage(100, 1048576),
	}})
	h.startAndWait()

	assertNoConfigWrites(t, tmpHome, h.worktree)
}

// assertNoConfigWrites proves neither the temp HOME nor the worktree gained a config file. It walks rather than
// stat-ing a list of guesses, so a NEW directory this bridge invents is caught too.
func assertNoConfigWrites(t *testing.T, home, worktree string) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == home {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the temp HOME: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("the bridge wrote %d entr(ies) under the temp HOME (%s): %v — the MCP servers travel on the ACP "+
			"wire, and no Google/agent config file may be created on disk", len(found), home, found)
	}

	// The worktree is a WORKER's, so it may hold the run's files — but no `.agents` state, which is agy's own
	// customization directory and the thing the work item forbids writing.
	agents := filepath.Join(worktree, ".agents")
	if _, err := os.Stat(agents); err == nil {
		t.Errorf("%s exists — the bridge must never write agy's customization directory; the MCP servers go on "+
			"the wire as session/new's mcpServers", agents)
	}
	// And no `mcp_config.json` anywhere in the worktree.
	var mcpFiles []string
	_ = filepath.WalkDir(worktree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.Contains(filepath.Base(path), "mcp_config") {
			mcpFiles = append(mcpFiles, path)
		}
		return nil
	})
	if len(mcpFiles) > 0 {
		t.Errorf("the bridge wrote MCP config file(s) into the worktree: %v", mcpFiles)
	}
}

// TestBridge_MCPMissingServerFailsTheLaunch: a server selected but not configured is a LAUNCH failure that falls
// back to the built-in only, never a silent empty set. claude's renderMCP applies the same rule.
func TestBridge_MCPMissingServerFailsTheLaunch(t *testing.T) {
	h := newHarness(t)
	h.b.SetScopeResolver(&fakeScopeResolver{res: mcpclient.Resolution{
		Missing: []string{"server_that_does_not_exist"},
	}})
	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	// The execution still RUNS — claude degrades the same way — but the log carries the failure and the built-in
	// server is present, so the worker keeps its orchicon_* tools.
	params := h.proc().sessionNewParams()
	if len(params) == 0 {
		t.Fatal("session/new carried no params")
	}
	var wire struct {
		MCPServers []struct {
			Name string `json:"name"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(params[0], &wire); err != nil {
		t.Fatalf("decode session/new params: %v", err)
	}
	if len(wire.MCPServers) != 1 || wire.MCPServers[0].Name != "orchicon" {
		t.Errorf("mcpServers = %+v, want exactly the built-in orchicon server after a failed resolution",
			wire.MCPServers)
	}
}

// TestBridge_MCPSecretResolutionFailureKeepsTheBuiltin: a secret that cannot be expanded must not leave the
// worker with no MCP tools at all.
func TestBridge_MCPSecretResolutionFailureKeepsTheBuiltin(t *testing.T) {
	h := newHarness(t)
	h.b.SetScopeResolver(&fakeScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{{
			Spec: mcpclient.ServerSpec{
				ID:      "github",
				Command: []string{"npx", "server-github"},
				Env:     map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${MISSING_SECRET}"},
			},
		}},
	}})
	h.b.SetMCPSecretResolver(func(context.Context, string, map[string]string, map[string]string) (map[string]string, map[string]string, error) {
		return nil, nil, os.ErrNotExist
	})
	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	params := h.proc().sessionNewParams()
	var wire struct {
		MCPServers []struct {
			Name string `json:"name"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(params[0], &wire); err != nil {
		t.Fatalf("decode session/new params: %v", err)
	}
	if len(wire.MCPServers) != 1 || wire.MCPServers[0].Name != "orchicon" {
		t.Errorf("mcpServers = %+v, want the built-in only — an unexpandable secret must not cost the worker "+
			"its orchicon_* tools", wire.MCPServers)
	}
}
