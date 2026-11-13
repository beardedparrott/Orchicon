# developer.md — Orchicon Development Guide

Read on demand by agents working directly with the human developer (and by human contributors). Orchicon **worker** runs read their own system prompt instead and should not load this file — see `AGENTS.md`.

- **Repo**: https://github.com/beardedparrott/Orchicon.git (PUBLIC)
- **Language**: Go (control plane) + TypeScript (frontend)
- **Documentation**: there is no `DOCUMENTATION.md` and no numbered `docs/NN_*.md` spec series — both were retired. The current set is `README.md`, `USERGUIDE.md`, `ARCHITECTURE.md`, this file, and the surviving `docs/*.md` design notes. `ARCHITECTURE.md` is the authority on structure and invariants; `USERGUIDE.md` on product behaviour.
- **Dead citations in source comments**: ~157 references to the retired specs survive in hand-written Go/TS/`.proto` (`docs/07_API_Specification.md`, `docs/02_Domain_Model.md`, `docs/05 §4` and friends). **They are dead pointers — there is nothing to go read.**
  - **Do not "fix" one by repointing it at `ARCHITECTURE.md` unless the target content is actually there.** A citation that resolves to the wrong place reads as authoritative, which is worse than one that plainly points nowhere.
  - **The code is the authority** for what these citations describe. Read the enforcement point and cite THAT in a comment, e.g. `internal/db/worker.go UpdateWorker is the gate` instead of `docs/05 §4`.
  - Removing the remainder is deliberately deferred to its own reviewed work item: the obvious scripted approach is unsafe, because an early attempt rewrote an embedded shell script in `internal/guard/guard.go` and deleted `()` from `db.NewID()` by matching a function call — while the test suite stayed green.

- **Everything ships as one binary**: the frontend dist, the host service configs and the SQL migrations are embedded via `go:embed`. No branch/commit/PR needed during active development — just build and test.

## Two contexts, two approval flows

A **worker running inside Orchicon** operates autonomously: it PRs into `develop`, merges on approval, and deletes the branch without asking. A **session working directly with a human** must ALWAYS ask the human before creating a PR and again before merging — the human owns the review. When in doubt, treat yourself as a human-facing session and ask.

## Working norms

- **Do not assume; test every hypothesis.** When a user reports a bug, reproduce it, read the relevant code path, and verify the fix resolves it at the system level — not just in a typecheck or unit test.
- **Efficiency is not an excuse for shortcuts.** Never patch a symptom without first identifying the root cause; verbose investigation is cheaper than a second pass that has to undo incorrect assumptions.
- **Every answer must explain the full picture:** *why* it was broken, *where* the fault lived, *who* caused it (which component), *when* it triggers, and *what* the fix does at a mechanical level.
- **Fix the whole class, not just one instance.** A bug in a query, handler, or component — especially a copy-paste pattern like a SQL column list or a Scan call — is often systemic; search for every other occurrence and fix them all.
- **Think broadly, not minimally.** If a suggestion applies to one endpoint (e.g. "add cache-control headers"), check whether every similar path has the same gap.
- **Prefer parallel tool calls** when independent (one message, many tools). Read only the slice of a file you need; keep edits surgical; skip preamble/postamble — the diff speaks for itself. Run `make ci` once at the end, not after every edit.

## Git workflow

- `develop` is the integration branch; `main` is release-only and managed by the human. Branch off `develop` and PR into `develop` — never `main`.
- ALWAYS create a new branch before starting work. Never commit directly to `main` or `develop`.
- Branch naming: `<type>/<short-description>` (`feat/`, `fix/`, `chore/`, `refactor/`, `docs/`, `test/`).
- The version tag is bumped automatically on every merge to `develop` (`.github/workflows/develop-bump.yml`), so local builds report the current version. `git fetch --tags` before rebuilding to pick up the latest tag — `git pull` does not fetch tags.
- PRs must target `develop` explicitly (`gh pr create --base develop`) — `main` is the default branch. PRs must NOT carry the release labels (`release`, `release-minor`, `release-major`): those belong only on the human's `develop` → `main` release PR, where they select the next patch / minor / major bump, computed by `.github/workflows/auto-release.yml` from the highest existing tag.

### `[NoDevBump]` — ride the current version instead of minting a new one

Put **`[NoDevBump]`** in a PR's **title** when its merge should share the current version rather than consume a new one. Two cases:

