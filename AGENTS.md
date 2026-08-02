# AGENTS.md — Development Guidelines for AI Agents

> This file is the entry point for any AI agent working on Orchicon.
> Read it before making any changes.

## Project

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `DOCUMENTATION.md` — single comprehensive documentation file

## WARNING

> **STOP!** Are you in a custom branch or are you on `main`? If you are on `main`. **DO NOT proceed.**
> Instead, create a new branch and continue. Keep this in your memory context **AT ALL TIMES**.
> This is **RULE number 1**.
>
> Check your branch at the very start of every task, before reading files,
> before writing code, before anything. `git branch --show-current` is the
> first command you run on every new task.

## Token discipline

The project's model spend is rising. Be economical but **never at the expense of rigor**:

- **Efficiency is not an excuse for shortcuts.** Never patch around a symptom without first identifying the root cause through proper troubleshooting. Verbose investigation is cheaper than a second pass that has to undo incorrect assumptions.
- **Do not assume.** Test every hypothesis. When a user reports a bug, reproduce it, read the relevant code path, and verify the fix actually resolves the issue at the system level — not just in a typecheck or unit test.
- **Fix the whole class, not just one instance.** When a bug is found in a query, handler, or component — especially a copy-paste pattern like a SQL column list or a Scan call — search for every other occurrence of the same pattern and fix them all. A single bug report often reveals a systemic issue.
- **Think broadly, not minimally.** When the user agrees to a suggestion (e.g. "add cache-control headers"), don't limit the fix to just the one endpoint that prompted it. Check whether every similar endpoint, handler, or serving path has the same gap and apply the fix consistently.
- **Every answer must explain the full picture:** *why* it was broken, *where* the fault lived, *who* caused it (which component), *when* it triggers (startup, every request, only after certain conditions), and *what* the fix does at a mechanical level.
- Prefer parallel tool calls when independent (one message, many tools) to cut round-trips.
- Read only the slice of a file you need; avoid re-reading whole files.
- Keep edits surgical — match surrounding style, don't reflow untouched code.
- Skip preamble/postamble in responses; the diff speaks for itself.
- Run `make ci` once at the end, not after every edit.

## Git Workflow

- ALWAYS create a new branch before starting work. NEVER commit to main.
- A local pre-commit hook (`.git/hooks/pre-commit`) rejects any commit on `main` or `master`. If the hook is missing, re-create it:

  ```bash
  #!/bin/sh
  branch="$(git symbolic-ref --short HEAD)"
  if [ "$branch" = "main" ] || [ "$branch" = "master" ]; then
    echo "❌ ERROR: Direct commits to $branch are blocked!"
    exit 1
  fi
  ```
- Branch naming: `<type>/<short-description>` (e.g. `feat/project-crud`, `fix/outbox-relay-dedup`, `chore/docker-compose-setup`). Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.
- **Before starting work on a new branch**, bump the version tag: `git tag -a v0.1.<next> -m "v0.1.<next>"`. This ensures the binary reports the correct version during local development and `make container-build` (or `scripts/install-local.sh`) embeds the new tag.
- Commit early and often on your branch. Write clear commit messages in present tense: `Add project CRUD service and data-access layer`. Stage only the files relevant to the commit.
- **Never create a pull request without asking the user for approval first.** Ask, wait for a yes, then proceed.
- Once work is complete and properly tested, ask the user to verify.
- After the user confirms, create a PR and merge. PRs MUST carry the `release` label to kick off the release creation on GitHub. After the merge, delete the branch.
- **Before every PR merge**, ALWAYS ASK THE USER BEFORE MERGING, update the "Last Release Changes" section in `README.md` with a table listing each feature/bug fix in the release. Only the most recent release info should be present — remove older entries. Table format:

  ```
  ### v0.1.NNN (date)

  | Type | Change |
  |---|---|
  | Feature | Short description |
  | Bug fix | Short description |
  | Chore | Short description |
  ```
- Before starting work, always `git pull origin main` to get the latest. Before pushing, `git fetch origin && git rebase origin/main` if the branch has been open for a while.

## Local development loop

During active development, iterate locally without creating PRs or releases:

