# Owner-scoped MCP definitions + ONE union resolution (schema, migration, db layer, service, proto)

Implementation plan. Every claim below is read from the tree at `develop` @ `7bc517d9` in worktree
`.orchicon-worktrees/01M3SM0CMEPG0C7V8HQR461W9B`. Line numbers are from that revision.

This step produces the plan only. The numbered list in §11 is the deliverable for the SSE.

---

## 0. Established facts (do not re-derive)

- `mcp_servers` is declared in raw SQL only: `db/migrations/20260915000000_mcp_servers.sql`, with
  `UNIQUE ("tenant_id","name")` and policy `tenant_isolation`. Columns: `id, tenant_id, name, transport,
  command, args, env, url, headers, enabled, catalog_slug, install_status, install_result, created_at, updated_at`.
- **`db/schema.hcl` does NOT model `mcp_servers`, `project_mcp_servers` or `tenant_settings`** — `grep -c` over
  `db/schema.hcl` for all three names returns 0 (only `tenants`, `projects`, `workers`, … are declared, 1796
  lines, last table `category_assignments` at :1783). Consequence: there is **no `schema.hcl` edit to make**;
  only `make migrate-hash` (Makefile:257) to refresh `db/migrations/atlas.sum`. CI applies migrations through
  `go run ./tools/atlas-ci` (`.github/workflows/ci.yml:93`), which runs `internal/migrate.Run` over the
  embedded FS and never validates `atlas.sum`; `atlas.sum` is load-bearing for `make migrate` only.
- Selection today: join table `project_mcp_servers` (`20260915000001_project_mcp_servers.sql`) + tenant tier
  `tenant_settings.default_mcp_servers` jsonb (`20260915000002_tenant_default_mcp_servers.sql`).
- The rule is implemented three times: `internal/mcpsettings/configsource.go:109-133` (`ProjectSelection` with
  the tenant fallback at :126-131), `internal/mcpsettings/resolve.go:34-96` (`ResolveForScope`, full rule +
  `Disabled`), `internal/mcpclient/config.go:208-245` (`Resolve`, worker → project → none).
- `ask_orchicon_conversations` PK is **composite** `(tenant_id, id)` (`db/migrations/20260731000016_ask_orchicon.sql:15`).
- The demand-set traversal is `WorkflowReconciler.runNeedsServe`
  (`internal/scheduler/workflow_reconciler.go:159-208`); it resolves per-step versions via
  `db.GetWorkerVersionByNumber` (:189) / `db.GetLatestWorkerVersion` (:194) and feeds
  `adapter.AdapterDemandSet(refs...).NeedsServe(r.runtime.ServeDependent)` (:207). `tx == nil` fallback at
  :168-175. Pinned by `internal/scheduler/runtime_serve_gate_test.go` `TestRunNeedsServeAdapterKinds` (:97).
- `internal/workflow.StepWire` is at `internal/workflow/service.go:1628-1639`
  (`ID, Name, Kind, Ref, WorkerVersion int, DependsOn, GatePolicyRef, Config, PositionX, PositionY`).
- Import graph (verified): `internal/adapter` imports only `sort` + `internal/db`. `internal/workflow` imports
  `apiv1, db, audit, auth, domain, migrate, tenant, eventbus` — **not** adapter, **not** mcpsettings, **not**
  scheduler. `internal/db` does **not** import workflow (so `db → workflow` would be a cycle).
  `internal/mcpsettings` imports `db, mcpclient, secretcrypto, tenant, secrets, audit`. `internal/scheduler`
  imports `adapter, contextfiles, db, domain, reconciler, runtime, workflow`.
- `internal/db/project.go:423` cascades `DELETE FROM project_mcp_servers …`, and the comment block at :366-390
  calls it "the one real FK to projects".
- `MCPServerRefsFromPermissions` (`internal/db/mcp_servers.go:387-419`) parses only ids (`{"mcp_servers":[{"id":…}]}`
  or bare strings). The frontend writes `permissions.mcp_servers` as `{id, command?: string}`
  (`frontend/src/components/MCPPicker.tsx:11-14`, `WorkerFormSections.tsx:69-93`).
- **There is no `skill_files` table anywhere in the tree** (`grep -rn skill db/migrations/*.sql` → only the
  `skills` *text* prompt column at `20260722000000_worker_prompt_fields.sql:6`). Child 2 owns it. This child
  therefore ships the skills half of the union as an *inline-spec* collector with the seam child 2 fills
  (see §9). Acceptance criterion 1's assertion (both sets from one walk) is satisfiable now.

---

## 1. Decisions (one line each)

- **DECISION**: the ONE walk lives in a new leaf package `internal/adapter/runsteps.go` (adapter already owns
  the shared demand-set primitive and imports only `db`; adding `workflow` is acyclic). Rationale: `runNeedsServe`
  (scheduler) and the union builder (mcpsettings) are in different packages, so a shared home is the only way to
  have one walk; putting it in `db` would cycle through `workflow`, and in `mcpsettings` it would be unreachable
  by the demand path.
- **DECISION**: the run-scope union takes `runID` only: `Service.ResolveRunUnion(ctx, tenantID, runID)`. There is
  no signature anywhere in this child that accepts a `(workerID, version)` pair for RUN scope. AC 3 by construction.
- **DECISION**: resolution becomes a **union**, not a precedence chain: project-owned ∪ own-scoped, deduped,
  order-stable (project rows by `name`, then own rows in declaration order). The tenant tier is deleted, so
  "worker wins / project wins" no longer exists.
- **DECISION**: `UpsertMCPServer` conflict target changes from `(tenant_id, name)` to `(id)` (the PK).
  Create generates `uuid.NewString()` (`service.go:473`) and Update passes the existing id, so PK-conflict
  upsert is exact; name uniqueness moves to the partial unique indexes.
