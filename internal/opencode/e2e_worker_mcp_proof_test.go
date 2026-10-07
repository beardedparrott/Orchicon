package opencode

// e2e_worker_mcp_proof_test.go — AC 3 (the OPENCODE worker leg).
//
// THE GRANULARITY IS THE POINT. opencode's MCP set is baked into the SERVE's
// config ONCE (servehost.go serveConfig), because a shared serve is created per
// resolved SET — never per execution (child 4). So the proof has two halves, and
// criterion 3 allows the second:
//
//  1. OFFLINE (always runs): the project's definition, resolved from REAL
//     mcp_servers rows, reaches the config opencode actually boots from
//     (BuildConfigContent's RunMCP/RunSkills) and the pool keys serves by that
//     set — so a session whose project owns a server gets a serve built for it.
//  2. LIVE (gated): a real `opencode serve` + a real FREE model
//     (opencode/longcat-2.5-preview-free) makes the call. When opencode is absent
//     or the free model is unreachable, the leg RECORDS the actionable reason
//     ("unavailable", naming the server it could not exercise) and skips — it
//     never passes silently, which is what criterion 3 requires.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// e2eOpenCodeResolvedSet resolves the seed's WORKER scope with the REAL
// storage-backed resolver — the scope whose SKILL half carries the version's
// inline skill files. It is also the scope a dispatched worker resolves
// (ScopeWorker = project-owned ∪ the version's own definitions), so this is the
// set a real opencode session's serve is built from.
func e2eOpenCodeResolvedSet(t *testing.T, pool *db.Pool, seed testfixtures.Seed) mcpclient.Resolution {
	t.Helper()
	res, err := mcpsettings.NewResolver(pool).ResolveScope(
		tenantWithID(context.Background(), testfixtures.E2ETenant),
		mcpclient.ScopeRef{
			Kind:           mcpclient.ScopeWorker,
			ProjectID:      seed.ProjectID,
			WorkerID:       seed.WorkerID,
			Version:        seed.WorkerVersion,
			OwnPermissions: seed.VersionPermissions,
		},
	)
	if err != nil {
		t.Fatalf("ResolveScope(worker): %v", err)
	}
	return res
}

// AC 3 (offline half): the project's MCP server and its skill FILE reach the
// config opencode boots from. This proves the project's configuration reaches
// the artifact the serve is launched with — not a hand-built set.
func TestE2EOpenCodeResolvesProjectServerIntoItsServeConfig(t *testing.T) {
	pool := testfixtures.Pool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	res := e2eOpenCodeResolvedSet(t, pool, seed)
	if len(res.Servers) == 0 {
		t.Fatalf("the project's MCP definition did not resolve from the DB: %v", res)
	}
	if len(res.Skills) == 0 {
		t.Fatalf("the version's skill file did not resolve from the DB: %v", res)
	}

	content := BuildConfigContent(ConfigOptions{
		AgentName:         workerAgent,
		DefaultAgent:      workerAgent,
		OrchiconMCP:       true,
		PermissionProfile: ProfileWorker,
		// WorktreeDir is where an inline skill is MATERIALISED; the config then
		// emits that on-disk path as an `instructions` entry.
		WorktreeDir: seed.ProjectDir,
		RunMCP:      res.Servers,
		RunSkills:   res.Skills,
	})

	// The MCP half: the server id appears with its REAL url.
	var cfg map[string]any
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("the opencode config is not valid JSON: %v\n%s", err, content)
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	entry, ok := mcp[seed.HTTPServerID]
	if !ok {
		t.Fatalf("the project's server %q is not in opencode's boot config; servers: %v", seed.HTTPServerID, keysOf(mcp))
	}
	entryMap, _ := entry.(map[string]any)
	if got, _ := entryMap["url"].(string); got != url {
		t.Errorf("opencode's config names %q with url %q, want %q", seed.HTTPServerID, got, url)
	}

	// The SKILL half: the version's skill file is materialised under the worktree
	// and emitted as an `instructions` path, so the serve actually loads it.
	skillSlug := "e2e-skill"
	if !strings.Contains(content, skillSlug) {
		t.Errorf("the version's skill file is not in opencode's boot config as an instruction:\n%s", content)
	}
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "opencode-serve-config", Adapter: "opencode", Surface: "worker-execution",
		ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Offered: keysOf(mcp), Result: "BuildConfigContent(ConfigOptions{RunMCP, RunSkills}) carries the project server + the materialised skill instruction",
		Model: "n/a", ModelAccess: "available", SkillPath: seed.SkillPath,
		SkillInPrompt: testfixtures.BoolPtr(strings.Contains(content, skillSlug)),
		Note:          "real DB rows -> real mcpsettings resolver -> the config the opencode serve boots from",
	})
}

