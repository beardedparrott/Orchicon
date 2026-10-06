package worker

// permissions_inline_test.go — the PLANE-SIDE gate on a worker version's INLINE MCP specs.
//
// Before it, `permissions` was opaque JSON to this service: the version-update paths stored whatever
// string arrived (it was not even checked for being JSON), and the specs inside it — argv, env/header
// keys, and the ${SECRET_NAME} references that are resolved when the worker RUNS — were never looked at.
// The rules now applied are the ones an OWNED definition has had since the secrets store existed
// (internal/mcpsettings/validate_inline.go).
//
// THE CONTRACT THAT MATTERS MOST HERE IS THE ONE ABOUT STORED DATA: a version that already carries a
// spec this gate would refuse must still be EDITABLE, because every client sends the permissions blob
// whole — so validating the incoming set in full would brick the save of a version whose only sin is
// carrying something written before the gate existed. Only what an edit CHANGES is judged, and the test
// that pins it seeds such a version directly in the column (the only way to produce pre-gate data).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/worker/ -run 'InlineSpecs' -v

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// permissionsOnVersion reads the version's permissions jsonb straight from the row, so an assertion is
// on the PERSISTED value rather than on a round-tripped proto.
func permissionsOnVersion(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int) string {
	t.Helper()
	var raw string
	err := pool.QueryRow(context.Background(),
		`SELECT permissions::text FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&raw)
	if err != nil {
		t.Fatalf("query permissions for %s v%d: %v", workerID, ver, err)
	}
	return raw
}

// seedPermissions writes a version's permissions column DIRECTLY, which is how the tests below produce
// the one thing the service can no longer produce: a version that predates this gate.
func seedPermissions(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int, perms string) {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(context.Background())
	if _, err := ttx.Tx.Exec(context.Background(),
		`UPDATE worker_versions SET permissions = $1 WHERE tenant_id = $2 AND worker_id = $3 AND version = $4`,
		perms, tenantID, workerID, ver); err != nil {
		t.Fatalf("seed permissions: %v", err)
	}
	if err := ttx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seeded permissions: %v", err)
	}
}

// A CHANGED spec is judged: a key no environment can carry is refused at save, and the version row is
// left exactly as it was.
func TestInlineSpecsChangedSpecRefusedOnVersionUpdate(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "inline-perms-refused")
	vid, ver := latestVersionID(t, pool, tenantID, id)

	bad := `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"NOT A KEY":"${TOKEN}"}}]}`
	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &bad,
	}))
	if err == nil {
		t.Fatal("a spec whose env key no environment can carry was saved")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("the refusal came back as %v, want InvalidArgument", got)
	}
	for _, want := range []string{"gh", "NOT A KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// THE ROW IS UNTOUCHED — a refused write must not have half-landed.
	if raw := permissionsOnVersion(t, pool, tenantID, id, ver); strings.Contains(raw, "NOT A KEY") {
		t.Errorf("a refused save still wrote the spec: %s", raw)
	}

	// The same spec with a lawful key saves, and the spec persists. (The value is a plain literal here:
	// a ${REFERENCE} would need the secret to exist, which the test below covers.)
	good := `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"GITHUB_TOKEN":"ghp_plaintext"}}]}`
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &good,
	})); err != nil {
		t.Fatalf("a lawful spec was refused: %v", err)
	}
	if raw := permissionsOnVersion(t, pool, tenantID, id, ver); !strings.Contains(raw, "GITHUB_TOKEN") {
		t.Errorf("the lawful spec did not persist: %s", raw)
	}
}

// A REFERENCE TO A SECRET NOBODY STORED is refused at save, which is the whole point: it used to be
// discovered per server when the worker RAN (mcpsettings.ResolveSecretRefs). With the secret stored it
// saves.
func TestInlineSpecsMissingSecretRefRefusedOnVersionUpdate(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "inline-perms-refs")
	vid, _ := latestVersionID(t, pool, tenantID, id)

	missing := `{"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"${NEVER_STORED}"}}]}`
	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &missing,
	}))
	if err == nil {
		t.Fatal("a spec referencing a secret the tenant does not hold was saved — the version would " +
			"now fail every session that resolved it")
	}
	if !strings.Contains(err.Error(), "NEVER_STORED") {
		t.Errorf("the refusal does not name the secret: %v", err)
	}

	// Store it (no KEK involved: existence is the question) and the same spec saves.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := db.CreateSecret(ctx, ttx.Tx, db.SecretRow{
		ID: db.NewID(), TenantID: tenantID, Name: "NEVER_STORED",
		Description: "seeded by the inline-ref test", Ciphertext: "not-a-real-ciphertext", KeyVersion: 1,
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit secret: %v", err)
	}
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &missing,
	})); err != nil {
		t.Fatalf("a spec referencing a STORED secret was refused: %v", err)
	}
}

// STORED DATA KEEPS WORKING: a version carrying a spec this gate would refuse is still editable, and
// still re-savable with the blob sent whole (which is what every client does). The moment that spec
// changes, the rules apply to it.
func TestInlineSpecsStoredBloatDoesNotBrickAnEdit(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "inline-perms-legacy")
	vid, ver := latestVersionID(t, pool, tenantID, id)

	// Pre-gate data, seeded directly: a spec with a key no environment can carry.
	legacy := `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"NOT A KEY":"${TOKEN}"}}]}`
	seedPermissions(t, pool, tenantID, id, ver, legacy)

	// 1. An unrelated edit (no permissions in the request) is untouched by the gate.
	note := "still editable"
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, VersionNote: &note,
	})); err != nil {
		t.Fatalf("an edit that did not touch permissions was refused: %v", err)
	}

	// 2. A WHOLE-BLOB re-save is the same no-op: the client resends what it read, and the stored spec
	//    is compared rather than re-judged. This is the failure this contract exists to prevent.
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &legacy,
	})); err != nil {
		t.Fatalf("a re-save of the version's OWN stored permissions was refused, which is how a stored "+
			"value bricks every later edit: %v", err)
	}

	// 3. Changing that spec brings it under the rules — and this is what the operator gets instead of a
	//    version that silently cannot run.
	changed := `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"NOT A KEY":"${TOKEN}","ALSO_BAD":"x"}}]}`
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &changed,
	})); err == nil {
		t.Error("a stored spec that was CHANGED was not judged")
	}
}

// PERMISSIONS IS A JSON FIELD LIKE THE OTHERS, and the version-update paths had stopped checking.
func TestVersionUpdateRefusesNonJSONPermissions(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "inline-perms-notjson")
	vid, ver := latestVersionID(t, pool, tenantID, id)

	junk := `{"mcp_servers":` // truncated: not JSON
	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId: id, VersionId: vid, Permissions: &junk,
	}))
	if err == nil {
		t.Fatal("permissions that is not JSON was saved")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("the refusal came back as %v, want InvalidArgument", got)
	}
	if raw := permissionsOnVersion(t, pool, tenantID, id, ver); strings.Contains(raw, "mcp_servers") {
		t.Errorf("a refused save still wrote the permissions: %s", raw)
	}
}