- **DECISION (revisitable)**: the storage resolver type/constructor is renamed
  `mcpsettings.ConfigSource`/`NewConfigSource` → `mcpsettings.Resolver`/`NewResolver` (one method +
  `ResolveRunUnion`), because the contract it satisfies is no longer the 3-method `ConfigSource`. `mcpclient`
  keeps the seam type (`ScopeResolver`). Cost: 4 one-line call-site edits (§7).
- **DECISION**: scope is **immutable after create** (like `name`, `service.go:557-559`); moving a definition is
  delete + create. Rationale: an owner change is a different row semantically, and it avoids an owner-swap
  racing the new partial unique indexes.
- **DECISION**: for the run scope, first-occurrence-wins on a duplicate inline server id (steps are visited in
  DAG order and the DB rows are visited first); recorded in the return value's provenance.

---

## 2. Migration (new files, timestamped after `20260928000000`)

New: `db/migrations/20260929000000_mcp_server_ownership.sql` and
`db/migrations/20260929000000_mcp_server_ownership_down.sql`.

Header comment must say **why the conversation FK is composite**: `ask_orchicon_conversations` has PK
`(tenant_id, id)` (`20260731000016_ask_orchicon.sql:15`), so a single-column `conversation_id → id` FK is
impossible (there is no unique index on `id` alone); the FK must carry `tenant_id` too, which also makes the
RLS-scoped cascade tenant-correct.

Statement order (order is load-bearing; each numbered block is one `ALTER`/`DO`):

1. `ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS project_id text NULL;`
   `ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS conversation_id text NULL;`
2. **Drop the old name uniqueness BEFORE cloning** — clones keep their source row's name, so while
   `UNIQUE(tenant_id,name)` exists the clone inserts fail. Defensive `DO` block dropping any unique constraint
   on `(tenant_id,name)` (the constraint is the auto-named `mcp_servers_tenant_id_name_key`, but do not hardcode):
   ```sql
   DO $$ DECLARE c record; BEGIN
     FOR c IN SELECT conname FROM pg_constraint
              WHERE conrelid='mcp_servers'::regclass AND contype='u' LOOP
       EXECUTE format('ALTER TABLE mcp_servers DROP CONSTRAINT %I', c.conname);
     END LOOP; END $$;
   ```
3. FKs:
   - `ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_project_fk FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE;`
   - `ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_conversation_fk FOREIGN KEY (tenant_id, conversation_id) REFERENCES ask_orchicon_conversations(tenant_id, id) ON DELETE CASCADE;`
4. Backfill, in this exact order, inside one `DO` block (see §3 for the SQL). This block is where the NOTICEs happen.
5. `ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_owner_xor CHECK ((project_id IS NULL) <> (conversation_id IS NULL));`
   — added **after** the backfill, because every pre-existing row is `(NULL, NULL)` and would violate it.
6. Partial indexes per owner:
   - `CREATE UNIQUE INDEX mcp_servers_project_name_key ON mcp_servers (tenant_id, project_id, name) WHERE project_id IS NOT NULL;`
   - `CREATE UNIQUE INDEX mcp_servers_conversation_name_key ON mcp_servers (tenant_id, conversation_id, name) WHERE conversation_id IS NOT NULL;`
   - `CREATE INDEX mcp_servers_project_idx ON mcp_servers (tenant_id, project_id) WHERE project_id IS NOT NULL;`
   - `CREATE INDEX mcp_servers_conversation_idx ON mcp_servers (tenant_id, conversation_id) WHERE conversation_id IS NOT NULL;`
   Created after the backfill so a bad backfill fails here loudly rather than blocking the clone inserts.
7. `DROP TABLE IF EXISTS project_mcp_servers;`
   `ALTER TABLE tenant_settings DROP COLUMN IF EXISTS default_mcp_servers;`

---

## 3. Backfill block (exact semantics)

```sql
DO $$
DECLARE
  ref record; clone_id text; n int := 0; orphan text; ownerless int := 0; cloned_secret int := 0;
BEGIN
  -- (1) FIRST referencing project takes the row itself. Deterministic:
  --     created_at ASC, project_id ASC (the work item's rule).
  WITH first_ref AS (
    SELECT DISTINCT ON (mcp_server_id) mcp_server_id, project_id
    FROM project_mcp_servers
    ORDER BY mcp_server_id, created_at ASC, project_id ASC
  )
  UPDATE mcp_servers m SET project_id = f.project_id
  FROM first_ref f WHERE m.id = f.mcp_server_id;

  -- (2) Every ADDITIONAL referencing project gets a CLONE (new id), all fields copied.
  FOR ref IN
    SELECT pms.mcp_server_id, pms.project_id,
           row_number() OVER (PARTITION BY pms.mcp_server_id ORDER BY pms.created_at ASC, pms.project_id ASC) AS rn
    FROM project_mcp_servers pms
  LOOP
    CONTINUE WHEN ref.rn = 1;
    clone_id := gen_random_uuid()::text;
    INSERT INTO mcp_servers (id, tenant_id, name, transport, command, args, env, url, headers, enabled,
                             catalog_slug, install_status, install_result, created_at, updated_at, project_id)
    SELECT clone_id, s.tenant_id, s.name, s.transport, s.command, s.args, s.env, s.url, s.headers, s.enabled,
           s.catalog_slug, s.install_status, s.install_result, s.created_at, now(), ref.project_id
    FROM mcp_servers s WHERE s.id = ref.mcp_server_id;
    n := n + 1;
    -- (2b) A cloned row whose credential name is ID-DERIVED points at a secret that does not exist:
    --      SecretNameFor (internal/mcpsettings/service.go:309-315) uses catalog_slug when set, ELSE the row id.
    IF (SELECT catalog_slug FROM mcp_servers WHERE id = clone_id) = ''
       AND (SELECT (env::text LIKE '%${%') OR (headers::text LIKE '%${%') FROM mcp_servers WHERE id = clone_id)
    THEN
      cloned_secret := cloned_secret + 1;
      RAISE NOTICE 'mcp clone % (project %, name %) has an ID-DERIVED secret name (no catalog slug); its credential must be RE-ENTERED in Settings -> Adapters -> MCP', clone_id, ref.project_id, (SELECT name FROM mcp_servers WHERE id = clone_id);
    END IF;
  END LOOP;
  RAISE NOTICE 'mcp ownership backfill: % definition(s) cloned across projects', n;
  RAISE NOTICE 'mcp ownership backfill: % clone(s) need credential re-entry', cloned_secret;

  -- (3) Tenant-default orphans: reported, NEVER cloned (no bounded target).
  FOR orphan IN SELECT jsonb_array_elements_text(default_mcp_servers) FROM tenant_settings LOOP
    RAISE NOTICE 'tenant default MCP server % is an ORPHAN after the tenant tier is removed and needs RE-ATTACHMENT to a project or conversation', orphan;
  END LOOP;

  -- (4) Remaining ownerless rows (unreachable through any project) are reported THEN deleted.
  SELECT count(*) INTO ownerless FROM mcp_servers WHERE project_id IS NULL AND conversation_id IS NULL;
  IF ownerless > 0 THEN
    RAISE NOTICE 'mcp ownership backfill: deleting % ownerless definition(s) (referenced by no project)', ownerless;
    DELETE FROM mcp_servers WHERE project_id IS NULL AND conversation_id IS NULL;
  END IF;
END $$;
```

