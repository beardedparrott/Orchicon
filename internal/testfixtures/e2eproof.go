package testfixtures

// e2eproof.go — shared DB seeding + the evidence sink for the end-to-end proof
// child ("a real worker per adapter and a live Ask turn use the project's server
// + skill file").
//
// WHY HERE. internal/testfixtures is already the cross-package test-support home
// (cmd/orchicon, internal/claude, internal/guard, internal/runtime, internal/tui
// all import it). Five proof packages need the SAME seed — one project, one work
// item, one worker version, one PROJECT-OWNED mcp_servers row, one skill file
// inside the project dir, one conversation — and five copies of that seed is
// exactly how the five legs start disagreeing about what "the project's server"
// means.
//
// WHAT MAKES IT EVIDENCE AND NOT A FIXTURE. Every leg appends one
// EvidenceRecord per observation to a JSONL file (ORCHICON_E2E_EVIDENCE, else
// <repo>/.gotmp/e2eproof/evidence.jsonl). Criterion 7 asks for "execution ids,
// what was called, what the model saw — real observations", so the artifact is
// the deliverable and the test output is not.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// E2ETenant is the tenant every DB-backed suite in this repo uses.
const E2ETenant = "tnt_dev"

// Pool opens the disposable test database and applies migrations + dev workers.
// It SKIPS (never fails) when ORCHICON_TEST_DSN is unset — the repo-wide
// convention for a DB-backed test.
func Pool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed end-to-end proof")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	// The tenant ROOT row MUST exist before any tenant-scoped write: mcp_servers
	// carries a `(tenant_id) REFERENCES tenants(id)` FK, so on a freshly-migrated
	// database (CI, or any database the sandbox daemon has not already booted
	// against) an UpsertMCPServer for E2ETenant fails with SQLSTATE 23503 unless
	// the tenant is seeded first. SeedDevWorkers does NOT create the tenant (it
	// writes tenant-scoped rows, which is exactly what would then violate the
	// FK), so the seed belongs here rather than relying on the daemon's boot path
	// having run — which is what made this harness pass locally and fail on CI.
	if err := db.SeedDevTenant(ctx, pool, E2ETenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool, E2ETenant); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

// Seed is every id + path a proof leg needs, all of them real rows in the DB.
type Seed struct {
	TenantID      string
	ProjectID     string
	ProjectDir    string
	WorkItemID    string
	WorkerID      string
	WorkerVersion int
	// VersionPermissions is the worker version's permissions jsonb as STORED:
	// what a dispatch carries on ExecutionManifest.Permissions, and the only
	// source of the version-owned inline servers/skills.
	VersionPermissions []byte
	ConversationID     string
	// HTTPServerID is the PROJECT-OWNED streamable-HTTP server's row id — also
	// the `mcp__<id>__` namespace prefix on the wire.
	HTTPServerID string
	// StdioServerID is the optional PROJECT-OWNED stdio server's row id (""
	// when no stdio spec was passed).
	StdioServerID string
	// InlineServerID is the optional VERSION-OWNED inline server's id ("").
	InlineServerID string
	// SkillPath / SkillBody are the project's skill FILE: a real file inside
	// ProjectDir, selected on the project row's skill_files.
	SkillPath string
	SkillBody string
	// ConvSkillPath / ConvSkillBody are the conversation's own skill file (""
	// unless SeedOpts.ConvSkill).
	ConvSkillPath string
	ConvSkillBody string
}

// SeedOpts tunes one seed.
type SeedOpts struct {
	// SkillName / SkillBody name the skill file written INSIDE the project dir
	// (default "e2e-skill.md" / a distinctive body).
	SkillName string
	SkillBody string
	// StdioCommand, when non-empty, registers a SECOND, project-owned stdio
	// server (the transport with child-lifecycle risk in production).
	StdioCommand []string
	// InlineServer enables a VERSION-OWNED inline HTTP server (so the WORKER
	// scope's union half is exercised against a real row + real JSON).
	InlineServer bool
	// ConvSkill writes a skill file owned by the CONVERSATION.
	ConvSkill bool
}