- **documentation-only PRs** — the last merge before a release
- **post-release fixes** — work that will ship with the next release rather than as its own

**Why this exists.** `develop-bump` tags every merge and `auto-release` computes the release number from the same pool, so intermediate merges consume release numbers before a release exists. `v0.6.1`, `v0.6.3` and `v0.6.4` were all minted by fix PRs, were never published, and pushed the real release onto `v0.6.4` — a number nobody chose.

**Why it rides rather than skips.** `next_tag` refuses to reuse a tag that sits on a different commit and bumps past it, so merely *not* tagging a merge makes the **next** one climb anyway. One opt-out therefore has to cover every merge between releases, or the counter keeps moving. The marker makes the merge **share** the current tag instead: two commits, one version, no number consumed.

That is safe because the tag means *"which version is this tree"*, not *"which commit"*. Local builds report it, `auto-release` computes from it, and release builds get the version passed explicitly through `-ldflags` rather than read from tag placement.

**Put the marker at the FRONT of the title**, e.g. `[NoDevBump] docs: v0.6.1 release notes`.

Two reasons, both measured:

1. The title is what reaches the commit either way a PR lands here — a squash merge uses it as the subject, a merge commit puts it in the body. A marker only in the *body* of a squash-merged PR is lost.
2. **GitHub truncates the squash subject at 72 characters** before appending ` (#N)`. A trailing marker on a long title is silently cut: a PR titled `docs(release): v0.6.1 highlights — install and desktop port improvements [NoDevBump]` merged as `… improvements (#967)` — the marker was at character 73 and never reached the commit, so the merge was tagged normally even though the marker *was* in the title. A leading marker cannot be truncated.

`TestDevelopBumpHonoursNoDevBumpMarker` guards this: it fails if the marker disappears, if the match drops to subject-only, if the ride step is removed or stops moving the tag, or if the ride and bump steps stop being mutually exclusive.
- Commit early and often on your branch. Write clear present-tense messages (`Add project CRUD service and data-access layer`). Stage only the files relevant to the commit.
- Before starting work, `git pull origin develop`. Before pushing, `git fetch origin && git rebase origin/develop` if the branch has been open for a while.
- Before opening a PR: fetch + rebase onto `origin/develop` if stale. Add the release narrative to `RELEASE_HIGHLIGHTS.md` under the current version section (that is what becomes the GitHub release body). Do NOT touch `README.md`'s "Last Release Changes" — it is generated by `scripts/gen-release-notes.sh --sync-readme`.
- Before merging, ask the user again (PR-creation approval ≠ merge approval). After merging, delete the branch.

## Local development loop

The plane and its services (Postgres, NATS, tempo, loki, VictoriaMetrics, the OTel collector) are **HOST processes** owned by `orchicon serve`. No image is built for the plane and no container is created for it; a container exists only as a per-project **workflow runtime container**, whose images come from `make runtime-build` (podman).

```bash
make instance-rebuild instance=dev    # stop dev instance -> build binaries -> start dev instance
make instance-status                  # is this instance's plane and services up, and healthy?
make instance-logs                    # tail this instance's plane log
make instance-ps                      # list the orchicon instances on this host
```

`instance-rebuild` always forces a fresh frontend build (`force-fe=1`), so a rebuilt instance is guaranteed to reflect current source — that closes the recurring "my frontend fix went invisible" trap where a stale `frontend/dist` silently shipped the previous UI. Or run the steps individually:

```bash
make build          # bin/orchicon with the frontend + configs embedded
make run            # run the control plane from source (starts this instance's services first)
```

The dev and prod instances are separate host instances sharing nothing by default. `scripts/instance-ports.sh` allocates their ports. Dev is the default instance for hands-on development; prod runs the real pipeline. Workers never touch either — a `:orchicon-dev` runtime image boots its own disposable sandbox plane inside the runtime container.

