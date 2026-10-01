# ADR-0012: Owner-scoped MCP definitions — one union, resolved by scope

## Status

Accepted. **Supersedes [ADR-0008](0008-mcp-client-stdio-streamable-http-config-resolution.md) decisions 4 and 6** (config resolution order). ADR-0008's transports, lifecycle, and registry-merge decisions (1, 2, 3, 5, 7) stand.

## Context

ADR-0008 decision 4 fixed MCP config resolution as a **precedence chain** — `worker selection → project selection → none` — over a tenant-configured server list, with the storage contract owned by a sibling adapter-settings task. That sibling task built the storage as **three tiers**:

- migration `db/migrations/20260915000000_mcp_servers.sql` — the tenant-wide server list;
- migration `db/migrations/20260915000001_project_mcp_servers.sql` — a `project_mcp_servers` **join** table (project *references* a server);
- migration `db/migrations/20260915000002_tenant_default_mcp_servers.sql` — a **tenant-wide default** on `tenant_settings.default_mcp_servers`.

Together with the worker tier that produced the four-tier order `worker → project → tenant default → none`, which then had to be implemented **three times** — once per adapter/bridge that consumed it — because each re-derived the same walk over the same list.

Two structural problems fell out of that shape:

1. **The tenant tier made provenance unknowable.** A tenant-wide default has no bounded owner, so an id on it that went missing had no target to be re-attached to. This is recorded in the header of the migration that removed it (`db/migrations/20260929000000_mcp_server_ownership.sql:17-23`).
2. **A *reference* model needs a delete guard.** Because a project or the tenant default could *point at* a shared definition, deleting a definition had to be blocked while any reference existed — a guard that existed only to protect a sharing model nobody actually needed.

## Decision

1. **Resolution becomes ONE union, addressed by SCOPE.** `effective(project, own) = project ∪ own`, deduped and order-stable. No exclusion, no precedence, no fall-through. Implemented **once** in `mcpsettings.Resolver.ResolveScope` (`internal/mcpsettings/resolver.go`), consumed by every adapter through the neutral `mcpclient.ScopeResolver` interface. Scopes are `project | conversation | worker | run` (`mcpclient.ScopeKind`, `internal/mcpclient/config.go`).

2. **Selection IS ownership — definitions are OWNED, never shared.** A definition belongs to **exactly one owner**: `mcp_servers.project_id XOR mcp_servers.conversation_id`, enforced by a DB CHECK constraint (`db/migrations/20260929000000_mcp_server_ownership.sql`). The `project_mcp_servers` join table and `tenant_settings.default_mcp_servers` column are **dropped** by the same migration.

   Because a row can never be shared, it can never be orphaned — and the **reference/deletion guard was deleted with it**. Deleting an owner cascades to its definitions; deleting a definition is always safe. The owner column *is* the selection.

3. **Worker-scoped definitions live inline in the immutable `worker_versions.permissions`**, never as a row. A worker's MCP servers are carried as inline specs in the version's `permissions` jsonb (`mcp_servers: [{id, command, ...}]`).

   *Why inline and not a row:* a row that **referenced** a worker version would let an edit to the referenced definition mutate the resolution of an **already-published** version. Inline keeps a published version's server set frozen with the version. A pinned dispatch resolves the version it pinned (the caller-held `ScopeRef.OwnPermissions`); a non-pinned caller falls back to the latest published version. Provenance is labelled `inline:<workerID>@<n>`.

4. **Credentials stay tenant-scoped, on purpose.** `tenant_secrets` is `(tenant_id, name)` with RLS (`db/migrations/20260912000000_secrets_store.sql`), and `SetMCPServerSecret` remains tenant-scoped. RLS needs a tenant to key on, and a secret is not a definition — the "there is no tenant level" rule in this ADR is about **MCP definitions**, not credentials. A definition's env/header values are `${SECRET_NAME}` references into the tenant secrets store, resolved to plaintext before connect.

5. **opencode per-serve constraint and the strategy chosen.** `opencode serve` exposes `POST /mcp` (add a server) but **no `DELETE /mcp/{name}`** — a serve's configured set is fixed for that process's lifetime. A single long-lived host serve therefore cannot be reconfigured per session.

   The strategy landed by the opencode child: a **pool of host serves, one per resolved set, each with its own data dir** (`HostServePool`, `internal/opencode/servepool.go`) — a session lands only on a serve whose baked config is exactly the set that session is entitled to — plus a **run-level union baked at container creation** (`RunMCP` / `RunSkills`, `internal/opencode/config.go`). The empty set reuses the single default serve, so an MCP-less plane keeps the previous one-process topology. Keying a serve by its exact set is also what makes "cross-project union is forbidden" safe to state absolutely: unioning several projects onto one serve would leak one project's servers into another project's session, with no consent boundary.

## Consequences

- **One resolution implementation instead of three.** Every adapter consumes the same `McpResolver`/`ScopeResolver` and only *renders* the result into its own config format; provenance is now knowable (`Resolution.From` labels the scope / `inline:<worker>@<n>`).
- **No orphan is possible and no reference guard exists.** Deleting an owner cascades to its definitions; there is no "clear the references first" failure mode.
- **A published worker version's server set is immutable with the version** — an edit cannot retroactively change what a shipped version resolves to.
- **`list_mcp_catalog` and `install_mcp_server` remain meaningful.** The curated catalog is **code-defined** and scope-independent (`internal/mcpsettings/registry.go`); only the definitions added *from* it are owner-scoped.
- **Credentials are still tenant-scoped.** "No tenant tier" must not be over-read into the secrets store.
- Migration note: the change is additive + backfilling (`20260929000000_mcp_server_ownership.sql`), reporting rather than cloning any orphaned tenant-default id; paired with its `_down` sibling.