```bash
make container-rebuild instance=dev   # stop dev container -> build binary+image -> start dev container
# or the individual steps:
make container-build                  # build bin/orchicon + the container image
scripts/container.sh down dev && scripts/container.sh up dev   # restart with the new image
```

**IMPORTANT:** The single container is the only full-stack deployment. Dev and prod are two container instances (`orchicon-cnt-dev` on :8080/:3002, `orchicon-cnt-prod` on :8091/:3003) sharing the compose-era Postgres volumes (`orchicon_postgres-data` / `orchicon-prod_postgres-data`) so data carries over. See DOCUMENTATION.md §Single-Container Deployment.

The binary embeds everything (the single-container runtime configs in `deploy/container/configs/`, frontend dist, migrations) via `go:embed` in `assets.go`. Any change to source files, container configs, or frontend code is included in the next build. No branches, commits, or PRs needed during the day — just build and test.

## Phases

Every task follows this sequence:

1. Read AGENTS.md first.
2. Read any docs or code necessary to perform the work.
3. Create a branch and do the work, committing changes often.
4. Fully test and verify.
5. Before the final commit on your branch, update **both** `README.md` (Last Release Changes section — table format, most recent only) and `UPDATES.md` (new table row) with a one-paragraph summary describing the changes in this PR. This is the commit that will be PR'd and merged.
6. Follow the Git Workflow above.
7. Inform the user every time UPDATES have been made. Show them in a tabled format what was changed and updated.

If architecture or anything referenced in AGENTS.md has changed, update this file for future agent runs.

## Architecture Quick Reference

- **Control plane**: Go, single binary, k8s-style reconcilers
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → Grafana stack (Tempo + Loki + VictoriaMetrics) — separated infra
- **Deployment**: single container (`orchicon container` PID-1 supervisor, `deploy/container/`, GHCR image `ghcr.io/beardedparrott/orchicon`; `scripts/container.sh` manages dev/prod instances preserving the compose-era postgres volumes)
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first, CLI now / IPC later)
- **Frontend**: TypeScript + React + Vite + Connect-ES + React Flow
- **Object storage**: BlobStore abstraction (S3 + local filesystem)
- **Deployment**: Fully local (no cloud) is a supported mode

## Key Invariants (do not violate)

1. No business logic in the frontend — the UI reflects server state.
2. No hand-written API URLs — use the generated Connect-ES client.
3. No mutations outside the transactional outbox pattern.
4. No raw SQL outside the data-access layer.
5. Every `tenant_id` table must have an RLS policy (CI gate enforces).
6. Adapters never touch Postgres or NATS directly — gRPC stream only.
7. No automatic model failover — the human defines the exact model.
8. Recovery is opt-out, not opt-in.
9. Migrations are forward-only.

## Security Standards (applies to every slice)

Every piece of functionality built in this repo must follow these security standards. They are the floor, not the ceiling — review them when adding any new RPC, handler, or frontend form.

### Secrets & credentials

- **No secrets in code or commits.** DSNs, API keys, tokens, and passwords come from the environment (`internal/config`) or a secret store — never hardcoded, never committed. The `.env.example` file documents the variables without containing real values.
- **No secrets in logs.** Never log DSNs, tokens, passwords, or full request payloads that may carry credentials. The slog setup in `cmd/orchicon/main.go` logs structured fields; only log non-sensitive identifiers (tenant id, project id, trace id).
- **Hashed at rest.** API keys are hashed before storage (never plaintext). Passwords are never stored by the control plane (OIDC handles authentication). See DOCUMENTATION.md §Authentication.
- **Dev-only credentials are placeholders.** The container's internal Postgres runs with trust auth on localhost (`orchicon:orchicon` in the default DSN); the `.env.example` documents real variables without values. None of these may appear in a production deployment config.

### Input validation & sanitization