**NEVER run a foreground server from a shell tool — it never returns.** Use `orchicon serve --detach` (stop with `--stop`, logs under the instance's log directory) or the `make instance-*` targets. Never `pkill -f` a pattern that could match your own shell command; kill by PID or port (`fuser -k 8080/tcp`).

**Disk hygiene:** the Go build cache grows to tens of GB (`~/.cache/go-build`) and repeated runtime-image builds leave dangling images. Reclaim with `make clean` (go cache + `bin/`) and `make clean-docker` (dangling images + stopped containers + unused volumes). The serve/detached log auto-rotates and is pruned via Settings → Defaults → Log management — never `rm` a live log by hand. Run `make clean` at the end of a heavy session, and check `make cache-check` before starting work.

## Phases

Every task follows this sequence:

1. Read `AGENTS.md` for the documentation map, then the relevant `ARCHITECTURE.md` / `USERGUIDE.md` sections before touching something unfamiliar.
2. Create a branch and do the work, committing often.
3. Fully test and verify (see Verification).
4. Before the final commit, add the narrative to `RELEASE_HIGHLIGHTS.md`. Do NOT touch `README.md`'s "Last Release Changes" manually — `scripts/gen-release-notes.sh --sync-readme` keeps it in sync. This is the commit that will be PR'd and merged into `develop`.
5. Follow the Git workflow above.
6. Tell the user what changed, in a compact table.

If architecture or anything referenced in the docs has changed, update the relevant `.md` for future runs. Do not edit `AGENTS.md` yourself — it is the human-maintained router; flag any proposed change to the human.

## Architecture quick reference

- Control plane: Go, single binary, k8s-style reconcilers. API: Protobuf + Connect (gRPC + REST + streaming from one schema). DB: PostgreSQL with RLS + transactional outbox. Event bus: NATS JetStream. Telemetry: OpenTelemetry → tempo + loki + VictoriaMetrics, read by the plane's native Telemetry panes. Deployment: host processes (`orchicon install` provisions and starts them; `orchicon serve` runs the plane).
- **Three clients over one API**: the web app (`frontend/src/**`, React + Vite + Connect-ES + React Flow), the TUI (`internal/tui/**`, `cmd/orch`), and the desktop app (`cmd/orchicon-desktop`, a Wails shell wrapping that same SPA).
- **The model_ref's first segment selects the adapter** (`adapter/provider/model`), resolved to a registered bridge by the dispatcher (`internal/adapter`). The registered set is not fixed — currently `claude`, `opencode`, `orchicon`. External CLIs are optional and mounted, never baked: the operator installs one on the host and it is bind-mounted into the containers only when a run's boot profile demands it.
- Workflow runtime containers: per-workflow execution on a warm pool, leased exclusively per run, reset to pristine on release. `:orchicon-dev` images boot a disposable in-container sandbox plane so workers get a consistent environment and the sandbox `orchicon_*` tools against sandbox data — never the host plane's.
- Recovery follows a default 6-step workflow (capture → summarize → preserve → review → plan → resume) with bounded auto-relax and L1→L2→L3 escalation. Policy engine uses OPA (Rego) with bundles from Postgres. Auth is OIDC with API keys (SHA-256 hashed) + RBAC. Webhooks via NATS consumer with HMAC signing and a replayable dead-letter queue.

## Key invariants (do not violate)

1. No business logic in the frontend — the UI reflects server state.
2. No hand-written API URLs — use the generated Connect-ES client.
3. No mutations outside the transactional outbox pattern.
4. No raw SQL outside the data-access layer.
5. Every `tenant_id` table must have an RLS policy (CI gate enforces).
6. Adapters never touch Postgres or NATS directly — gRPC stream only.
7. No automatic model failover — the human picks the exact model.
8. Recovery is opt-out, not opt-in.
9. Migrations are forward-only.
10. **Windows is a first-class NATIVE target** — orchicon.exe runs the plane as host processes on Windows, with no WSL2 and no Docker Desktop. `scripts/install.ps1` installs the native build into `%LOCALAPPDATA%\Programs\Orchicon` and refuses to run elevated (Postgres will not run as Administrator). The same holds on macOS and Linux; the TUI and desktop app ship for all three.
11. **Engine behaviour is never keyed on a tenant's identifiers** — no step id, project id, title, or worker id in a reconciler branch. Recorded policy is config; anything else is DERIVED from structure (the graph, the kind, the run's own rows). Two live loop decisions were once rescued and one stranded by a guard comparing `loop_branch == "step-devops-pr"` and a hardcoded success-branch list, which broke silently for every workflow that named its end step differently.
12. **An omitted budget gate means NO LIMIT.** There are no built-in ceilings: a `tokens`, `cost_usd`, `wall_clock_seconds` or `tool_call_count` gate exists only because an operator set one, and an explicit `0` also disables it.