Ordering guarantees the work item demands: (3) reports the tenant-default orphans **before** (4) deletes
ownerless rows, so an id that is both in the default set and ownerless is reported as an orphan *and* then
deleted, in that observable order. Clone names stay unique because the old `UNIQUE(tenant_id,name)` proved
names unique per tenant, hence per project after cloning; the new partial index in step 6 is the backstop that
makes a violation loud.

---

## 4. Down migration (`…_down.sql`)

1. Recreate the join table verbatim from `20260915000001_project_mcp_servers.sql` (table, `project_mcp_servers_mcp_idx`,
   `ENABLE`/`FORCE ROW LEVEL SECURITY`, `DROP POLICY IF EXISTS tenant_isolation` + `CREATE POLICY`).
2. Backfill it from ownership: `INSERT INTO project_mcp_servers (project_id, tenant_id, mcp_server_id) SELECT project_id, tenant_id, id FROM mcp_servers WHERE project_id IS NOT NULL ON CONFLICT DO NOTHING;`
3. Re-add the tenant tier: `ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS default_mcp_servers JSONB NOT NULL DEFAULT '[]'::jsonb;`
   (empty — the down migration cannot know the pre-up default set; say so in the header).
4. Drop the indexes, the XOR check, the two FKs, then the two columns
   (`DROP INDEX IF EXISTS …; ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS …; ALTER TABLE mcp_servers DROP COLUMN IF EXISTS project_id, DROP COLUMN IF EXISTS conversation_id;`).
5. Restore `ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_tenant_id_name_key UNIQUE (tenant_id, name);` —
   **will fail if two owners hold the same name**, which the up migration explicitly allows. State it in the
   header as a known, accepted irreversibility.
6. Header must also state: rows CLONED across N projects **cannot be un-cloned** (the up migration records no
   clone linkage), and conversation-owned rows have no representation in the join table — the down migration
   leaves them ownerless and reports the count via `RAISE NOTICE`.

Then: `make migrate-hash` (Makefile:257 → `cd db && atlas migrate hash --dir file://migrations`; `atlas` is on
PATH at `/usr/local/bin/atlas`). No `db/schema.hcl` edit (§0).

---

## 5. db layer — `internal/db/mcp_servers.go`

**Row shape** (`MCPServerRow`, :17-33): add `ProjectID string`, `ConversationID string`; extend
`mcpServerCols` (:35-36) with `project_id, conversation_id` (order appended so `scanMCPServer` :38-63 gains two
`&r.ProjectID, &r.ConversationID` scans before `&r.CreatedAt`).

**`UpsertMCPServer` (:110-151)**: `INSERT … ON CONFLICT (id) DO UPDATE SET … ` — replace the conflict target at
:131 and add `project_id`/`conversation_id` to the column list (:128-130), the `VALUES` (:130), the params
(:145) and the `DO UPDATE SET` list. Ownership is not mutable by upsert-with-same-id? It is — include both in
the `SET` list (the service never changes them; harmless and consistent).

**Replace** the join-table pair:
- delete `SetProjectMCPServers` (:205-221) and `ListProjectMCPServerIDs` (:223-242);
- add `ListMCPServersByOwner(ctx, tx, tenantID, projectID, conversationID string) ([]MCPServerRow, error)` —
  `SELECT <cols> FROM mcp_servers WHERE tenant_id=$1 AND ((project_id IS NOT NULL AND project_id=$2) OR (conversation_id IS NOT NULL AND conversation_id=$3)) ORDER BY name ASC`;
  used by the project scope (pass the conversation id empty) and the conversation scope.

**Delete**: `MCPServerReferencingProject` (:269-273), `MCPServerReferencingWorker` (:275-279),
`ListMCPServerReferences` (:281-349), `SetTenantDefaultMCPServers` (:351-364),
`GetTenantDefaultMCPServers` (:366-385).

**`MCPServerRefsFromPermissions` (:387-419) → the single parser, full specs + legacy.** New canonical function
and a delegating wrapper so nothing else changes shape:

```go
// InlineMCPServer is one inline MCP definition carried by a worker version's
// permissions jsonb ("mcp_servers"). Full specs and the legacy id-only /
// {id,command} shapes all decode into this.
type InlineMCPServer struct {
    ID      string            `json:"id"`
    Type    string            `json:"type,omitempty"`     // "stdio" | "http"
    Command []string          `json:"command,omitempty"`  // legacy string form is split on fields
    URL     string            `json:"url,omitempty"`
    Headers map[string]string `json:"headers,omitempty"`
    Env     map[string]string `json:"env,omitempty"`
    Enabled *bool             `json:"enabled,omitempty"`  // nil = enabled
    OnError string            `json:"onError,omitempty"`
}

// MCPServersFromPermissions is THE parser of worker_versions.permissions'
// "mcp_servers" jsonb. Three shapes decode here, nothing else parses that key:
//  1. full inline spec  {"id","type","command":["npx","-y","x"],"url","headers","env","enabled","onError"}
//  2. legacy reference  {"id":"01H…","command":"npx -y x"}   (command is a *string* -> strings.Fields)
//  3. legacy bare id    "01H…"
func MCPServersFromPermissions(permissions []byte) []InlineMCPServer

// MCPServerRefsFromPermissions keeps the id-only view; it DELEGATES to
// MCPServersFromPermissions (one parser, one place).
func MCPServerRefsFromPermissions(permissions []byte) []string
```

Implementation: `json.Unmarshal` into `struct{ MCPServers []json.RawMessage }` (as today, :395-400); for each
raw: if it starts with `{` unmarshal into a local shape with `Command json.RawMessage` — if it starts with `[`
it is the full array form, else a JSON string → `strings.Fields`; if the raw is a bare string, `ID` = it with
quotes trimmed. `Enabled *bool` nil → treated as enabled by the caller.

**`internal/db/mcp_servers_by_name.go:12`**: rename to owner-scoped
`GetMCPServerByNameForOwner(ctx, tx, tenantID, projectID, conversationID, name string) (MCPServerRow, error)` with
the same OR-owner predicate as `ListMCPServersByOwner`; keep the old `GetMCPServerByName` name? No — its single
caller is `service.go:467` and it is retargeted there, so the old function is replaced outright.

**`internal/db/project.go`**: replace cascade step 15 at :423
(`DELETE FROM project_mcp_servers …`) with `DELETE FROM mcp_servers WHERE tenant_id = $1 AND project_id = $2`
(the FK `ON DELETE CASCADE` would do it, but the function's stated contract is explicit, ordered, leaf-first
deletion) and update the comment block at :366-390/:388 ("the one real FK to projects") accordingly.
**New**: `RequireConversation(ctx, tx, tenantID, conversationID string) error` in `internal/db/ask_orchicon.go`
— `SELECT 1 FROM ask_orchicon_conversations WHERE tenant_id=$1 AND id=$2`, `ErrNotFound` when absent — the
conversation twin of `RequireProjectActive` (`internal/db/project.go:468`) for `CreateInput` validation.

---

## 6. Service — `internal/mcpsettings/service.go`

- **Package doc comment (:1-11)**: rewrite — tenant-scoped, owner-scoped (project XOR conversation)
  definitions; no tenant-default tier; resolution is ONE union (`§8`), not `worker → project → tenant default → none`.
- **Delete** `ReferencingProject` (:73-77), `ReferencingWorker` (:79-83), `ErrReferenced` (:85-87),
  `ReferencedError` (:89-110), and their use inside `Delete` (:655-668). An owned row cannot be orphaned: every
  reference *is* the row's owner.
- **`Entry` (:114-131)**: add `ProjectID string`, `ConversationID string`; `entryFromRow` (:171-197) copies them.
- **`CreateInput` (:394-404) / `UpdateInput` (:524-539)**: add `ProjectID, ConversationID string`. Validation in
  `validateCreate` (:406-453): exactly one non-empty (else `invalidf("exactly one of project_id or conversation_id must be set")`);
  `db.RequireProjectActive(ctx, tx, tenantID, projectID)` for a project owner (the call already exists at :873,
  move/reuse it); `db.RequireConversation(...)` for a conversation owner. `Update` rejects an owner change with
  `invalidf("owner is immutable after create; delete + recreate to move")`, mirroring `name` at :557-559.
- **`Create` name pre-check (:465-471)**: `GetMCPServerByNameForOwner(...)` with the input's owner; the partial
  unique index is the backstop. Also map a `23505` on `mcp_servers_project_name_key` /
  `mcp_servers_conversation_name_key` to the same `invalidf` (the pre-check races).
- **`Create` (:472-485)** passes `ProjectID`/`ConversationID` into `db.MCPServerRow`; **`Update` (:622)** keeps
  `merged.ProjectID/ConversationID` (already carried by the row read at :552).
