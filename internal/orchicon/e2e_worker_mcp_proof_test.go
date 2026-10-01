package orchicon

// e2e_worker_mcp_proof_test.go — AC 1 (NATIVE worker leg) and AC 5.3 (the
// native negative proof).
//
// WHAT MAKES IT END-TO-END AND NOT A UNIT TEST. Every input is a REAL row read
// back by the REAL storage-backed resolver over a REAL MCP transport:
//
//   - a project-OWNED mcp_servers row + a version-OWNED inline spec, written to
//     Postgres and resolved by mcpsettings.NewResolver(pool) — the SAME resolver
//     internal/server/server.go wires into the bridge at :644;
//   - a REAL streamable-HTTP MCP server (the in-repo fixture's probe tool), so
//     discovery and the call cross a real JSON-RPC transport;
//   - the REAL NativeBridge.Start path (buildSession → mcpResolveAndStart →
//     the session loop), so the observation is what the PRODUCTION path produced
//     rather than what a hand-built registry would have produced.
//
// The model is SCRIPTED (no permitted model exists in this container — see the
// run's facts): the provider stands in for the model, and the record says so.
// Everything the provider is OFFERED and everything it RECEIVES is real.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// e2eProbeProvider is the scripted model for the native worker leg: round 1 asks
// for the project's probe tool, round 2 quotes the tool result it received. It
// RECORDS both the def names it was offered and the messages it was re-sent, so
// the assertions are over what the model actually saw.
type e2eProbeProvider struct {
	mu        sync.Mutex
	tool      string
	argsJSON  string
	offered   []string
	round2Msg []Message
	round     int
}

func (p *e2eProbeProvider) StreamTurn(_ context.Context, req TurnRequest) (TurnStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.round++
	if p.round == 1 {
		names := make([]string, 0, len(req.Tools))
		for _, d := range req.Tools {
			names = append(names, d.Name)
		}
		p.offered = names
		return newE2EStream([]Event{
			ToolCallStart{Index: 0, ToolCallID: "e2e-call-1", Name: p.tool},
			ToolCallDelta{Index: 0, ArgsJSONDelta: p.argsJSON},
			ToolCallEnd{Index: 0},
			Finish{StopReason: StopToolUse},
		}), nil
	}
	p.round2Msg = req.Messages
	out := ""
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				out = c.ToolResult.Content
			}
		}
	}
	return newE2EStream([]Event{
		TextDelta{Text: "the project's server said: " + out},
		// The worker loop's decision-signal gate requires a terminal marker; a
		// conforming worker model emits it, so the scripted model does too. The
		// probe output above is what the assertion reads.
		TextDelta{Text: "\n\nORCHICON WORKER SUMMARY: success — the project's MCP tool returned " + out},
		Finish{StopReason: StopStop},
	}), nil
}

func (p *e2eProbeProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *e2eProbeProvider) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true}
}

func (p *e2eProbeProvider) snapshot() (offered []string, round2 []Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.offered...), append([]Message(nil), p.round2Msg...)
}

// e2eStream is an in-memory TurnStream.
type e2eStream struct {
	events []Event
	i      int
}

func newE2EStream(evts []Event) *e2eStream { return &e2eStream{events: evts} }
func (s *e2eStream) Next(context.Context) (Event, bool, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil
}
func (s *e2eStream) Close() error { return nil }

// e2eToolResultFrom extracts the tool result string for `tool` from a re-sent
// history — the literal "what the model received" observation.
func e2eToolResultFrom(msgs []Message) string {
	out := ""
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				out = c.ToolResult.Content
			}
		}
	}
	return out
}

// e2eDiscardLogger keeps the bridge's per-session log out of the test output
// while still being a real logger (the provenance line is asserted via the
// captured buffer in a sibling leg, not here).
func e2eDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
}