// SeedProjectWithServerAndSkill creates, in ONE tenant transaction: a project
// (with a real project_dir and a real skill FILE selected on it), a work item,
// a worker + published version, a PROJECT-OWNED mcp_servers row (HTTP, plus an
// optional stdio one), an optional VERSION-OWNED inline server, and a
// conversation in that project.
//
// This is the child-1 owner column, child-2 skill selection, child-6 Ask scope
// and child-3/4 dispatch scope, all in one place — which is what makes each leg
// end-to-end rather than a unit assertion over a hand-built struct.
func SeedProjectWithServerAndSkill(t *testing.T, pool *db.Pool, httpURL string, opts SeedOpts) Seed {
	t.Helper()
	ctx := context.Background()
	skillName := opts.SkillName
	if skillName == "" {
		skillName = "e2e-skill.md"
	}
	skillBody := opts.SkillBody
	if skillBody == "" {
		skillBody = "E2E_SKILL_BODY: always call the project's orchicon_e2e_probe tool when asked.\n"
	}
	projectDir := t.TempDir()
	skillPath := filepath.Join(projectDir, skillName)
	if err := os.WriteFile(skillPath, []byte(skillBody), 0o644); err != nil {
		t.Fatalf("write project skill file: %v", err)
	}

	ttx, err := pool.BeginTenantTx(ctx, E2ETenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()

	suffix := strings.ToLower(db.NewID())
	// The LAST 8 chars, not the first: a ULID leads with a 48-bit millisecond
	// timestamp, so two ids minted in the same millisecond share their first
	// chars (this collided on the unique (tenant, slug) index). The tail carries
	// the randomness.
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: E2ETenant, Name: "E2E Proof " + suffix,
		Slug: "e2e-proof-" + suffix, Status: "active",
		Goals: []byte("[]"), ProjectDir: projectDir,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Select the skill file on the PROJECT row (the SELECTABLE half of the
	// prompt — child 2's column).
	skillsJSON := []byte(`["` + skillPath + `"]`)
	if _, err := db.UpdateProject(ctx, ttx.Tx, E2ETenant, proj.ID, proj.Version, db.UpdateProjectFields{
		SkillFiles: &skillsJSON,
	}); err != nil {
		t.Fatalf("select project skill file: %v", err)
	}

	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: E2ETenant, ProjectID: proj.ID,
		Kind: "task", Title: "e2e proof work item", Status: "pending",
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}

	// The project-owned server: ONE row, owner column set, enabled.
	httpID := "e2ehttp" + suffix
	if _, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
		ID: httpID, TenantID: E2ETenant, Name: httpID, Transport: "streamable-http",
		URL: httpURL, Enabled: true, ProjectID: proj.ID,
		Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert project-owned http server: %v", err)
	}
	stdioID := ""
	if len(opts.StdioCommand) > 0 {
		stdioID = "e2estdio" + suffix
		if _, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
			ID: stdioID, TenantID: E2ETenant, Name: stdioID, Transport: "stdio",
			Command: opts.StdioCommand[0], Args: opts.StdioCommand[1:], Enabled: true, ProjectID: proj.ID,
			Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}"),
		}); err != nil {
			t.Fatalf("upsert project-owned stdio server: %v", err)
		}
	}

	// The worker + its published version. The version's permissions carry the
	// inline server (the WORKER scope's own half) and its skill file.
	workerID := "e2eworker" + suffix
	if _, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: workerID, TenantID: E2ETenant, Name: "E2E Worker " + suffix,
		Slug: "e2e-worker-" + suffix, Status: "published", CurrentVersion: 1,
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	inlineID := ""
	if opts.InlineServer {
		inlineID = "e2einline" + suffix
	}
	perms := versionPermissions(inlineID, httpURL, skillPath, skillBody)
	ver, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: E2ETenant, WorkerID: workerID, Version: 1,
		Status: "published", ModelRef: "orchicon/e2e-probe", Role: "E2E Proof Worker",
		Permissions: perms,
	})
	if err != nil {
		t.Fatalf("create worker version: %v", err)
	}

	conv, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: E2ETenant, ProjectID: proj.ID, Title: "e2e proof conversation",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	convSkillPath, convSkillBody := "", ""
	if opts.ConvSkill {
		convSkillBody = "E2E_CONV_SKILL_BODY: this conversation's own skill file.\n"
		convSkillPath = filepath.Join(projectDir, "e2e-conv-skill.md")
		if err := os.WriteFile(convSkillPath, []byte(convSkillBody), 0o644); err != nil {
			t.Fatalf("write conversation skill file: %v", err)
		}
		convSkills := []byte(`["` + convSkillPath + `"]`)
		if _, err := db.SetConversationSkillFiles(ctx, ttx.Tx, E2ETenant, conv.ID, convSkills); err != nil {
			t.Fatalf("select conversation skill file: %v", err)
		}
	}

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	return Seed{
		TenantID: E2ETenant, ProjectID: proj.ID, ProjectDir: projectDir,
		WorkItemID: item.ID, WorkerID: workerID, WorkerVersion: ver.Version,
		VersionPermissions: perms, ConversationID: conv.ID,
		HTTPServerID: httpID, StdioServerID: stdioID, InlineServerID: inlineID,
		SkillPath: skillPath, SkillBody: skillBody,
		ConvSkillPath: convSkillPath, ConvSkillBody: convSkillBody,
	}
}

