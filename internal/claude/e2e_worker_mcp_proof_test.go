package claude

// e2e_worker_mcp_proof_test.go — AC 2 (the CLAUDE worker leg) and AC 5.1 (the
// claude negative proof, plus the claude Ask negative).
//
// WHAT IS REAL HERE. The claude adapter renders its MCP set into FIXED
// `--mcp-config` argv, so the observation is the argv the process is actually
// launched with: the assertion parses `--mcp-config` out of the REAL
// `session.argv()` / `askSession.argv()` — the same function the spawn path
// consumes — after the set was resolved from REAL mcp_servers rows by
// mcpsettings.NewResolver(pool), the SAME resolver internal/server/server.go
// wires at :756.
//
// A resolved-from-a-stub argv proves the renderer; a resolved-from-Postgres argv
// proves the adapter's whole path. This is the second.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// e2eParseMCPConfig returns the JSON string carried by `--mcp-config` in an argv.
func e2eParseMCPConfig(argv []string) string {
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// e2eArgvServerNames returns the server names registered in an argv's
// `--mcp-config`.
func e2eArgvServerNames(t *testing.T, argv []string) map[string]string {
	t.Helper()
	cfg := e2eParseMCPConfig(argv)
	if cfg == "" {
		t.Fatal("the argv carries no --mcp-config")
	}
	var parsed struct {
		MCPServers map[string]struct {
			URL     string `json:"url"`
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("the --mcp-config is not valid JSON: %v\n%s", err, cfg)
	}
	out := map[string]string{}
	for name, v := range parsed.MCPServers {
		if v.URL != "" {
			out[name] = v.URL
		} else {
			out[name] = v.Command
		}
	}
	return out
}

// AC 2 (worker): a REAL claude worker session's argv carries the PROJECT's MCP
// server — resolved from the DB row, not a stub — beside the built-in Orchicon
// sidecar, and the per-session log names its provenance.
func TestE2EClaudeWorkerArgvCarriesTheProjectServer(t *testing.T) {
	pool := testfixtures.Pool(t)
	builtinFor(t) // installs the fake CLI + a temp HOME (ClaudeBinaryPath resolution)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{InlineServer: true})

	b := New(quietLogger())
	b.SetScopeResolver(mcpsettings.NewResolver(pool)) // production wiring (server.go:756)

	logBuf := captureLog(t)

	s := &session{b: b, tenantID: testfixtures.E2ETenant}
	s.manifest = scheduler.ExecutionManifest{
		ProjectID: seed.ProjectID, WorkerID: seed.WorkerID,
		WorkerVersion: seed.WorkerVersion, Permissions: seed.VersionPermissions,
		ProjectDir: seed.ProjectDir,
	}
	argv := s.argv()

	names := e2eArgvServerNames(t, argv)
	if got, ok := names[seed.HTTPServerID]; !ok || got != url {
		t.Fatalf("the project's server %q is not in claude's real argv with url %q; got %v",
			seed.HTTPServerID, url, names)
	}
	if _, ok := names["orchicon"]; !ok {
		t.Errorf("the built-in Orchicon sidecar is missing from the argv: %v", names)
	}
	// The version's INLINE spec is the WORKER scope's own half.
	if seed.InlineServerID != "" {
		if _, ok := names[seed.InlineServerID]; !ok {
			t.Errorf("the version-owned inline server %q is missing from the argv: %v", seed.InlineServerID, names)
		}
	}
	// The provenance line names WHERE the definition came from.
	line := logBuf.String()
	if !strings.Contains(line, "claude: MCP servers registered") {
		t.Errorf("claude logged no MCP registration line: %s", line)
	}
	if !strings.Contains(line, "project:"+seed.ProjectID) {
		t.Errorf("the provenance line does not name the project owner: %s", line)
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "claude-worker-argv", Adapter: "claude", Surface: "worker-execution",
		ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Offered: e2eArgvNames(names), Result: "--mcp-config",
		Model: "scripted", ModelAccess: "available", SkillPath: seed.SkillPath,
		Note: "real DB rows -> real mcpsettings resolver -> real session.argv() --mcp-config names the project server with its url",
	})
}

// e2eArgvNames flattens the name map for evidence.
func e2eArgvNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AC 2 (claude, live half): the real CLI is spawned with that same argv and the
// model calls the project's tool.
//
// GATED on ORCHICON_TEST_LIVE_CLAUDE=1 AND a `claude` binary on PATH. When
// either is absent the leg records `model_access: unavailable` WITH the reason
// and skips — a recorded distinction, never a red: no permitted model exists in
// this container, and turning that into a failure would prove nothing about MCP.
func TestE2EClaudeWorkerLiveToolCall(t *testing.T) {
	if bin, err := exec.LookPath("claude"); err != nil || bin == "" {
		testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
			Leg: "claude-worker-live", Adapter: "claude", Surface: "worker-execution",
			Model: "live-cli", ModelAccess: "unavailable",
			Note: "no `claude` binary on PATH in this container; the offline argv leg proves the configuration half",
		})
		t.Skip("no claude binary on PATH; recording model_access=unavailable")
	}
	if strings.TrimSpace(os.Getenv("ORCHICON_TEST_LIVE_CLAUDE")) != "1" {
		testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
			Leg: "claude-worker-live", Adapter: "claude", Surface: "worker-execution",
			Model: "live-cli", ModelAccess: "unavailable",
			Note: "ORCHICON_TEST_LIVE_CLAUDE is not set; the live CLI leg is opt-in",
		})
		t.Skip("ORCHICON_TEST_LIVE_CLAUDE not set; recording model_access=unavailable")
	}

	pool := testfixtures.Pool(t)
	builtinFor(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})
	nonce := testfixtures.Nonce(t)

	b := New(quietLogger())
	b.SetScopeResolver(mcpsettings.NewResolver(pool))
	s := &session{b: b, tenantID: testfixtures.E2ETenant}
	s.manifest = scheduler.ExecutionManifest{
		ProjectID: seed.ProjectID, WorkerID: seed.WorkerID,
		WorkerVersion: seed.WorkerVersion, Permissions: seed.VersionPermissions,
		ProjectDir: seed.ProjectDir, ModelRef: "claude-sonnet-4-5",
	}
	argv := s.argv()
	wantTool := mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool)

	// The live spawn path itself is the production one (b.spawn -> newLocalProc).
	env, envCleanup := s.childEnv()
	defer envCleanup()
	proc, err := b.spawn(context.Background(), procSpec{
		Argv: argv, Cwd: seed.ProjectDir, Env: env,
	}, s.manifest)
	if err != nil {
		t.Fatalf("spawn the real claude CLI: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	prompt := "Call the tool " + wantTool + ` with the argument {"nonce":"` + nonce + `"} and report its output verbatim.`
	turn, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": prompt}},
		},
	})
	if err := proc.WriteTurn(append(turn, '\n')); err != nil {
		t.Fatalf("write the turn: %v", err)
	}

	called, output := e2eDriveProc(t, proc)
	if called != wantTool {
		t.Fatalf("the live claude model did not call %q; called %q (output: %s)", wantTool, called, output)
	}
	wantResult := mcpclient.E2EProbeResultPrefix + nonce
	if !strings.Contains(output, wantResult) {
		t.Fatalf("the live call's output did not carry %q: %s", wantResult, output)
	}
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "claude-worker-live", Adapter: "claude", Surface: "worker-execution",
		ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID, Tool: wantTool,
		Args: `{"nonce":"` + nonce + `"}`, Result: wantResult,
		Model: "claude-sonnet-4-5", ModelAccess: "available", SkillPath: seed.SkillPath,
		Note: "real claude CLI spawned with the real --mcp-config; the model called the project's tool and got its result",
	})
}