// AC 1 (native): a REAL dispatched native execution uses the PROJECT'S MCP
// server — the tool is OFFERED to the model, the call SUCCEEDS, its output
// reaches the model's next round, the terminal result is a success, and the
// durable transcript carries the result.
//
// The server set spans BOTH project-owned rows (HTTP + stdio) so the criterion
// is observed on both transports, and the version's inline spec so the WORKER
// scope's union half is real too.
func TestE2ENativeWorkerUsesProjectServerAndSkill(t *testing.T) {
	pool := testfixtures.Pool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{
		StdioCommand: mcpclient.E2EStdioSpec("ignored").Command,
		InlineServer: true,
	})
	nonce := testfixtures.Nonce(t)

	prov := &e2eProbeProvider{
		tool:     mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool),
		argsJSON: `{"nonce":"` + nonce + `"}`,
	}
	b := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	}), seed.ProjectDir, e2eDiscardLogger())
	b.SetScopeResolver(mcpsettings.NewResolver(pool)) // production wiring (server.go:644)

	exec := db.ExecutionRow{
		ID: db.NewID(), TenantID: testfixtures.E2ETenant,
		ProjectID: seed.ProjectID, TaskID: seed.WorkItemID, WorkerID: seed.WorkerID,
	}
	mf := scheduler.ExecutionManifest{
		ExecutionID: exec.ID, ProjectID: seed.ProjectID, TaskID: seed.WorkItemID,
		WorkerID: seed.WorkerID, WorkerVersion: seed.WorkerVersion,
		Permissions: seed.VersionPermissions,
		ProjectDir:  seed.ProjectDir,
		SystemPrompt: "You are the E2E proof worker.", ModelRef: "orchicon/e2e-probe",
	}

	// The RESOLUTION is asserted on its own first — it is what a dispatched
	// worker actually got, independent of the session loop.
	res, err := b.ResolveExecutionMCP(context.Background(), exec, mf)
	if err != nil {
		t.Fatalf("ResolveExecutionMCP: %v", err)
	}
	resolvedIDs := e2eServerIDs(res)
	for _, want := range []string{seed.HTTPServerID, seed.StdioServerID, seed.InlineServerID} {
		if !e2eContains(resolvedIDs, want) {
			t.Fatalf("the execution did not resolve %q; got %v (provenance %s)",
				want, resolvedIDs, mcpclient.ProvenanceString(res.Servers))
		}
	}

	cb := &recordedCallback{}
	if err := b.Start(context.Background(), exec, mf, cb); err != nil {
		t.Fatalf("Start: %v", err)
	}

	offered, round2 := prov.snapshot()
	wantTool := mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool)
	if !e2eContains(offered, wantTool) {
		t.Fatalf("the model was NOT offered the project's tool %q; offered: %v", wantTool, offered)
	}
	// The stdio project-owned server's probe is offered too (same server, other
	// transport, so the child lifecycle is exercised end to end).
	if wantStdio := mcpclient.ToolName(seed.StdioServerID, mcpclient.E2EProbeTool); !e2eContains(offered, wantStdio) {
		t.Errorf("the model was not offered the project's STDIO tool %q; offered: %v", wantStdio, offered)
	}

	// AC 1: the call succeeded and its REAL result came back to the model.
	result := e2eToolResultFrom(round2)
	wantResult := mcpclient.E2EProbeResultPrefix + nonce
	if !strings.Contains(result, wantResult) {
		t.Fatalf("the tool result did not reach the model's next round: got %q, want it to carry %q", result, wantResult)
	}

	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want exactly one SUCCESS", results)
	}

	// The durable transcript carries the result: the third-party-readable record.
	tr := filepath.Join(seed.ProjectDir, ".orchicon", "sessions", exec.ID+".jsonl")
	data, rerr := os.ReadFile(tr)
	if rerr != nil {
		t.Fatalf("read the execution transcript %s: %v", tr, rerr)
	}
	if !strings.Contains(string(data), wantResult) {
		t.Errorf("the durable transcript does not carry the MCP result %q", wantResult)
	}

	sum := sha256.Sum256([]byte(mf.SystemPrompt))
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "native-worker", Adapter: "native", Surface: "worker-execution",
		Execution: exec.ID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Tool: wantTool, Args: `{"nonce":"` + nonce + `"}`, Result: result,
		Offered: offered, Model: "scripted", ModelAccess: "available",
		SkillPath: seed.SkillPath,
		PromptSHA: hex.EncodeToString(sum[:]),
		Note:      "real DB rows + real HTTP transport + real bridge Start; provider scripted (no permitted model in container)",
	})
}