// AC 3 (offline half): the pool keys a serve by the resolved SET, so a session
// whose project owns a server runs on a serve built for it — and a DIFFERENT set
// gets a DIFFERENT serve, each with its own data dir. This is child 4's
// granularity, re-observed against a real DB-resolved set.
func TestE2EOpenCodeHostServePoolKeysByResolvedSet(t *testing.T) {
	pool := testfixtures.Pool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})
	res := e2eOpenCodeResolvedSet(t, pool, seed)
	if len(res.Servers) == 0 {
		t.Fatal("the project's set resolved empty; the pool-keying proof would be vacuous")
	}

	p := NewHostServePool(quietTestLogger(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()

	withSet, err := p.ServeFor(ctx, res)
	if err != nil {
		t.Fatalf("ServeFor(project set): %v", err)
	}
	empty, err := p.ServeFor(ctx, mcpclient.Resolution{})
	if err != nil {
		t.Fatalf("ServeFor(empty): %v", err)
	}
	if withSet == empty {
		t.Fatal("a project MCP set and the empty set share ONE serve — a project's servers would leak into an unconfigured session")
	}
	if p.Size() != 1 {
		t.Fatalf("pool size = %d, want 1 non-default serve for one non-empty set", p.Size())
	}
	// The pooled serve carries the project's set (its own data dir, distinct
	// from the default's).
	if withSet.DataDir() == empty.DataDir() {
		t.Errorf("the pooled serve and the default share a data dir %q", withSet.DataDir())
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "opencode-pool-keying", Adapter: "opencode", Surface: "worker-execution",
		ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Result: "distinct serve + data dir for the project set",
		Model:  "n/a", ModelAccess: "available",
		Note: "AC3 granularity: HostServePool.ServeFor keys one serve per resolved set; the project's set gets its own serve and data dir",
	})
}

// AC 3 (live half, or the ACTIONABLE failure criterion 3 permits): a REAL
// `opencode serve` boots the project's config and the free model calls the
// project's tool. Gated on ORCHICON_TEST_OPENCODE=1 + a binary on PATH.
func TestE2EOpenCodeLiveWorkerCallsTheProjectServer(t *testing.T) {
	bin, lerr := exec.LookPath("opencode")
	if lerr != nil || bin == "" {
		testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
			Leg: "opencode-worker-live", Adapter: "opencode", Surface: "worker-execution",
			Model: "opencode/longcat-2.5-preview-free", ModelAccess: "unavailable",
			Note: "no `opencode` binary on PATH in this container; the offline serve-config + pool-keying halves are the recorded evidence, and the live leg is the actionable failure criterion 3 permits",
		})
		t.Skip("no opencode binary on PATH; recording model_access=unavailable with the reason")
	}
	if os.Getenv("ORCHICON_TEST_OPENCODE") != "1" {
		testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
			Leg: "opencode-worker-live", Adapter: "opencode", Surface: "worker-execution",
			Model: "opencode/longcat-2.5-preview-free", ModelAccess: "unavailable",
			Note: "ORCHICON_TEST_OPENCODE is not set; the live serve leg is opt-in (it spawns opencode and makes one free-model call)",
		})
		t.Skip("ORCHICON_TEST_OPENCODE not set; recording model_access=unavailable")
	}

	pool := testfixtures.Pool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})
	res := e2eOpenCodeResolvedSet(t, pool, seed)
	nonce := testfixtures.Nonce(t)
	wantTool := mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool)

	hs := NewHostServe(quietTestLogger(), t.TempDir(), "")
	hs.SetMCPSet(res)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := hs.Start(ctx); err != nil {
		testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
			Leg: "opencode-worker-live", Adapter: "opencode", Surface: "worker-execution",
			ServerID: seed.HTTPServerID, Model: "opencode/longcat-2.5-preview-free", ModelAccess: "unavailable",
			Note: "a real `opencode serve` could not boot in this container: " + err.Error() + " — the actionable failure criterion 3 permits, naming the server it could not exercise",
		})
		t.Skipf("opencode serve could not start (%v); recording model_access=unavailable", err)
	}
	defer hs.Stop()

	// child 4's QA observation, re-taken against a DB-resolved set: a real serve
	// reports the distinctive server CONNECTED.
	client := hs.Client()
	if client == nil {
		t.Fatal("no host serve client")
	}

	callbacks := &e2eOpenCodeCallbacks{}
	execRow := db.ExecutionRow{
		ID: db.NewID(), TenantID: testfixtures.E2ETenant,
		ProjectID: seed.ProjectID, TaskID: seed.WorkItemID,
	}
	// opencode renders a namespaced MCP tool as `<server>_<tool>` (it strips
	// the native `mcp__`/`__` framing), so the model is told the name opencode
	// actually exposes. The assertion below normalizes both forms.
	manifest := scheduler.ExecutionManifest{
		Goal: "Call the tool " + e2eOpenCodeToolName(seed.HTTPServerID, mcpclient.E2EProbeTool) +
			` with {"nonce":"` + nonce + `"} and report its output verbatim. ` +
			"Finish your reply with the literal line: " + e2eDecisionMarker +
			" success — called the project's MCP tool and saw its result.",
		SystemPrompt: "You are a terse test bot. Use the tool you were given. " +
			"You MUST end your final message with the literal line " + e2eDecisionMarker +
			" success — <one line>. Do not omit it.",
		ModelRef:   "opencode/longcat-2.5-preview-free",
		ProjectDir: seed.ProjectDir,
	}
	a := New(quietTestLogger())
	r := &sessionRun{
		a: a, parentCtx: ctx, procCtx: ctx,
		execRow: execRow, manifest: manifest, callbacks: callbacks,
		client: client, modelRef: manifest.ModelRef, system: manifest.SystemPrompt,
		done: make(chan struct{}), stats: &execStreamState{},
	}
	if err := r.run(); err != nil {
		t.Fatalf("session run (live opencode): %v", err)
	}
	ok, output, errMsg, got := callbacks.result()
	if !got {
		t.Fatal("OnResult never fired")
	}
	if !e2eSameTool(callbacks.toolName(), wantTool) {
		t.Fatalf("the live opencode model did not call %q (called %q); ok=%v err=%s output=%s",
			wantTool, callbacks.toolName(), ok, errMsg, output)
	}
	wantResult := mcpclient.E2EProbeResultPrefix + nonce
	if !strings.Contains(output, wantResult) {
		t.Fatalf("the live opencode call's output did not carry %q: %s", wantResult, output)
	}
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "opencode-worker-live", Adapter: "opencode", Surface: "worker-execution",
		Execution: execRow.ID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Tool: wantTool, Args: `{"nonce":"` + nonce + `"}`, Result: wantResult,
		Model: "opencode/longcat-2.5-preview-free", ModelAccess: "available", SkillPath: seed.SkillPath,
		Note: "real `opencode serve` with the project's MCP set baked into its config + a real FREE model that called the project's tool",
	})
}