- **Delete** `SetProjectSelection` (:862-887), `GetProjectSelection` (:889-897), `SetTenantDefaultSelection`
  (:899-918), `GetTenantDefaultSelection` (:920-928). `validateIDs` (:930-952) loses its only two callers →
  delete it with them (keep `dedupIDs` (:954) if §8's union still uses it; otherwise delete).
- **Keep unchanged**: `RequiredSecretsFor` (:266-295), `SecretNameFor` (:305-315), `secretRefName` (:297-303),
  `validateSecretRefs` (:325-349), the whole catalog (`registry.go`), the install path (`install.go`),
  `SetSecret`/`ClearSecret` (:710-858), `ListForTenant` (:213-236), audit records.

---

## 7. Resolution: ONE resolver, THREE input shapes

### 7a. `internal/mcpclient/config.go` — the seam (types only, no rule)

Delete `ConfigSource` (:94-110), `NoopConfigSource` (:112-124), `workerPermissionSelection`/`mcpSelectionEntry`
(:126-137), `ManifestConfigSource` + its three methods (:139-185), `Resolved` (:187-200), `Resolve` (:202-245).
The package doc (:1-13) loses the "worker selection → project selection → none" sentence.

Add:

```go
// ScopeKind addresses a resolution scope. RUN is run-shaped on purpose: its
// result is applied ONCE per container (child 4), so it must never be derived
// from an executing worker.
type ScopeKind string

const (
    ScopeProject      ScopeKind = "project"
    ScopeConversation ScopeKind = "conversation"
    ScopeWorker       ScopeKind = "worker"
    ScopeRun          ScopeKind = "run"
)

// ScopeRef addresses exactly one scope. The zero value resolves to nothing.
type ScopeRef struct {
    Kind           ScopeKind
    ProjectID      string
    ConversationID string
    WorkerID       string // ScopeWorker only; the version is the latest published
    RunID          string // ScopeRun only
}

// ScopedServer is one resolved server plus its provenance (which scope
// supplied it) — the caller logs and reports it.
type ScopedServer struct {
    Spec   ServerSpec
    From   ScopeKind
    FromID string // "project:<id>" | "conversation:<id>" | "worker:<id>@<n>" | "inline:<workerID>@<n>"
    EntryID string // the mcp_servers row id, "" for an inline (worker-owned) definition
}

// Resolution is the outcome of resolving one scope.
type Resolution struct {
    Servers     []ScopedServer
    SelectedIDs []string
    Missing     []string // selected ids with no matching row/definition
    Disabled    []string // resolved definitions whose enabled flag is false
    Skills      []InlineSkillFile
}

// ScopeResolver is the ONE resolution contract. Exactly one implementation
// exists (mcpsettings.Resolver).
type ScopeResolver interface {
    ResolveScope(ctx context.Context, ref ScopeRef) (Resolution, error)
}

// NoopScopeResolver resolves every scope to nothing (tests, unwired planes).
type NoopScopeResolver struct{}

func (NoopScopeResolver) ResolveScope(context.Context, ScopeRef) (Resolution, error) {
    return Resolution{}, nil
}
```

### 7b. `internal/adapter/runsteps.go` (NEW) — the ONE walk

```go
// RunStepResolution is one worker-bearing step of a run, resolved the way DISPATCH resolves it
// (step-pinned version by NUMBER -> latest published).
type RunStepResolution struct {
    StepID        string
    Kind          string
    WorkerID      string
    WorkerVersion int
    ModelRef      string          // verbatim; "" = unresolvable (conservative, see callers)
    Version       db.WorkerVersionRow
}

type RunStepResolutions []RunStepResolution
func (r RunStepResolutions) ModelRefs() []string // verbatim, in step order

// ResolveRunSteps is THE one walk over a run's steps: it resolves each
// task/approval step's worker version once, and BOTH consumers read that one
// result — the adapter demand set (scheduler.runNeedsServe) and the
// MCP/skills union (mcpsettings.ResolveRunScope), so the two cannot drift.
// Mirrors runNeedsServe's current behaviour exactly: non-worker kinds skipped,
// empty Ref skipped, version-by-NUMBER then latest-published, load failures
// folded to the zero value (empty ModelRef).
func ResolveRunSteps(ctx context.Context, tx pgx.Tx, tenantID string, steps []workflow.StepWire) RunStepResolutions
```

Body = the loop currently at `internal/scheduler/workflow_reconciler.go:176-199` verbatim (same
`domain.StepKindTask`/`StepKindApproval` switch, same `s.Ref == ""` skip, same `GetWorkerVersionByNumber`
then `GetLatestWorkerVersion(…, true)` fallback), extended to also keep the `db.WorkerVersionRow` it already
read. `internal/adapter` gains an `internal/workflow` import — acyclic (§0).

### 7c. `internal/scheduler/workflow_reconciler.go` — extend, do not duplicate

`runNeedsServe` (:159-208) keeps its `runtimeEnabled` guard, its `tx == nil` tenant-tx fallback (:168-175) and
its doc, but its body from :176 becomes:

```go
res := adapter.ResolveRunSteps(ctx, tx, tenantID, steps)
// ONE computation, ONE place (AC 7): the same walk that feeds the MCP/skills
// union also feeds the adapter demand set.
return adapter.AdapterDemandSet(res.ModelRefs()...).NeedsServe(r.runtime.ServeDependent)
```

`internal/scheduler/runtime_serve_gate_test.go` `TestRunNeedsServeAdapterKinds` (:97) must pass unchanged —
the walk reproduces the old per-step behaviour byte for byte.

### 7d. `internal/mcpsettings/resolver.go` (NEW; replaces `internal/mcpsettings/configsource.go` and `internal/mcpsettings/resolve.go`)

```go
// Resolver is the ONE implementation of mcpclient.ScopeResolver: the storage-backed,
// scope-addressed resolution of MCP definitions (project-owned ∪ scope-owned), union,
// deduped, order-stable, plus the skills union for the run scope.
type Resolver struct{ pool *db.Pool }
func NewResolver(pool *db.Pool) *Resolver
var _ mcpclient.ScopeResolver = (*Resolver)(nil)

func (r *Resolver) ResolveScope(ctx context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
    switch ref.Kind {
    case mcpclient.ScopeProject:      return r.resolveProject(ctx, ref.ProjectID)
    case mcpclient.ScopeConversation: return r.resolveConversation(ctx, ref.ProjectID, ref.ConversationID)
    case mcpclient.ScopeWorker:       return r.resolveWorker(ctx, ref.ProjectID, ref.WorkerID)
    case mcpclient.ScopeRun:          return r.resolveRun(ctx, ref.RunID)
    }
    return mcpclient.Resolution{}, nil
}

// ResolveRunUnion is the run-scope entry point for callers that hold a run id.
// It takes the RUN (and, internally, its project) and NEVER an executing worker:
// its result is applied ONCE per container (child 4), so a per-execution input
// would make it order-dependent.
func (r *Resolver) ResolveRunUnion(ctx context.Context, tenantID, runID string) (mcpclient.Resolution, error)
```

`resolveRun(ctx, runID)`: one tenant tx → `db.GetWorkflowRun` (project id) → `db.GetWorkflowVersion` +
`workflow.ParseSteps` → `adapter.ResolveRunSteps(ctx, tx, tenantID, steps)` → for each resolution with a
non-empty `Version.Permissions`, `db.MCPServersFromPermissions(v.Permissions)` and
`db.SkillsFromPermissions(v.Permissions)`; union with `resolveProject`'s rows. Deduped by (a) entry id for DB
rows, (b) inline spec id for inline specs, first occurrence wins; ordering: project rows by `name` ASC, then
inline specs in step order. Missing/Disabled come from DB rows only (an inline spec is never "missing").

`resolveProject` / `resolveConversation` / `resolveWorker` all funnel through one helper
`resolveRowsAndInline(ctx, tx, tenantID, projectID, inline []db.InlineMCPServer, from ScopeKind, fromID string)`:
DB rows via `db.ListMCPServersByOwner`, enabled-only (`Disabled` collects the rest), converted to
`mcpclient.ServerSpec` by the existing mapper moved here from `configsource.go:58-78`
(`Command = append([]string{r.Command}, r.Args...)`, `TransportStreamable|"http"` → `TypeHTTP`, else `TypeStdio`.
`internal/db` must expose `TransportStreamable` — use the literal `"streamable-http"` + `"http"` as the current
code does, or import the const; keep the current switch as-is).
- **project scope**: `ListMCPServersByOwner(project)`.
- **conversation scope**: `ListMCPServersByOwner(project ∪ conversation)` — both owners in one call.
- **worker scope**: `ListMCPServersByOwner(project)` ∪ the latest published version's inline specs
  (`workerPermissions` helper, currently `resolve.go:100-113` — move it into `resolver.go`, keep the
  `ORDER BY wv.version DESC LIMIT 1` behaviour for the worker shape only).
- **run scope**: `ListMCPServersByOwner(project)` ∪ every step worker version's inline specs.

**Delete** `internal/mcpsettings/configsource.go`'s `ConfigSource` (:33-135) and
`internal/mcpsettings/resolve.go`'s `ResolveResult`/`ResolveForScope` (:17-96) — `Resolver` is the ONE
implementation. **Keep** `ResolveSecretRefs` (`configsource.go:137-202`) — move it verbatim into `resolver.go`
(or a `secrets.go`) since `configsource.go` goes away; `server.go:611-612,714-715` depends on it.

---

## 8. Mechanical repointing (keep the tree compiling; child 3 owns worker-config correctness)

| file:line | change |
|---|---|
| `internal/orchicon/bridge.go:73` | field `mcpConfig mcpclient.ConfigSource` → `mcpResolver mcpclient.ScopeResolver` |
| `internal/orchicon/bridge.go:187-193` | `SetConfigSource` → `SetScopeResolver(src mcpclient.ScopeResolver)` (retarget nil-guards) |
| `internal/orchicon/mcptools.go:46` | `if b.mcpResolver == nil { return nil, nil }` |
| `internal/orchicon/mcptools.go:50` | `res, rerr := b.mcpResolver.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeProject, ProjectID: exec.ProjectID})` — project scope only, per the task |
| `internal/orchicon/mcptools.go:41` doc comment | drop the `worker → project → tenant-default → none` sentence |
| `internal/claude/adapter.go:67` | field type → `mcpclient.ScopeResolver` |
| `internal/claude/ask.go:452-455` | `SetConfigSource` → `SetScopeResolver` |
| `internal/claude/mcpresolve.go:3-34` header | rewrite the "three small types / mcpclient.Resolve" paragraph |
| `internal/claude/mcpresolve.go:67` | `b.mcpResolver.ResolveScope(ctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeProject, ProjectID: projectID})` |
| `internal/server/server.go:610`, `:713` | `mcpsettings.NewConfigSource(pool)` → `mcpsettings.NewResolver(pool)` |
| `internal/mcpclient/manager_test.go:340,347,361` | rewrite the three `Resolve`/`ManifestConfigSource` uses against `NoopScopeResolver` / a stub `ScopeResolver` (asserts `Resolution.Servers`) |

---

## 9. Skills half of the union (child 2's seam)

`internal/db/` gains one parser, next to the MCP one, with the same "single parser of that jsonb key" rule:

```go
// InlineSkillFile is one inline skill file carried by a worker version's
// permissions jsonb ("skill_files"). Child 2's skill_files table, when it
// lands, extends the SAME collector in adapter.ResolveRunSteps — this child
// only guarantees the union returns the inline half.
type InlineSkillFile struct {
    Path    string `json:"path"`
    Content string `json:"content"`
}

// SkillsFromPermissions is THE parser of worker_versions.permissions'
// "skill_files" array. Malformed/absent -> nil.
func SkillsFromPermissions(permissions []byte) []InlineSkillFile
```

`Resolution.Skills` is the deduped (by `Path`, first occurrence wins), order-stable union across the run's
step versions. AC 1's test asserts both sets come from the same walk; when child 2 adds the table, its rows are
appended to the same `Resolution.Skills` slice in `adapter.ResolveRunSteps`'s consumer — no second walk.

---

## 10. Proto + handler

`proto/orchicon/api/v1/mcp_server_service.proto`:
- **Remove** `SetProjectMCPServers`, `GetProjectMCPServers`, `SetTenantDefaultMCPServers`,
  `GetTenantDefaultMCPServers` (:31-34).
- `MCPServerListRequest` (`mcp_server.proto:105`) gains `string project_id = 1; string conversation_id = 2;`
  (both empty = tenant-wide listing, preserving today's `ListForTenant` behaviour).
- `MCPServerCreateRequest` (`mcp_server.proto:117-127`) gains `string project_id = 10; string conversation_id = 11;`.
- **Remove** the messages `ProjectMCPServersSet/GetRequest|Response`, `TenantDefaultMCPServersSet/GetRequest|Response`
  (`mcp_server.proto:191-218`).
- Service doc comment (:7-14) loses "project/tenant-default selections".
- Regenerate: `make gen` (Makefile:124, `buf generate`; `buf` is on PATH at `/usr/local/bin/buf`). That
  regenerates `api/gen/go/…` **and** `frontend/src/api/gen/…`; the deleted RPCs vanish from
  `apiv1connect/mcp_server_service.connect.go:68-79,95-98,279-315,391-446,500-513`.

`internal/mcpsettings/handler.go`:
- `ListMCPServers` (:63-77): pass the request's scope into a new `Service.ListForScope(ctx, tenantID, projectID, conversationID)`.
- `CreateMCPServer` (:97-107): `CreateInput{… ProjectID: msg.ProjectId, ConversationID: msg.ConversationId}`.
- **Delete** the four selection handlers (:272-324) and the `ReferencedError` branch in `DeleteMCPServer`
  (:166-181) → plain `h.mapErr(err)`; `mapErr` (:49-61) also drops the `ReferencedError` case at :50-53.
- Keep the error convention: `errInvalidArgument` → `CodeInvalidArgument` (`:54-56`), `db.ErrNotFound` →
  `CodeNotFound`.

`internal/askorchicon/tool_mcpservers.go`: delete `toolSetProjectMCPServers` (:263-277) and
`toolSetTenantDefaultMCPServers` (:279-292); `toolCreateMCPServer` (:99-131) forwards `project_id`/`conversation_id`
from its params. `internal/askorchicon/tools.go`: delete the `set_project_mcp_servers` (:940-947) and
`set_tenant_default_mcp_servers` (:948-955) tool entries, update the `create_mcp_server` description, and the
`delete_mcp_server` description (:913-918) which currently promises the reference guard.

**Compile breaks this forces (children 6/7 own the proper UX; these are the minimal edits, flagged):**
- `internal/tui/launch.go:427-431` calls `SetProjectMCPServers` — delete that block (and the now-unused
  `mcpChosen` :395 / `mcpLoaded` :396 bindings); the project-create form's MCP multi-select (:345,
  `work.PROJECTMCPField`) is child 7's to re-home onto the new scope-aware create.
- `frontend/src/api/mcpServers.ts:127-164`: delete `useGetProjectMCPServers`, `useSetProjectMCPServers`,
  `useGetTenantDefaultMCPServers`, `useSetTenantDefaultMCPServers`.
- `frontend/src/components/MCPServersTab.tsx:113,122` and its `setDefault` usage: drop the tenant-default state
  (child 7 replaces it with a scope picker). `MCPServersTab.test.tsx:62` asserts the deleted hook — update.
- `frontend/src/components/MCPPicker.test.tsx:41,48` asserts `useSetProjectMCPServers` — update.

---

## 11. Numbered implementation list (mechanically executable)

1. Create `db/migrations/20260929000000_mcp_server_ownership.sql` per §2 (columns → drop old unique → FKs →
   backfill `DO` block per §3 → XOR check → partial indexes → drop join table → drop tenant column), with the
   composite-FK rationale in the header.
2. Create `db/migrations/20260929000000_mcp_server_ownership_down.sql` per §4, with the clone/duplicate-name
   irreversibility stated in the header.
3. Run `make migrate-hash`; confirm `db/migrations/atlas.sum` gains the four new lines. Do **not** edit
   `db/schema.hcl` (`mcp_servers` is not modelled there — §0).
4. `internal/db/mcp_servers.go`: add `ProjectID`/`ConversationID` to `MCPServerRow` + `mcpServerCols` +
   `scanMCPServer`; retarget `UpsertMCPServer`'s conflict to `(id)` and include the two owner columns; replace
   `SetProjectMCPServers`/`ListProjectMCPServerIDs` with `ListMCPServersByOwner`; delete
   `ListMCPServerReferences` + the two `MCPServerReferencing*` types +
   `SetTenantDefaultMCPServers`/`GetTenantDefaultMCPServers`.
5. `internal/db/mcp_servers.go`: add `InlineMCPServer` + `MCPServersFromPermissions` (three shapes: full spec,
   `{id,command:string}`, bare id) and reimplement `MCPServerRefsFromPermissions` as a delegating wrapper.
6. `internal/db/mcp_servers.go`: add `InlineSkillFile` + `SkillsFromPermissions` (parses `skill_files`).
7. `internal/db/mcp_servers_by_name.go`: replace `GetMCPServerByName` with
   `GetMCPServerByNameForOwner(ctx, tx, tenantID, projectID, conversationID, name)`.
8. `internal/db/ask_orchicon.go`: add `RequireConversation(ctx, tx, tenantID, conversationID) error`
   (`ErrNotFound` when absent).
9. `internal/db/project.go`: replace cascade step 15 (:423) with the owner-scoped `mcp_servers` delete and fix
   the comment block (:366-390).
10. Create `internal/adapter/runsteps.go` per §7b (the ONE walk, lifted verbatim from
    `workflow_reconciler.go:176-199`, extended to return the version rows).
11. `internal/scheduler/workflow_reconciler.go`: `runNeedsServe` body (:176-207) → `adapter.ResolveRunSteps` +
    `AdapterDemandSet(res.ModelRefs()...)`; keep the `tx == nil` fallback and the doc;
    `TestRunNeedsServeAdapterKinds` must stay green.
12. `internal/mcpclient/config.go`: delete `ConfigSource`, `NoopConfigSource`, `ManifestConfigSource`,
    `workerPermissionSelection`, `mcpSelectionEntry`, `Resolved`, `Resolve`; add `ScopeKind` consts, `ScopeRef`,
    `ScopedServer`, `Resolution`, `ScopeResolver`, `NoopScopeResolver` per §7a; fix the package doc.
13. Create `internal/mcpsettings/resolver.go` per §7d; move `ResolveSecretRefs` and `workerPermissions` into it.
14. Delete `internal/mcpsettings/configsource.go` and `internal/mcpsettings/resolve.go`.
15. `internal/mcpsettings/service.go` per §6: doc comment; delete the reference guard + `ReferencedError` and its
    `Delete` use; `Entry`/`CreateInput`/`UpdateInput` owner fields + validation; owner-scoped name pre-check and
    `23505` mapping; owner-immutability in `Update`; delete the two selection pairs and `validateIDs`; add
    `ListForScope`.
16. `internal/orchicon/bridge.go`, `internal/orchicon/mcptools.go`, `internal/claude/adapter.go`,
    `internal/claude/ask.go`, `internal/claude/mcpresolve.go`, `internal/server/server.go:610,713`,
    `internal/mcpclient/manager_test.go` repoints per §8 (project scope only).
17. `proto/orchicon/api/v1/mcp_server_service.proto` + `mcp_server.proto` per §10; run `make gen`.
18. `internal/mcpsettings/handler.go` per §10 (scope-aware list/create; four handlers deleted; guard branch
    deleted; `mapErr` guard case deleted).
19. `internal/askorchicon/tool_mcpservers.go` + `tools.go` per §10 (two tools and their two functions deleted;
    create forwards the owner; descriptions updated).
20. Minimal frontend + TUI edits per §10's compile-break list (children 6/7 own the real UX).
21. Rewrite `internal/db/mcp_servers_test.go`: owner-scoped upsert + `ListMCPServersByOwner`; the XOR check
    rejects both-null and both-set; the composite conversation FK cascades on conversation delete; two projects
    may each own a `postgres`-named row but one owner may not hold two; `MCPServersFromPermissions` full-spec and
    legacy id-only/`{id,command}` shapes. Delete the join-table and tenant-default cases (:59-63,:87-157).
22. Rewrite `internal/mcpsettings/service_test.go`: `cleanupMCP` (:54-77) drops the join-table and
    default-column statements; `TestMCPSelectionsAndResolution` (:341-456) becomes the **four-case union test for
    both scopes** (project-only, own-only, both, neither) using `ResolveScope` with `ScopeRef{Kind: ScopeProject}`
    and `ScopeRef{Kind: ScopeConversation}`; the guard assertions (:423-455) are replaced by "an owned row deletes
    cleanly and its derived secrets are purged"; add owner validation failures (both owners set, unknown project,
    unknown conversation).