- **Validate at the API boundary.** Every RPC handler validates and sanitizes input before it reaches the data-access layer. See `internal/project/validate.go` for the pattern: trim, bound-check length, regex-validate structured fields (e.g. slug), and reject malformed data with `connect.CodeInvalidArgument`.
- **Parameterized queries only.** All SQL uses pgx parameterized queries (`$1`, `$2`, …). No string interpolation of user input into SQL, ever. The data-access layer (`internal/db`) is the only place SQL lives (invariant #4).
- **JSON fields are validated.** JSON-typed columns (e.g. `goals`) must be parsed/validated as valid JSON before storage. Reject malformed JSON at the handler, not the database.
- **Size bounds on all inputs.** Every text input has a max length enforced at the handler to prevent memory-exhaustion abuse.
- **Slugs and identifiers are regex-validated.** Slugs match `^[a-z0-9]+(?:-[a-z0-9]+)*$`; IDs are ULIDs generated server-side, never accepted from the client on create.

### Tenant isolation

- **Every request is tenant-scoped.** The middleware resolves the tenant and the data-access layer sets `app.tenant_id` per transaction. RLS is the backstop — even a buggy query cannot leak cross-tenant data.
- **No cross-tenant queries.** The data-access layer injects `tenant_id` into every `WHERE` and `INSERT`. A query without a tenant scope is a bug, not an optimization.

### Frontend

- **The browser never stores long-lived secrets.** Access tokens live in memory; refresh tokens in HttpOnly secure cookies. API keys are for headless/CI clients only.
- **Client-side validation is UX, not security.** Zod schemas in forms improve the user experience but every rule is re-validated server-side. Never trust client-side validation as the security gate.
- **No business logic in the frontend** (invariant #1). The UI does not make policy, scheduling, or recovery decisions.

## Tooling hints

- When you need library docs (Connect-ES, Atlas, TanStack Router, pgx, NATS, Grafana/Tempo/Loki), use `context7` tools before guessing.
- If unsure how to use a library or pattern, use `gh_grep` to search real GitHub usage examples.
- LSP servers (gopls, typescript, eslint, yaml-ls) are enabled — diagnostics surface in the edit loop. Treat them as fast feedback; `make ci` is the authoritative gate.
- Playwright MCP is configured in `opencode.jsonc` for browser testing. **Playwright is installed** — `npx playwright install chrome` has been run, so Chrome is available for browser verification. Use Chrome and/or Playwright for frontend verification. NEVER use Firefox — the developer uses Firefox and testing needs a separate browser.
- **NEVER run a foreground server (`orchicon serve`, `orchicon container`, or `container.sh up`) from a shell tool** — a foreground process keeps the caller's stdout/stderr pipe open and never returns, which hangs the agent session until the user kills it. **Use the built-in detach mode for the headless plane** — `orchicon serve --detach` forks the server, writes the PID file, and returns immediately (stop with `orchicon serve --stop`; check with `serve --status`; logs in `.dev/logs/orchicon.log`). The `scripts/container.sh` commands are for the user to run; if you need to test the container, launch it with `docker run -d` and poll `/healthz`.
  - This requires the stack already up (the container / `docker run` manages it) and migrations applied (the server also runs migrations on boot).
  - `serve --detach` writes the PID file and stops cleanly with `serve --stop`; use `fuser -k 8080/tcp` to stop a test server that lacks a PID file.
  - **Never use `pkill -f` with a pattern that could match the shell command itself** (e.g. `pkill -f "orchicon"` from within a bash `-c` that contains that string) — it kills your own shell. Kill by exact PID or by port (`fuser -k 8080/tcp`).
- The `site/` landing page and `README.md` document the `orchicon` commands and installed files. Keep both in sync when commands, flags, or install paths change. The CloudFlare Pages build copies `scripts/install.{sh,ps1}` to the deployed site.
- **UI consistency**: Every list page must follow the same visual pattern: search input, filter/sort dropdowns, select-all checkbox, per-item checkboxes, a selection count label, and a bulk action button (delete / approve / reject etc.) that appears when ≥1 item is selected. Do not add a page with a different interaction model — new list pages must replicate this pattern exactly. The Approvals, Work Items, Executions, Workers, and Policies pages all follow this pattern; use them as reference.

## Verification

> **Compilation passing is not the same as working.**
> Agents must verify runtime behavior, not just `go build` / `tsc`.

Before marking a phase or task as complete, verify the following at minimum (adapt to what the change touches):

1. **`make ci` passes end-to-end** — buf lint, codegen, go vet/test, RLS gate. This is the authoritative CI gate.
2. **Container instance starts healthy** — `make container-build && scripts/container.sh up dev`, then `scripts/container.sh status` shows the dev instance `running (healthy)`. When the change touches Docker or infrastructure:
   - Verify the full stack boots: `curl http://localhost:8080/healthz` returns `{"status":"ok"}`, Grafana answers on `:3002/api/health`, and telemetry flows (traces in Tempo, logs in Loki, metrics in VictoriaMetrics — query the backends from inside the container).
   - Run `scripts/container.sh down dev && scripts/container.sh up dev` from a clean slate to verify the full startup sequence works end-to-end.
3. **Migrations apply cleanly** — on a fresh container data volume (`ORCHICON_PG_VOLUME=fresh`); `make rls-check` passes.
4. **Control plane boots and serves** — `make build && make run`, then `curl http://localhost:8080/healthz` returns `{"status":"ok"}`. Time this command — if the telemetry stack is starting, the boot should still be <2s (not 20s+). Check the control plane logs for `"otel pipeline initialized"` — if it appears before the 2s mark, the non-blocking OTel dial is working.
5. **Frontend renders** — `make fe-dev` (or `npx vite`), then `curl http://localhost:5173/` returns HTTP 200 with the app shell.
6. **Runtime calls are real, not simulated** — end-to-end verification that exercises adapter dispatch MUST call the real `opencode` runtime with a **free model** (e.g. `opencode/deepseek-v4-flash-free`), never the simulation-mode fallback. Simulation mode is a development aid for the offline case only; it must not be used to "verify" dispatch, recovery, or any flow that depends on adapter telemetry. If `opencode` is absent from PATH, fix the environment (install it) — do not fall back to simulation and claim the slice works. Seed workers / executions used for verification must pin a free model in `model_ref` so verification is reproducible at no cost.
   - **Stall + wall-clock guardrails**: the opencode adapter bridge runs a per-execution progress monitor that detects stuck-looping and triggers recovery (opt-out, idempotent). Three stall signals, configurable via env: `ORCHICON_STALL_NO_PROGRESS_WINDOW` (default 300s — no step_finish/token progress), `ORCHICON_STALL_NO_FILE_DIFF_WINDOW` (default 15m — no file modifications), `ORCHICON_STALL_REPETITION_COUNT` (default 5 — same tool_call signature repeated within `ORCHICON_STALL_REPETITION_WINDOW`, default 300s). The worker's `budget_overrides.wall_clock_seconds` (default 3600) is the hard per-execution timeout backstop (context deadline → subprocess kill → recovery). Verification that exercises stall/timeout paths must use tight env windows + a free model.

### Docker / infrastructure changes

When a change modifies the single-container runtime (`deploy/container/Dockerfile`, `deploy/container/configs/`), the telemetry setup in `internal/telemetry/`, or `cmd/orchicon/container.go`:

- **Image build + run**: `make container-build`, then run a throwaway instance on offset ports (`docker run --rm -p 18080:8080 -e ORCHICON_GRAFANA_PUBLIC_URL=http://localhost:18080/grafana -v /tmp/cnt-test:/var/lib/orchicon orchicon:local`) and verify `curl localhost:18080/healthz` returns `{"status":"ok"}`, plus telemetry flows (traces in Tempo, logs in Loki, metrics in VictoriaMetrics — query from inside the container).
- **Control plane boot speed**: after `make build`, time how long the container's `/healthz` takes to answer. With the non-blocking OTel gRPC dial (`grpc.NewClient`), boot is <2s even while the collector is still warming. Check the control-plane log for `"otel pipeline initialized"` at process start.
- **Data preservation**: if the instance reuses a compose-era postgres volume, verify the container's postgres runs as the volume's owner uid (70 for the alpine volumes) — see `scripts/container.sh`.
- **Fresh-boot path**: `scripts/container.sh down dev && scripts/container.sh up dev` with `ORCHICON_PG_VOLUME=fresh` to verify initdb + migrations from an empty data dir.

If the change adds a new API RPC, also verify the Connect endpoint responds (e.g. via `curl` or a frontend smoke test). If it adds a new table, verify the RLS gate still passes after migration.

**Do not claim "done" without having run the thing.** State what was verified and what was not in the commit message or PR description.

**Testing preference**: The canonical test is to build a release binary (`make build`) and the container image, then run it like a user would. Do not rely on `go run` / `npx vite` for final verification unless the change is frontend-only and cannot be tested from a release bundle. If the change touches both layers, cut a release artifact and verify end-to-end from there.

## Dev Control Script

`scripts/container.sh` is the one-command deployment controller for the single-container instances. It manages the full stack — Postgres, NATS, the Grafana telemetry plane (Tempo, Loki, VictoriaMetrics, collector, Grafana), and the control plane — as `orchicon-cnt-dev` (:8080/:3002) and `orchicon-cnt-prod` (:8091/:3003):

```
scripts/container.sh build              # build bin/orchicon + the container image
scripts/container.sh up dev|prod        # start an instance (dev data preserved via the compose-era volume)
scripts/container.sh rebuild dev|prod   # down -> build -> up
scripts/container.sh down dev|prod      # stop + remove the instance container (data volume kept)
scripts/container.sh status [dev|prod]  # show instance state + health
scripts/container.sh logs dev|prod      # tail the supervisor log
scripts/container.sh ps                 # list both instances
```

Or via Make: `make container-build`, `make container-up`, `make container-down`, `make container-status`, `make container-logs`, `make container-rebuild instance=dev|prod`.

Data volumes live in Docker named volumes (`orchicon-cnt-dev-data` / `orchicon-cnt-prod-data` for the telemetry/state, plus the compose-era Postgres volumes for the databases).

When a phase adds a new runtime component — a reconciler, an adapter process, the recovery engine, the policy engine, a webhook dispatcher, etc. — make sure it is included in the container image and the supervisor (`cmd/orchicon/container.go`) brings it up in the right order.

## Install Scripts

`scripts/install.sh` (Linux/macOS) and `scripts/install.ps1` (Windows) are the one-line installers published at `orchicon.dev`:

```
curl -fsSL https://orchicon.dev/install | bash          # Linux/macOS
irm https://orchicon.dev/install.ps1 | iex               # Windows
```

They download the latest release binary from GitHub Releases, install it to `~/.local/bin` (or a chosen dir), and verify the install. The release workflow (`.github/workflows/release.yml`) builds binaries for linux/darwin/windows × amd64/arm64 on tag push and attaches them to the GitHub Release.

When a phase changes what ships in the binary — a new subcommand, a new dependency the binary needs at runtime, a new asset (e.g. the frontend bundle, adapter binaries, Rego policy files), or a new platform/architecture target — update the install scripts and the release workflow so the installer stays correct. Specifically:

- **`scripts/install.sh`** — update if the download asset name changes, new files need to be downloaded alongside the binary, or new post-install steps are required (e.g. installing an adapter).
- **`scripts/install.ps1`** — mirror any changes from `install.sh` for Windows. Both scripts must stay in sync.
- **`.github/workflows/release.yml`** — update the build matrix if a new OS/arch is added, add build steps if the binary now needs the frontend embedded, and verify the asset naming matches what the install scripts download. The workflow also builds + pushes the single-container image to `ghcr.io/beardedparrott/orchicon` — when `deploy/container/` changes (Dockerfile, embedded configs, new runtime binaries), verify the image build still succeeds.
- **`deploy/container/Dockerfile` + `deploy/container/configs/`** — when the container runtime changes (new bundled process, changed ports, new config), update the Dockerfile / embedded configs and verify the image (see §Verification).
- **README.md** — update the Installation section if the commands or prerequisites change.

Verify by running the installer against a draft release at minimum (`bash scripts/install.sh --version vX.Y.Z --dry-run` on each target platform, or `--uninstall` to test cleanup).

## Documentation

The single comprehensive documentation file is
[`DOCUMENTATION.md`](./DOCUMENTATION.md) at the project root. It
replaces the old `docs/` directory and covers all subsystems:
architecture, project structure, installation, development,
deployment, troubleshooting, and more. Read it before touching any
subsystem you are unfamiliar with.

**Keep DOCUMENTATION.md in sync.** Whenever you add, delete, or change
a main feature or architectural component — a new service, RPC, proto,
reconciler, frontend route, database table, adapter, or significant
policy — update DOCUMENTATION.md to reflect it. If the change is
entirely internal refactoring with no user-visible or architectural
impact, you may skip the update, but err on the side of updating.

## E2E Testing & Cleanup

## E2E Testing & Data Preservation

### Backup before any agent session that modifies data

Before making any changes (migrations, edits, test data creation), back up the database:

```bash
docker exec orchicon-postgres pg_dump -U orchicon -d orchicon > /tmp/orchicon-backup-$(date +%Y%m%d-%H%M%S).sql
```

This ensures you can always restore if something goes wrong. To restore:

```bash
cat /tmp/orchicon-backup-*.sql | docker exec -i orchicon-postgres psql -U orchicon -d orchicon
```

### Cleanup notes

- **Stale work items**: The TaskReconciler scans for `ready` items. If you need to cancel stray items after E2E, update them by ID via the API or UI — never issue bulk SQL deletes.
- **FK constraints**: Every `REFERENCES` column must carry `ON DELETE SET NULL` or `ON DELETE CASCADE`. The default (`NO ACTION`) blocks deletes and causes silent UI failures.
- **Migration idempotency**: Every `ALTER TABLE ADD COLUMN` must use `ADD COLUMN IF NOT EXISTS`. Without it, re-running migrations on an existing database errors with "column already exists". This is mandatory for all new migrations (see DOCUMENTATION.md §Database Migrations).
- **Seed data is managed in Go code** (`internal/db/seed_workers.go`), not SQL migrations. Worker changes go in Go, not in new `.sql` files.

## Ask Orchicon — keep it in sync

> Every time you add, change, or remove a first-class entity, RPC, or user-facing capability, update the Ask Orchicon agent to match.

The "Ask Orchicon" conversational agent (`internal/askorchicon/`) has a `ToolRegistry` in `tools.go` that defines every action the agent can perform. When you:

- **Add a new entity** (table, proto, service) — add a tool for its CRUD in the appropriate `tool_*.go` file and register it in `allTools()` in `tools.go`
- **Add a new RPC** — add a tool that calls the DB layer directly (same as existing tools)
- **Change the data-access layer** — update any tools that call the affected `db.*` functions
- **Change the agent's identity** — update `agent.go` (the hardcoded root system prompt) or the DB-stored config defaults in `service.go`'s `defaultAgentConfigProto()`
- **Add a new interactive feature** — add a new `tool_*.go` file following the existing patterns and register it

The three touchpoints to check:

1. `internal/askorchicon/tools.go` — `allTools()` list (add/remove tool definitions)
2. `internal/askorchicon/tool_*.go` — tool implementations (one file per domain)
3. `internal/askorchicon/agent.go` — `BuildSystemPrompt()` (update if the agent needs to know about the new feature)
4. `internal/askorchicon/service.go` — `defaultAgentConfigProto()` (update Skills/Behavior defaults if the new feature changes what Orchicon can do or how it should behave)
5. `db/migrations/` — if the new feature adds a table, ensure it has RLS (the CI gate enforces this, but new Ask Orchicon conversation/message tables must too)

The frontend route (`/ask-orchicon`) and its components generally don't need changes unless the chat UI itself is being modified — the agent adapts to new tools automatically through the system prompt.

## Things you need to know

- **Landing page + install deploy**: `site/` holds the static landing page deployed to CloudFlare Pages (`orchicon-site`). The build step copies `scripts/install.{sh,ps1}` into the deployed bundle so the one-liner install commands work. `site/install` and `site/install.ps1` are git-ignored build artifacts. Full setup in `CLOUDFLARE_SETUP.md`.
- **Connect-ES codegen** is pinned to local v1 npm plugins (`protoc-gen-es` / `protoc-gen-connect-es`) matching the v1 runtime. `make gen` prepends `frontend/node_modules/.bin` to PATH. See PR #1 notes before bumping to v2.
- **Atlas RLS** policies are hand-appended SQL (the free tier does not diff `policy` blocks). After hand-editing a migration, run `make migrate-hash`. Future diffs won't drop RLS.
- **`orchicon container`** subcommand embeds the single-container runtime configs (`deploy/container/configs/`) and runs the whole stack as PID-1 (§Architecture Quick Reference → Deployment). `orchicon serve` runs the plane headless (migrations + embedded frontend); `serve --detach`/`--stop` manage a background instance. The OTel pipeline uses non-blocking `grpc.NewClient` so boot is <2s even without a healthy collector. NATS subscriber fans out events to streaming RPCs. Reconciler framework uses `pg_try_advisory_lock` for per-kind leadership.
- **Worker lifecycle**: draft → published → deprecated → retired. Published versions are immutable. WorkItem hierarchy: Epic → Feature → Task → Subtask (max 4 levels). Dependency edges form a DAG; cycle detection uses recursive CTE. Edit locks have automatic TTL expiry.
- **TaskReconciler** is the only component that creates WorkerExecutions. It polls ready tasks, resolves dependencies, selects a worker+adapter, and dispatches. The OpenCode adapter bridge wraps the `opencode` CLI as a subprocess. Simulation mode is opt-in only (`ORCHICON_SIMULATE_ADAPTER=1`) — real runtime calls with a free model are required for verification.
- **Workflows** are the top-level reconcilable object. The WorkflowReconciler progresses step DAGs, evaluating gates at transitions. Task steps create WorkItems and hand off to TaskReconciler. Frontend has a full drag-and-drop React Flow editor with undo/redo, cycle detection, palette with Workers, Work Items, Policies, and Step primitives.
- **Recovery** follows a default 6-step workflow (capture → summarize → preserve → review → plan → resume) with bounded auto-relax (25% / 150%) and L1→L2→L3 escalation. TaskReconciler triggers recovery on execution failure (opt-out, idempotent). Recovery is also available as typed work item kinds (stop, summarize_restart, human_escalation, retry_n).
- **Policy Engine** uses OPA v1 with bundles loaded from Postgres. Evaluation is narrowest-scope-first with first-definitive-decision-wins; default is allow (fail-open). Rego traces are captured for `ExplainDecision`.
- **Auth**: OIDC-based with built-in dev IdP for local verification (HS256). Production uses authorization-code flow. API keys are SHA-256 hashed with least-privilege scopes. RBAC is a per-RPC Connect interceptor. Frontend stores access tokens in memory; refresh tokens in HttpOnly cookies.
- **Webhooks**: NATS consumer dispatches events to matching subscriptions with HMAC signing, exponential backoff, and dead-letter queue (replayable). Runs in the control-plane binary.
- **BlobStore** has two backends: local filesystem (content-addressed, atomic writes, path-traversal-safe) and S3-compatible (AWS SDK v2).
- **Markdown** is supported on all prompt-affecting fields: work item description/AC, worker system_prompt, execution output/error/reasoning, composite prompt, project goals, and recovery narrative fields. The frontend uses `react-markdown` + `remark-gfm` via a reusable `<Markdown>` component with theme-aware styling. Server-side extraction of the `composite` field from JSONB prompt_context ensures the API delivers plain markdown text.

## UPDATES.md

> Read this before starting any work. All changes must be recorded in `UPDATES.md` in the same table format as the existing entries.

Phase progress and per-PR changes are tracked in `UPDATES.md` (created on the first run of the new AGENTS.md structure). Before commencing any work, always read `UPDATES.md` first to understand the current state and what has already been shipped.

When a PR is merged, add a new row to the table in `UPDATES.md` describing what was done. The table format is:

```
| # (next number) | Short description | done | Brief summary of what was shipped |
```

If no existing phase fits, group related PRs under a descriptive phase name (e.g. "Markdown rendering", "Prompt card fix"). Keep entries concise — one line per PR, linking the PR number where useful.