// --- small helpers -----------------------------------------------------------

// e2eDecisionMarker is the worker completion contract's terminal line. The LIVE
// model only emits it when it is told to (in production it rides the composite
// worker prompt), so the gate instructs the model and the run does not stall on
// `missing_decision_signal`.
const e2eDecisionMarker = "ORCHICON WORKER SUMMARY:"

// e2eOpenCodeToolName renders the tool name opencode exposes for a namespaced
// MCP tool: it strips the native `mcp__<server>__<tool>` framing down to
// `<server>_<tool>`, so the model must be asked for THAT name.
func e2eOpenCodeToolName(server, tool string) string {
	return server + "_" + tool
}

// e2eSameTool reports whether two tool names denote the same MCP tool, ignoring
// the framing opencode strips (`mcp__` prefix, `__`/`_` separators). This keeps
// the assertion tied to the OBSERVATION — that the project's distinctive probe
// was called — rather than to one adapter's spelling.
func e2eSameTool(got, want string) bool {
	return e2eNormTool(got) == e2eNormTool(strings.TrimPrefix(want, "mcp"))
}

// e2eNormTool lowercases and drops every non-alphanumeric byte.
func e2eNormTool(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tenantWithID stamps the tenant on a context (the resolvers read it there).
func tenantWithID(ctx context.Context, tenantID string) context.Context {
	return tenant.WithID(ctx, tenantID)
}

// keysOf lists a JSON object's keys (evidence-friendly).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// quietTestLogger silences a serve's own logging inside the test output.
func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// DataDir exposes the serve's data dir for the pool-keying assertion.
func (h *HostServe) DataDir() string { return h.dataDir }

// e2eOpenCodeCallbacks records what the live opencode session called, so the
// live leg asserts on the OBSERVATION (a tool call the model actually made)
// rather than on the session's terminal text alone.
type e2eOpenCodeCallbacks struct {
	liveCallbacks
	mu   sync.Mutex
	tool string
}

func (c *e2eOpenCodeCallbacks) OnToolCall(_ context.Context, _ string, toolName string, _, _ []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tool == "" {
		c.tool = toolName
	}
}

func (c *e2eOpenCodeCallbacks) toolName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tool
}
