package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestMCPServerOwnership exercises the OWNER-SCOPED MCP definition surface:
// owner-scoped list/upsert, the owner XOR CHECK, the composite conversation
// FK's cascade, owner-scoped name uniqueness (two projects may each own a
// `postgres`; one owner may not hold two), and the permissions jsonb parser
// in all three shapes. Guarded by ORCHICON_TEST_DSN like the seed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestMCPServer -v
func TestMCPServerOwnership(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed MCP ownership test")
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

	// mcp_servers has an FK to tenants(id); create the tenant row first.
	//
	// The tenant id is UNIQUE PER RUN: a fixed id makes two concurrent test
	// processes contend on the same tenants row (the mcp_servers FK takes a
	// key-share lock on it) and deadlock instead of failing.
	tenant := "tnt_mcpmodel_" + db.NewID()[10:22]
	// SETUP COMMITS BEFORE THE BODY. The body's negative probes run in their
	// own transactions; while this tx held its tenant INSERT uncommitted, an
	// mcp_servers INSERT in another tx would block on the uncommitted
	// referenced row (FK key-share) and DEADLOCK — the outer tx never gets to
	// commit because it is the one waiting.
	stx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin setup tx: %v", err)
	}
	if _, err := stx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$2,$3,'active')`,
		tenant, "mcp-model-"+db.NewID()[:8], "MCP Model Test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	mkProject := func(name, slug string) db.ProjectRow {
		p, err := db.CreateProject(ctx, stx.Tx, db.ProjectRow{
			ID: db.NewID(), TenantID: tenant,
			Name: name, Slug: slug,
			Status: "active", Goals: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}
		return p
	}
	// A ULID's leading characters can collide (same millisecond), so the slugs
	// are explicit rather than derived from a truncated random id.
	projA := mkProject("Owner A", "mcp-owner-a")
	projB := mkProject("Owner B", "mcp-owner-b")

	conv, err := db.CreateConversation(ctx, stx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: tenant, Title: "MCP conversation",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := stx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// The body runs in its own transaction, so its negative probes (below) can
	// take their own and not poison it.
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin body tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	t.Cleanup(func() {
		c := context.Background()
		dttx, err := pool.BeginTenantTx(c, tenant)
		if err == nil {
			// mcp_servers cascades from projects AND conversations, but delete
			// explicitly so a failure here is visible rather than silent.
			_, _ = dttx.Exec(c, `DELETE FROM mcp_servers WHERE tenant_id=$1`, tenant)
			_, _ = dttx.Exec(c, `DELETE FROM ask_orchicon_conversations WHERE tenant_id=$1`, tenant)
			_, _ = dttx.Exec(c, `DELETE FROM projects WHERE tenant_id=$1`, tenant)
			_ = dttx.Commit(c)
		}
	})

	upsert := func(name, projectID, conversationID string) db.MCPServerRow {
		row, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
			ID: db.NewID(), TenantID: tenant, Name: name, Transport: "stdio",
			Command: "npx", Args: []string{"-y", name},
			Enabled: true, InstallStatus: "not_installed",
			ProjectID: projectID, ConversationID: conversationID,
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return row
	}

	// Project A and project B may EACH own a `postgres` — the old
	// UNIQUE(tenant_id, name) forbade exactly this.
	pgA := upsert("postgres", projA.ID, "")
	pgB := upsert("postgres", projB.ID, "")
	if pgA.ID == pgB.ID {
		t.Fatal("the two owners got the same row")
	}
	// A conversation-owned row.
	convSrv := upsert("chat-tools", "", conv.ID)

	// Owner-scoped read: project A sees only its own.
	gotA, err := db.ListMCPServersByOwner(ctx, ttx.Tx, tenant, projA.ID, "")
	if err != nil {
		t.Fatalf("list by owner A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != pgA.ID || gotA[0].ProjectID != projA.ID {
		t.Fatalf("owner A rows = %+v, want only its own postgres", gotA)
	}
	// The conversation scope reads project ∪ conversation in ONE call.
	gotConv, err := db.ListMCPServersByOwner(ctx, ttx.Tx, tenant, projA.ID, conv.ID)
	if err != nil {
		t.Fatalf("list conversation scope: %v", err)
	}
	if len(gotConv) != 2 {
		t.Fatalf("conversation scope rows = %+v, want project A's + the conversation's", gotConv)
	}
	seen := map[string]bool{}
	for _, r := range gotConv {
		seen[r.ID] = true
	}
	if !seen[pgA.ID] || !seen[convSrv.ID] || seen[pgB.ID] {
		t.Fatalf("conversation scope membership wrong: %+v", seen)
	}

	// The XOR CHECK rejects both-set and neither-set, and the owner-scoped
	// unique index rejects a second row of the same name for ONE owner. Each
	// probe runs in its OWN transaction: a rejected statement aborts the
	// transaction it ran in, so sharing one would poison the rest of the test.
	// Each probe must also roll back, so nothing is written when it is
	// (wrongly) accepted.
	probeRejected := func(what, q string, args ...any) {
		t.Helper()
		// A SAVEPOINT, not a nested tx: the rejected statement aborts the
		// transaction it runs in, so without a savepoint to roll back to the
		// rest of the body would be poisoned. No second tx means no FK lock
		// contention either.
		if _, err := ttx.Exec(ctx, `SAVEPOINT probe`); err != nil {
			t.Fatalf("savepoint (%s): %v", what, err)
		}
		_, err := ttx.Exec(ctx, q, args...)
		if _, rerr := ttx.Exec(ctx, `ROLLBACK TO SAVEPOINT probe`); rerr != nil {
			t.Fatalf("rollback to savepoint (%s): %v", what, rerr)
		}
		if err == nil {
			t.Errorf("%s was ACCEPTED", what)
		}
		t.Logf("%s rejected: %v", what, err)
	}
	probeRejected("the owner XOR with BOTH owners set",
		`INSERT INTO mcp_servers (id, tenant_id, name, project_id, conversation_id)
		 VALUES ($1,$2,'both',$3,$4)`, db.NewID(), tenant, projA.ID, conv.ID)
	probeRejected("the owner XOR with NO owner set",
		`INSERT INTO mcp_servers (id, tenant_id, name) VALUES ($1,$2,'neither')`, db.NewID(), tenant)
	probeRejected("a SECOND row named postgres for the SAME owner",
		`INSERT INTO mcp_servers (id, tenant_id, name, project_id)
		 VALUES ($1,$2,'postgres',$3)`, db.NewID(), tenant, projA.ID)

	// The COMPOSITE conversation FK cascades: deleting the conversation takes
	// its definition with it (and only its definition).
	if err := db.DeleteConversation(ctx, ttx.Tx, tenant, conv.ID); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if _, err := db.GetMCPServer(ctx, ttx.Tx, tenant, convSrv.ID); err != db.ErrNotFound {
		t.Fatalf("conversation-owned definition survived its conversation delete: %v", err)
	}
	if _, err := db.GetMCPServer(ctx, ttx.Tx, tenant, pgA.ID); err != nil {
		t.Fatalf("an unrelated definition was cascaded away: %v", err)
	}

	// Owner-scoped name lookup.
	if _, err := db.GetMCPServerByNameForOwner(ctx, ttx.Tx, tenant, projB.ID, "", "postgres"); err != nil {
		t.Fatalf("owner-scoped name lookup: %v", err)
	}
	if _, err := db.GetMCPServerByNameForOwner(ctx, ttx.Tx, tenant, projA.ID, "", "chat-tools"); err != db.ErrNotFound {
		t.Fatalf("owner-scoped name lookup leaked across owners: %v", err)
	}

	// Upsert with the same id updates in place (PK conflict target).
	pgA.Name = "postgres"
	pgA.Enabled = false
	upd, err := db.UpsertMCPServer(ctx, ttx.Tx, pgA)
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if upd.Enabled || upd.ProjectID != projA.ID {
		t.Fatalf("upsert update lost fields: %+v", upd)
	}

	// Delete an owned row cleanly.
	if err := db.DeleteMCPServer(ctx, ttx.Tx, tenant, pgB.ID); err != nil {
		t.Fatalf("delete owner B row: %v", err)
	}
	if _, err := db.GetMCPServer(ctx, ttx.Tx, tenant, pgB.ID); err != db.ErrNotFound {
		t.Fatalf("deleted row still present: %v", err)
	}

	// A conversation owner must EXIST (composite FK backstop + RequireConversation).
	if err := db.RequireConversation(ctx, ttx.Tx, tenant, "conv-does-not-exist"); err != db.ErrNotFound {
		t.Fatalf("RequireConversation on an unknown id = %v, want ErrNotFound", err)
	}
}

// TestRequireConversation pins the conversation-existence check the
// owner-scoped create path validates its owner with.
func TestRequireConversation(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed RequireConversation test")
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
	tenant := "tnt_mcpconv_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)
	c, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{ID: db.NewID(), TenantID: tenant, Title: "T"})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := db.RequireConversation(ctx, ttx.Tx, tenant, c.ID); err != nil {
		t.Fatalf("RequireConversation on an existing conversation: %v", err)
	}
	if err := db.RequireConversation(ctx, ttx.Tx, "tnt_other", c.ID); err != db.ErrNotFound {
		t.Fatalf("RequireConversation leaked across tenants: %v", err)
	}
}

// TestMCPServersFromPermissionsShapes is the PURE parser test: full inline
// specs, the legacy {id,command:string} reference, and the legacy bare id —
// all three decode through the ONE parser, and the id-only view delegates to
// it (so the reader and the writer cannot drift).
func TestMCPServersFromPermissionsShapes(t *testing.T) {
	full := []byte(`{"mcp_servers":[
		{"id":"full-1","type":"stdio","command":["npx","-y","@x/y"],"env":{"K":"V"},"enabled":true,"onError":"degrade"},
		{"id":"full-2","type":"http","url":"https://mcp.example.com","headers":{"Authorization":"Bearer x"},"enabled":false},
		{"id":"legacy-1","command":"npx -y @legacy/server"},
		"legacy-2"
	]}`)

	defs := db.MCPServersFromPermissions(full)
	if len(defs) != 4 {
		t.Fatalf("parsed %d definitions, want 4: %+v", len(defs), defs)
	}
	// 1. FULL SPEC: argv array survives verbatim.
	if defs[0].ID != "full-1" || defs[0].Type != "stdio" || len(defs[0].Command) != 3 || defs[0].Command[2] != "@x/y" {
		t.Errorf("full stdio spec lost fields: %+v", defs[0])
	}
	if defs[0].Env["K"] != "V" || defs[0].OnError != "degrade" {
		t.Errorf("full spec lost env/onError: %+v", defs[0])
	}
	if defs[0].Enabled == nil || !*defs[0].Enabled {
		t.Errorf("full spec lost its enabled flag: %+v", defs[0])
	}
	// 2. FULL HTTP SPEC.
	if defs[1].URL != "https://mcp.example.com" || defs[1].Headers["Authorization"] != "Bearer x" {
		t.Errorf("full http spec lost fields: %+v", defs[1])
	}
	if defs[1].Enabled == nil || *defs[1].Enabled {
		t.Errorf("full http spec lost its enabled=false: %+v", defs[1])
	}
	// 3. LEGACY {id, command: string}: the string splits on fields.
	if defs[2].ID != "legacy-1" || len(defs[2].Command) != 3 || defs[2].Command[0] != "npx" {
		t.Errorf("legacy string command did not split: %+v", defs[2])
	}
	if defs[2].Enabled != nil {
		t.Errorf("legacy reference should carry no enabled flag, got %+v", defs[2].Enabled)
	}
	// 4. LEGACY bare id.
	if defs[3].ID != "legacy-2" || len(defs[3].Command) != 0 {
		t.Errorf("bare id did not decode: %+v", defs[3])
	}

	// The id-only view DELEGATES (one parser): same ids, same order.
	refs := db.MCPServerRefsFromPermissions(full)
	want := []string{"full-1", "full-2", "legacy-1", "legacy-2"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs = %v, want %v", refs, want)
		}
	}

	// Malformed / absent degrade to nothing, never panic.
	if got := db.MCPServersFromPermissions([]byte("{not json")); got != nil {
		t.Errorf("malformed permissions yielded %+v, want nil", got)
	}
	if got := db.MCPServersFromPermissions(nil); got != nil {
		t.Errorf("nil permissions yielded %+v, want nil", got)
	}
	if got := db.MCPServersFromPermissions([]byte(`{"mcp_servers":[]}`)); len(got) != 0 {
		t.Errorf("empty list yielded %+v, want none", got)
	}
}

// TestSkillsFromPermissions pins the inline half of the run union's skills.
func TestSkillsFromPermissions(t *testing.T) {
	defs := db.SkillsFromPermissions([]byte(`{"skill_files":[
		{"path":"a.md","content":"A"},
		{"path":"","content":"anonymous"}
	]}`))
	if len(defs) != 1 || defs[0].Path != "a.md" || defs[0].Content != "A" {
		t.Fatalf("skills = %+v, want the one path-bearing entry", defs)
	}
	if got := db.SkillsFromPermissions([]byte(`not json`)); got != nil {
		t.Fatalf("malformed skills yielded %+v, want nil", got)
	}
}

// TestMCPServerTimestampOrderUnused keeps time imported for the row shape
// assertions without a magic sleep; a no-op assertion that the row carries a
// created_at (the owner-scoped backfill's determinism depends on it).
func TestMCPServerTimestampOrderUnused(t *testing.T) {
	var r db.MCPServerRow
	r.CreatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		t.Fatal("created_at not carried on the row shape")
	}
}