// AC 5.3 (native negative): REMOVING the project's definition removes it from
// the NEXT session. A per-execution resolve that memoised its answer would keep
// offering the deleted server's tools; this is the pin against that.
func TestE2ENativeWorkerSecondExecutionHasNoDeletedServer(t *testing.T) {
	pool := testfixtures.Pool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	b := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return &e2eProbeProvider{}, nil
	}), seed.ProjectDir, e2eDiscardLogger())
	b.SetScopeResolver(mcpsettings.NewResolver(pool))

	exec := db.ExecutionRow{
		ID: db.NewID(), TenantID: testfixtures.E2ETenant,
		ProjectID: seed.ProjectID, WorkerID: seed.WorkerID,
	}
	mf := scheduler.ExecutionManifest{
		ExecutionID: exec.ID, ProjectID: seed.ProjectID, WorkerID: seed.WorkerID,
		WorkerVersion: seed.WorkerVersion, Permissions: seed.VersionPermissions,
		ProjectDir: seed.ProjectDir, SystemPrompt: "probe", ModelRef: "orchicon/e2e-probe",
	}

	// FIRST session: the server is present.
	before, err := b.ResolveExecutionMCP(context.Background(), exec, mf)
	if err != nil {
		t.Fatalf("first ResolveExecutionMCP: %v", err)
	}
	if !e2eContains(e2eServerIDs(before), seed.HTTPServerID) {
		t.Fatalf("the server was absent BEFORE deletion; the negative proof would be vacuous: %v", e2eServerIDs(before))
	}

	// DELETE the project's definition (the operator path: the row is gone).
	e2eDeleteServer(t, pool, seed.HTTPServerID)

	// NEXT session: it must be gone — no cache, no stale serve config.
	after, err := b.ResolveExecutionMCP(context.Background(), exec, mf)
	if err != nil {
		t.Fatalf("second ResolveExecutionMCP: %v", err)
	}
	if e2eContains(e2eServerIDs(after), seed.HTTPServerID) {
		t.Fatalf("the deleted server %q is STILL resolved for the next session: %v",
			seed.HTTPServerID, e2eServerIDs(after))
	}
	if len(after.Servers) != 0 {
		t.Fatalf("the next session resolved %d server(s) after the definition was removed, want 0", len(after.Servers))
	}

	// And the NEXT real session offers NO mcp__ tool at all.
	prov := &e2eProbeProvider{}
	b2 := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	}), seed.ProjectDir, e2eDiscardLogger())
	b2.SetScopeResolver(mcpsettings.NewResolver(pool))
	exec2 := exec
	exec2.ID = db.NewID()
	mf2 := mf
	mf2.ExecutionID = exec2.ID
	cb := &recordedCallback{}
	if err := b2.Start(context.Background(), exec2, mf2, cb); err != nil {
		t.Fatalf("Start (second session): %v", err)
	}
	offered, _ := prov.snapshot()
	for _, n := range offered {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("the NEXT session was still offered %q after the definition was removed", n)
		}
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "native-worker-negative", Adapter: "native", Surface: "worker-execution",
		Execution: exec2.ID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Offered: offered, Model: "scripted", ModelAccess: "available",
		Note: "AC5: after deleting the project-owned row, the next session resolves 0 servers and offers no mcp__ tool",
	})
}

// e2eServerIDs lists a resolution's server ids.
func e2eServerIDs(res mcpclient.Resolution) []string {
	out := make([]string, 0, len(res.Servers))
	for _, s := range res.Servers {
		out = append(out, s.Spec.ID)
	}
	return out
}

// e2eDeleteServer removes one mcp_servers row in a tenant transaction.
func e2eDeleteServer(t *testing.T, pool *db.Pool, id string) {
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

// e2eContains is a small membership test.
func e2eContains(hay []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