// AC 5.1 (claude negative): REMOVING the definition removes it from the NEXT
// session. claude's servers ride FIXED argv, so this is exactly the stale-config
// risk the criterion names.
func TestE2EClaudeAskDropsADeletedServerOnTheNextTurn(t *testing.T) {
	pool := testfixtures.Pool(t)
	builtinFor(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	b := New(quietLogger())
	b.SetScopeResolver(mcpsettings.NewResolver(pool))

	as := newAskSession(b, seed.ConversationID, filepath.Join(t.TempDir(), "ask"))
	as.tenantID = testfixtures.E2ETenant
	as.projectID = seed.ProjectID

	before := e2eArgvServerNames(t, as.argv())
	if _, ok := before[seed.HTTPServerID]; !ok {
		t.Fatalf("the server was absent BEFORE deletion (the negative proof would be vacuous): %v", before)
	}
	fingerprintBefore := as.mcpFingerprint

	// DELETE the project's definition.
	e2eDeleteServerRow(t, pool, seed.HTTPServerID)

	// A FRESH session (the next turn's spawn) must not carry it.
	as2 := newAskSession(b, seed.ConversationID, filepath.Join(t.TempDir(), "ask2"))
	as2.tenantID = testfixtures.E2ETenant
	as2.projectID = seed.ProjectID
	after := e2eArgvServerNames(t, as2.argv())
	if _, ok := after[seed.HTTPServerID]; ok {
		t.Fatalf("the deleted server %q is STILL in claude's argv for the next session: %v", seed.HTTPServerID, after)
	}
	if fingerprintBefore != "" && as2.mcpFingerprint == fingerprintBefore {
		t.Errorf("the MCP fingerprint did not change after deletion (%q) — the staleness check would not fire", fingerprintBefore)
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "claude-ask-negative", Adapter: "claude", Surface: "ask-conversation",
		Conversat: seed.ConversationID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Result: "argv after deletion omits the server", Model: "scripted", ModelAccess: "available",
		Note: "AC5.1: claude's fixed --mcp-config argv drops the deleted project server on the next session; the fingerprint changed too",
	})
}

// e2eDeleteServerRow removes one mcp_servers row in a tenant transaction.
func e2eDeleteServerRow(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, testfixtures.E2ETenant)
	if err != nil {
		t.Fatalf("begin tx to delete server: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	if err := db.DeleteMCPServer(ctx, ttx.Tx, testfixtures.E2ETenant, id); err != nil {
		t.Fatalf("delete mcp server %q: %v", id, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
}

// e2eDriveProc reads the CLI's stream-json lines until it sees a tool_use or the
// process goes quiet, returning the tool name it called and the accumulated
// output. It never hangs: the read is bounded by a deadline.
func e2eDriveProc(t *testing.T, proc ProcSession) (called string, output string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	var sb strings.Builder
	lines := proc.Lines()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return called, sb.String()
			}
			sb.Write(line)
			sb.WriteByte('\n')
			if n := e2eToolUseName(line); n != "" && called == "" {
				called = n
			}
			if strings.Contains(string(line), mcpclient.E2EProbeResultPrefix) || strings.Contains(string(line), `"result"`) {
				return called, sb.String()
			}
		case <-deadline:
			return called, sb.String()
		}
	}
}

// e2eToolUseName extracts the first tool_use name from one stream-json line.
func e2eToolUseName(line []byte) string {
	var ev struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return ""
	}
	for _, c := range ev.Message.Content {
		if c.Type == "tool_use" && c.Name != "" {
			return c.Name
		}
	}
	return ""
}