// versionPermissions builds the worker version's permissions jsonb: the inline
// (version-owned) MCP server — the WORKER scope's own half — plus the version's
// own skill file entry.
func versionPermissions(inlineID, inlineURL, skillPath, skillBody string) []byte {
	servers := "[]"
	if inlineID != "" {
		servers = `[{"id":` + strconv.Quote(inlineID) + `,"type":"http","url":` + strconv.Quote(inlineURL) + `}]`
	}
	skills := "[]"
	if skillPath != "" {
		skills = `[{"path":` + strconv.Quote(skillPath) + `,"content":` + strconv.Quote(skillBody) + `}]`
	}
	return []byte(`{"mcp_servers":` + servers + `,"skill_files":` + skills + `}`)
}

// nonceSeq makes every Nonce unique within a process, so two legs (or two
// sub-tests) can never share one.
var nonceSeq struct {
	sync.Mutex
	n int64
}

// Nonce returns a per-call unique, human-readable nonce. A probe result that
// echoes it proves the answer was produced by THIS call, not memoised from an
// earlier one — which is what makes criterion 5 ("removing the definition
// removes it from the NEXT session") falsifiable rather than decorative.
func Nonce(t interface{ Name() string }) string {
	nonceSeq.Lock()
	nonceSeq.n++
	n := nonceSeq.n
	nonceSeq.Unlock()
	return fmt.Sprintf("e2e-%s-%d-%d", t.Name(), n, time.Now().UnixNano())
}

// EvidenceRecord is ONE observation. Every field is something a leg actually
// saw: an id from a real row, a tool name from a real offered list, a result
// from a real tool call, the system prompt the provider received.
type EvidenceRecord struct {
	Time      string   `json:"time"`
	Leg       string   `json:"leg"`
	Adapter   string   `json:"adapter"`
	Surface   string   `json:"surface"`
	Execution string   `json:"execution_id,omitempty"`
	Conversat string   `json:"conversation_id,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
	ServerID  string   `json:"server_id,omitempty"`
	Tool      string   `json:"tool,omitempty"`
	Args      string   `json:"args,omitempty"`
	Result    string   `json:"result,omitempty"`
	Offered   []string `json:"offered_tools,omitempty"`
	// Model / ModelAccess record the ONE thing this child cannot manufacture
	// (no permitted model exists in the runtime container): "scripted",
	// "free-ref" or "live-cli", and available|unavailable. A leg that skips for
	// model access is a recorded DISTINCTION, never a red.
	Model       string `json:"model,omitempty"`
	ModelAccess string `json:"model_access,omitempty"`
	// SkillPath + SkillInPrompt are the skill-file half of every leg.
	SkillPath     string `json:"skill_path,omitempty"`
	SkillInPrompt *bool  `json:"skill_in_prompt,omitempty"`
	PromptSHA     string `json:"system_prompt_sha256,omitempty"`
	Note          string `json:"note,omitempty"`
}

var evidenceMu sync.Mutex

// EvidencePath resolves the JSONL sink: ORCHICON_E2E_EVIDENCE when set, else
// <repo>/.gotmp/e2eproof/evidence.jsonl (the repo root is found by walking up
// from the working directory to go.mod, so the artifact always lands inside the
// project and never in the container's scratch tmpfs).
func EvidencePath() string {
	if p := strings.TrimSpace(os.Getenv("ORCHICON_E2E_EVIDENCE")); p != "" {
		return p
	}
	dir, err := os.Getwd()
	if err != nil {
		return filepath.Join(os.TempDir(), "orchicon-e2e-evidence.jsonl")
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".gotmp", "e2eproof", "evidence.jsonl")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(os.TempDir(), "orchicon-e2e-evidence.jsonl")
}

// WriteEvidence appends one observation as a JSON line. It never fails a test:
// the evidence sink is a record, and a record that could fail the thing it
// records would be worse than a missing line — so a write error is reported with
// t.Errorf (visible, non-fatal).
func WriteEvidence(t *testing.T, rec EvidenceRecord) {
	t.Helper()
	if rec.Time == "" {
		rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Errorf("marshal evidence record: %v", err)
		return
	}
	path := EvidencePath()
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Errorf("create evidence dir: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Errorf("open evidence file: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Errorf("append evidence line: %v", err)
	}
}

// BoolPtr is the *bool helper for EvidenceRecord.SkillInPrompt (proof packages
// outside this one build records).
func BoolPtr(b bool) *bool { return &b }