23. Add `internal/mcpsettings/union_run_test.go`: seed a 2-step run on one project whose two steps use two
    DIFFERENT worker versions with DIFFERENT inline `mcp_servers` sets → `Resolver.ResolveRunUnion(ctx, tenant, runID)`
    contains the project-owned server **and both** inline specs (the case a per-execution resolution cannot
    produce), order-stable, each carrying its `From`/`FromID` provenance, plus the `Skills` union from both
    versions. Seed steps via `db.CreateWorkflowRun` + `workflow.ParseSteps`-shaped JSON (mirror
    `internal/scheduler/runtime_serve_gate_test.go:106-125`'s worker seeding).
24. Add `internal/adapter/runsteps_test.go` (DB-backed, `ORCHICON_TEST_DSN`-guarded like the sibling suites):
    `ResolveRunSteps` returns one entry per task/approval step with `ModelRef`/`Version` populated, skips
    decision/parallel/work_item steps, and folds an unresolvable worker to an empty `ModelRef` — the property
    `runNeedsServe` now inherits.
25. Verify: `go build ./...`; `go test ./internal/db/ ./internal/mcpsettings/ ./internal/adapter/
    ./internal/scheduler/ ./internal/mcpclient/ ./internal/orchicon/ ./internal/claude/` with
    `ORCHICON_TEST_DSN` set against the in-container sandbox plane; `make ci-go`; `make migrate-hash` applied.

---

## 12. Integration map (what the next children must learn)

- **Depends on**: `projects(id)`; `ask_orchicon_conversations(tenant_id,id)`; the tenant secrets store
  (unchanged); `adapter.AdapterDemandSet` (unchanged) and the new `adapter.ResolveRunSteps`.
- **Depended on by**: child 2 (skills add rows to `Resolution.Skills` in the same walk), child 3 (feeds version
  inline specs into the WORKER scope and logs `ScopedServer.From`), child 4 (bakes `ResolveRunUnion` into the
  container once), child 5 (reads the demand set the same walk produces), children 6/7 (the Ask tool surface and
  the GUI/TUI repoint onto scope-aware create/list).
- **Must learn about it**: the `ConfigSource` → `ScopeResolver` contract change; the two deleted selection RPCs
  and their four service methods; the three input shapes; the new owner-scoped name-uniqueness rule (two projects
  may each own `postgres`); the migration's NOTICE behaviour and the two irreversibilities.
- **Observes it**: the migration's NOTICE output on a seeded DB, plus the AC-1 run-scope/union test and
  `go build ./...` on the touched packages.

## 13. Risks the implementation must respect

- The RUN-scope signature is the trap: `ResolveRunUnion(ctx, tenantID, runID)` only — accepting an executing
  worker makes the value order-dependent once baked into a container.
- Owner-scoped name uniqueness is a behavioural change (intentional).
- Cloned definitions with id-derived secret names (`catalog_slug = ''` + a `${…}` ref) need operator re-entry —
  the migration reports, never mints an unreachable name.
- The tenant-default orphan NOTICE must precede the ownerless-row delete (`§3` order).
- Frontend/TUI do not compile against the removed RPCs; the minimal edits in §10 are required, and children 6/7
  own the replacement UX.