## Security standards (floor, not ceiling)

- **Secrets**: no secrets in code/commits/logs; env vars or the secret store only. API keys hashed at rest; human passwords stored only by the embedded identity provider (argon2id PHC strings, bcrypt accepted on verify via prefix dispatch, `internal/auth/op`), never by control-plane business logic; external auth flows through OIDC. Dev-only credentials are placeholders.
- **Input validation**: validate at the API boundary (see `internal/project/validate.go` for the pattern). Parameterized queries only. JSON fields validated as JSON before storage. Size bounds on all inputs. Slugs regex-validated (`^[a-z0-9]+(?:-[a-z0-9]+)*$`); IDs are server-generated ULIDs.
- **Tenant isolation**: every request tenant-scoped; RLS is the backstop. The data-access layer injects `tenant_id` into every WHERE/INSERT.
- **Frontend**: the browser never stores long-lived secrets (access tokens in memory, refresh tokens in HttpOnly cookies). Client-side validation is UX, not the security gate.

## Tooling hints

- LSP servers (gopls, typescript, eslint) are enabled; `make ci` is the authoritative gate.
- Playwright is configured in `opencode.jsonc` (Chrome installed). NEVER use Firefox for verification.
- The Orchicon MCP is registered in `opencode.jsonc` as the `orchicon` server against the dev instance. The prod entry is registered but disabled.
- **Batch your file access.** When `batch_read` / `batch_grep` / `batch_write` are available, prefer them to repeated single-file calls — each call re-sends the conversation, so fewer, larger calls are dramatically cheaper.
- UI consistency: every list page follows the same pattern — search input, filter/sort dropdowns, select-all checkbox, per-item checkboxes, selection count, bulk action button.

## Verification

Compilation passing is not working. Verify runtime behaviour. At minimum: `make ci` passes end-to-end; the instance starts healthy (`make instance-status`); migrations apply cleanly on a fresh data volume; the control plane serves (`curl localhost:8080/healthz`); the frontend renders from the bytes actually served; and runtime calls use a REAL adapter with a model you are permitted to use. **Do not claim "done" without having run the thing.** The canonical test is a release binary run the way a user would.

Site and install-script changes: CloudFlare Pages is pinned to `main`, so staging a site change on `develop` does NOT publish it. Verify with `scripts/build-site.sh` locally, not by curling orchicon.dev.

Interactive TUI changes (`internal/tui/**`, `cmd/orch`) need the real-PTY gate — `make tui-pty-gate`. Rendered-string assertions are supporting evidence only; see `docs/tui-pty-verification-gate.md` for why.

## Install scripts & release

`scripts/install.sh` / `scripts/install.ps1` are the one-line installers published at orchicon.dev, each a single source of truth for its platform. Windows installs the **native** build and runs the plane as host processes — no WSL2, no Docker Desktop. `site/` and the install scripts are staged on `develop` and only reach orchicon.dev when the human merges `develop` → `main` (CloudFlare Pages is pinned to `main`). The release workflow builds binaries for linux/darwin/windows × amd64/arm64 on tag push, attaches them plus the desktop packages to the GitHub Release, and publishes the runtime images. Releases are capped at 5 (`prune-releases.yml`); the release body is the curated narrative from `RELEASE_HIGHLIGHTS.md` rendered by `scripts/gen-release-notes.sh`, and README's "Last Release Changes" gets a compact digest of the same story via `--sync-readme`. Release versioning: the develop→main PR's label picks the bump — `release` (next patch), `release-minor` (next minor), `release-major` (next major). Verify an installer against a draft release before trusting it (`bash scripts/install.sh --version vX.Y.Z --dry-run`, or `--uninstall` to test cleanup).

## E2E & data preservation

- Back every `REFERENCES` column with `ON DELETE SET NULL` or `ON DELETE CASCADE`.
- Back up before any session that modifies real data: `pg_dump -U orchicon -d orchicon > /tmp/orchicon-backup-<ts>.sql`. Postgres runs as a host service of the instance, not as a separate container.
- Acceptance procedures live in `docs/*-e2e-acceptance.md`, each paired with a `scripts/*-e2e.sh`: `no-engine-e2e.sh` (the engine-free path), `plane-control-e2e.sh` (plane RPC control), plus the runtime and desktop paths. They are deliberately outside `make ci` — they bind real ports and download Postgres + NATS.
