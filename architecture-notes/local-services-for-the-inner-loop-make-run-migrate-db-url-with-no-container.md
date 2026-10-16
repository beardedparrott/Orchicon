# Local services for the inner loop (make run / migrate / DB_URL with no container)

**Work item:** `local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container`
**Step:** 1 — Principal Software Architect (plan only; no product code written)
**Branch:** `local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container-49d1jjyn8j31gf52` (off `develop` @ `8865d24fb3`)

## Context (read from `develop` this session — the premise has CHANGED)

The task was written against a tree in which the host-services supervisor did not exist yet.
It now does: **`develop` has landed the de-containerized service layer**, so this work item is
NOT "build a host supervisor" — it is the **inner-loop wiring on top of primitives that already
ship.** Building a second supervisor would be the parallel-implementation defect the repo's own
comments warn against.

What `develop` already provides (all verified by reading the files):

| Primitive | Path | What it gives us |
|---|---|---|
| Service residency (host vs container) | `cmd/orchicon/residency.go:35` `serviceResidencyEnv`; `servicePortsFor` `:118` | `ORCHICON_SERVICE_RESIDENCY=host` runs postgres/nats/telemetry as **processes of the invoking user**, no `setpriv`, no container |
| Residency-aware supervisor | `cmd/orchicon/container.go` `runContainer` `:71`, `:86` | `servicesOnly := servicesOnlyFromEnv() \|\| residency == residencyHost` — a host-residency boot has **no plane child**, i.e. the dev-equivalent of `ORCHICON_CONTAINER_SERVICES_ONLY=1` |
| Detect-else-download binaries | `cmd/orchicon/binaries.go:1555` `acquireInfraBinaries`, `:576` `loadInfraBinaries`, record at `<DataDir>/infra-binaries.json` | A host with **no Postgres installed** still starts one (pinned Postgres 16.4.0 + nats 2.10.29, SHA256-verified) |
| Start/stop host services, detached + PID | `cmd/orchicon/install_host.go:538` `startHostServices`, `:588` `stopHostServices`, `:482` `hostServicesPIDFile`, `:494` `hostServicesAlive`, `:393` `servicesReady` | **Idempotent** start (returns early when `servicesReady`), recorded PID, bounded stop |
| Per-instance host data dir | `cmd/orchicon/install_ports.go:77` `hostDataDir(instance)` → `~/.local/share/orchicon-<inst>`; `install.go:501` `hostDataDirFor` | Two instances never collide |
| Per-instance port table | `cmd/orchicon/install_ports.go:162` `installerPorts()` — Postgres `5432/5433/5434`, NATS `4222/4223/4224`; `:201` `defaultFor`, `:231` `activeInstallPorts` | The ONE source of ports (asserted equal to `scripts/container.sh`'s table by `TestInstallPortDefaultsMatchContainerScript`) |
| Port-free probe | `install_ports.go:278` `portFree(port)` | AC6 orphan verification |
| Resolved-port record | `install.go` `resolvedPortsFile`/`writeResolvedPorts`/`readResolvedPorts` | The instance's *actual* ports, read back without re-deriving |

What is **still missing on `develop`** (the actual gap this work item closes — every row verified absent):

| Gap | Evidence on `develop` |
|---|---|
| `Makefile` `DB_URL` is a **dev literal** | `Makefile:77` `DB_URL ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` — prod is 5433, so a prod checkout's `make migrate` hits DEV's DB |
| `make run` starts **no services** and passes no plane env | `Makefile:192-193` `run: fetch-tags fe-build` → `$(GO) run … ./cmd/orchicon`; plane defaults are `5432`/`4222` (`internal/config/config.go:207-208`) |
| No `services`/`services-down` targets | `grep -c services Makefile` on develop = **0** |
| No pure `services-port`/`db-url` verbs | `scripts/container.sh` has `instance_info()` `:95` but no such verbs |
| `check-rls.sh` cannot reach a host-services psql | `scripts/check-rls.sh:31-42` `run_sql` tries host `psql` then `docker exec` — a downloaded-only host has psql in `<DataDir>/bin/postgres/bin/psql`, not on PATH |
| No `orchicon services` inner-loop entrypoint | `cmd/orchicon/main.go:54-80` dispatch has no `services` case |

**A prior sibling branch** (`…-qa6q1nw8eda5mfzy`, commits `5c8c59967`+`dab0957687`+`b0e107d1e5`)
built this feature against the OLD tree with a **597-line `services.go` + 475-line
`services_acquire.go` second supervisor and a `hostMode` branch on `container.go`.** Those files
now **CONFLICT** with `develop` (`git merge-tree` reports content conflicts in `Makefile`,
`cmd/orchicon/container.go`, `internal/askorchicon/chat_turn_db_test.go`), because `develop`
superseded that design with `residency.go` + `binaries.go` + `install_host.go`. **This plan
deliberately does NOT resurrect that code.** Only three ideas from it survive, and they are
wiring, not a supervisor: derive `DB_URL`, make `services` a `run`-only prerequisite, and add a
`check-rls.sh` bindir rung.

## Decisions (one line each)

- **DECISION:** reuse `startHostServices`/`stopHostServices`/`servicesReady` (`install_host.go`) as
  the engine of the new `orchicon services` verb — they already do idempotent detached start, PID
  recording, and bounded stop. No new supervisor, no `hostMode` branch on `container.go`.
- **DECISION:** add a thin `orchicon services up|down|status [instance]` subcommand
  (`cmd/orchicon/services.go`, ~180 lines) rather than exposing raw env on `container` — the inner
  loop needs a *verb* the Makefile can call, and `down` needs port-free verification the raw
  `container` boot does not perform.
- **DECISION:** telemetry is **off** for the inner loop — `make services` exports
  `ORCHICON_TELEMETRY=none`, which passes through `startHostServices`' inherited env into the
  detached `container` child. Rationale: the inner loop needs the DB and the bus; the four
  telemetry backends are ~300 MB of downloads and a wider port set for no inner-loop benefit.
- **DECISION:** `DB_URL` is **derived** via a new pure `scripts/container.sh db-url <inst>`, backed
  by `services-port` which reads `instance_info` (`container.sh:95`, the override choke point at
  `:185`). One source of truth ⇒ the DSN and the bound port cannot drift, and two instances cannot
  collide (AC4).
- **DECISION:** only `make run` gets a `services` prerequisite. `migrate`/`rls-check`/`test` do
  **not**: CI runs them after `container up` against the container DB, and starting host services
  there would double-bind 5432. They keep consuming `$(DB_URL)`, which now resolves per instance.
- **DECISION (revisitable):** `services up` builds its port map from `activeInstallPorts(residencyHost)`
  defaults + `ORCHICON_*_PORT` overrides (never `resolveInstallPorts`, which **prompts on a tty** —
  a Makefile prerequisite must never ask a question). If a future install path always writes
  `ports.json`, this can prefer `readResolvedPorts`; today defaults+overrides is the dependency-free
  path.
- **DECISION:** `services down` = `stopHostServices` **then** verify `portFree` on the instance's
  postgres/nats ports, and only then report success — an orphaned Postgres holding 5432 is the
  classic next-start failure (AC6).

## File-level evidence (change site → exact anchor)

| # | Change site | `path:line` read this session | Today |
|---|---|---|---|
| 1 | Plane run target | `Makefile:192` | `run: fetch-tags fe-build ## Run the control plane from source` |
| 2 | DSN literal | `Makefile:77` | `DB_URL ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` |
| 3 | Migrate | `Makefile:329-331` | `migrate:` applies with `"$(DB_URL)"` |
| 4 | RLS gate | `Makefile:341-342` | `rls-check:` → `scripts/check-rls.sh "$(DB_URL)"` |
| 5 | Test isolation | `Makefile:222-224` | `for v in ORCHICON_GUARD_POLICY … ORCHICON_SERVE_STATE_DIR; do unset "$$v"; done; ORCHICON_CONFIG_DIR=… $(GO) test $(PKGS)` |
| 6 | CI gate list | `Makefile:789-790` | `ci-go: … host-residency-test cache-trim-test …` |
| 7 | Help generator | `Makefile:102-103` | `awk '… /^[a-zA-Z_-]+:.*##/ …'` |
| 8 | Subcommand dispatch | `cmd/orchicon/main.go:54-80` | `case "install" … case "restart" … default:` |
| 9 | Help rows | `cmd/orchicon/main.go` `printHelp` "Setting up and driving an instance:" block | rows are data; `help_test.go` **derives the expectation from the switch** |
| 10 | Pure port table (bash) | `scripts/container.sh:95` `instance_info()`; overrides `:185-194` | dev PG 5432 / prod 5433 / test 5434 |
| 11 | 3rd-arg guard | `scripts/container.sh:1648-1658` | rejects anything not `host`/`container`/`--yes` |
| 12 | Verb dispatch | `scripts/container.sh:1659` `case "${1:-}" in` … `:1702 *)` usage | no `services-port`/`db-url` |
| 13 | RLS reachability | `scripts/check-rls.sh:31-42` | host psql → `docker exec` → return 3 (skip) |
| 14 | Host-service starter | `cmd/orchicon/install_host.go:538` `startHostServices(instance, ports, hostDataDir, out)` | takes resolved `map[string]int` ports + data dir |
| 15 | Host-service stopper | `cmd/orchicon/install_host.go:588` `stopHostServices(instance, out)` | bounded `terminateProcess(pid)`; **does not** verify ports |
| 16 | Idempotence probe | `cmd/orchicon/install_host.go:393` `servicesReady(instance, ports)` | pg `pg_isready` + nats `/healthz` |
| 17 | PID file | `cmd/orchicon/install_host.go:482` `hostServicesPIDFile(instance)` | `<dataDir>/serve/services.pid` |
| 18 | Port knobs | `cmd/orchicon/install_host.go:513` `hostServicesPortEnvs` | the 12 `ORCHICON_*_PORT` names |
| 19 | Port-table defaults | `cmd/orchicon/install_ports.go:162` `installerPorts`, `:201` `defaultFor`, `:231` `activeInstallPorts` | the ONE Go table |
| 20 | Port probe | `cmd/orchicon/install_ports.go:278` `portFree(port)` | bind 127.0.0.1:port, close |
| 21 | Data dir | `cmd/orchicon/install_ports.go:77` `hostDataDir`, `install.go:501` `hostDataDirFor` | `~/.local/share/orchicon-<inst>` |
| 22 | Infra record | `cmd/orchicon/binaries.go` `infraBinaries.Psql` in `<DataDir>/infra-binaries.json` | the psql check-rls.sh must find |
| 23 | Plane config defaults | `internal/config/config.go:207-208` | `PostgresDSN …5432`, `NATSURL …4222` |
| 24 | Docs | `ARCHITECTURE.md:1018` §Host residency | needs the inner-loop subsection |

## New files

1. **`scripts/tests/local-services/run.sh`** — offline, docker-free AND database-free harness
   (mirrors `scripts/tests/host-residency/run.sh` conventions; it EXECUTES `container.sh`'s pure
   verbs and `make -n` rather than extracting text):
   - `container.sh services-port dev postgres` = `5432`; `prod` = `5433`; `test` = `5434`;
     `services-port dev nats` = `4222`; `prod` = `4223`.
   - `container.sh db-url prod` contains `:5433/`; `db-url dev` contains `:5432/`.
   - `services-port` honours `ORCHICON_POSTGRES_PORT=6543` → `6543` (the override rung).
   - The Makefile's `DB_URL` default no longer contains a bare `5432` literal (it is derived):
     `make -n migrate instance=prod` resolves a `:5433/` DSN.
   - `make help` lists `services`, `services-down`, `services-status` (AC8).
   - `make -n run` shows a `services` prerequisite; `make -n migrate`/`rls-check`/`test` do **not**
     (AC7: isolation contract unbroken).
   - The `make test` recipe still contains the ambient-unset list and does **not** unset
     `ORCHICON_TEST_DSN`.
   - `MAKEFLAGS` is cleared for child `make` calls and Entering/Leaving banners stripped (the
     measured `make-in-make` failure mode).

2. **`cmd/orchicon/services.go`** — the inner-loop verb (thin; reuses `install_host.go`):
   - `func runServices(args []string) int` — verb dispatch `up|down|status` (bare → `status`, so a
     no-arg command never starts a database); instance from arg else `ORCHICON_INSTANCE` (dev).
   - `func servicesPortsFor(instance string) (map[string]int, error)` — `activeInstallPorts(residencyHost)`
     defaults via `defaultFor`, then apply each `hostServicesPortEnvs` env override. **Never**
     `resolveInstallPorts` (it prompts).
   - `func servicesUp(instance string) int` — build ports; `if servicesReady(instance, ports)` print
     `already running` and exit 0 (AC5); else `startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)`
     then `waitServicesReady`-style poll, and print the DSN.
   - `func servicesDown(instance string) int` — `stopHostServices(instance, os.Stdout)`, then poll
     `portFree(ports["ORCHICON_POSTGRES_PORT"])` and `portFree(ports["ORCHICON_NATS_PORT"])` for a
     bounded window; only report success when both are free (AC6); name any port still held.
   - `func servicesStatus(instance string) int` — `hostServicesAlive(instance)` + `servicesReady`
     + the per-instance DSN; exit 0 when up, 1 when down.

3. **`architecture-notes/<this file>`** — this plan (gitignored at `.gitignore:88`; the SSE must
   `git add -f` if it wants it committed).

No new file for binary acquisition — `binaries.go` already does it (develop superseded the prior
branch's `services_acquire.go`).

## Modified files (with exact insertion points)

1. **`scripts/container.sh`** — two pure verbs, near the other query verbs in the `case` block
   (`:1677` `plane-status)` / before `:1702 *)`):
   - `services_port() { local inst="${1:-dev}" svc="${2:-postgres}"; instance_info "$inst" || return 1; case "$svc" in postgres|pg) echo "$PG_PORT";; nats) echo "$NATS_PORT";; nats-mon) echo "$NATS_MON_PORT";; *) echo "unknown service: $svc" >&2; return 1;; esac; }`
   - `db_url() { local inst="${1:-dev}" p; p="$(services_port "$inst" postgres)" || return 1; echo "postgres://orchicon:orchicon@localhost:${p}/orchicon?sslmode=disable"; }`
   - dispatch: `services-port) services_port "${2:-dev}" "${3:-postgres}" ;;` and `db-url) db_url "${2:-dev}" ;;`
   - **3rd-arg guard fix (`:1648-1658`):** `services-port` takes a real 3rd arg (`<svc>`), so the
     residency guard must not claim it. Scope it by command name:
     `case "${1:-}" in services-port|db-url) ;; *) case "${3}" in host|container)…;; --yes|-y)…;; *) refuse ;; esac ;; esac`.
   - extend the `*)` usage string to list the two new verbs.

2. **`Makefile`** —
   - `Makefile:77`: add `instance ?= dev` above it and replace the literal with
     `DB_URL ?= $(shell scripts/container.sh db-url $(instance) 2>/dev/null || echo postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable)`
     (the fallback keeps `DB_URL` non-empty when `scripts/`/bash is unavailable; explicit `DB_URL` still wins by `?=`).
   - `Makefile:192`: `run: services fetch-tags fe-build ## Run the control plane from source (starts this instance's local services first)`
     and export the plane env in the recipe so the plane addresses the instance the services bound:
     `ORCHICON_INSTANCE=$(instance) ORCHICON_DATA_DIR="$${ORCHICON_DATA_DIR:-$$HOME/.local/share/orchicon-$(instance)}" ORCHICON_POSTGRES_DSN="$(DB_URL)" ORCHICON_NATS_URL="nats://localhost:$$(scripts/container.sh services-port $(instance) nats)" $(GO) run …`
     (plane defaults are 5432/4222 — `internal/config/config.go:207-208` — so without this a
     `instance=prod` run points at dev).
   - new targets in the Database section after `Makefile:342` (`rls-check`), all `.PHONY` with `##`
     help comments (AC8):
     ```
     services: ## Start this instance's local Postgres + NATS on the host (no container engine)
     	ORCHICON_TELEMETRY=none $(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services up $(instance)
     services-down: ## Stop the local services for this instance (verifies the ports are free)
     	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services down $(instance)
     services-status: ## Show the local services state and the DSN for this instance
     	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services status $(instance)
     ```
   - `Makefile:329` migrate comment: append `(instance=<dev|prod|test> picks the instance)`.
   - `Makefile:789` `ci-go`: insert `local-services-test` before `cache-trim-test`.
     new target: `local-services-test: ## CI gate: the local-services inner loop (derived DB_URL, per-instance ports, help rows)` → `bash scripts/tests/local-services/run.sh`.
   - `Makefile:222-224` test recipe: **unchanged behaviour**; add only a one-line comment that
     services are deliberately not started here and `ORCHICON_SERVICE_RESIDENCY` is not in the
     unset list (the inner loop must stay reachable, CI must not double-bind 5432).

3. **`scripts/check-rls.sh`** — add a bindir rung to `run_sql` (`:31-42`), **between** the host
   `psql` branch and the `docker exec` branch:
   - resolve from the infra record: `bindir_psql="${ORCHICON_PG_PSQL:-$(container.sh pg-psql <inst>)}"`,
     where the new `pg-psql` verb reads `infraBinaries.Psql` from `<hostDataDir>/infra-binaries.json`.
     (Simpler, dependency-free fallback also acceptable: `python3 -c` / `sed` read of the JSON, with
     `ORCHICON_PG_PSQL` winning.) If it is executable, use it before `docker exec`.
   - update the `rc==3` skip message to name the bindir rung.
   - **Add the `pg-psql` pure verb to `container.sh`** alongside `services-port`/`db-url` (reads
     `~/.local/share/orchicon-<inst>/infra-binaries.json`, prints `.psql`; empty when absent).

4. **`cmd/orchicon/main.go`** — dispatch (`:76`) insert `case "services": os.Exit(runServices(os.Args[2:]))`,
   and a `printHelp` row in the "Setting up and driving an instance:" section:
   `{cmd: bin + " services", desc: []string{"Start/stop this instance's Postgres + NATS on the HOST (no container).", "services up | down | status — the backend `make run` / `make migrate` need."}}`.
   **Mandatory:** `help_test.go` derives the expectation from the dispatch switch, so adding the
   `case` without the row fails `make test`.

5. **`ARCHITECTURE.md`** — a short subsection near `:1018` §Host residency documenting
   `orchicon services up|down|status`, the derived `DB_URL`, telemetry-off, and that `services` is a
   prerequisite of `run` only.

## Acceptance-criteria → mechanism map

| AC | Mechanism |
|---|---|
| 1 no engine/no Postgres → `make run` | `binaries.go` `acquireInfraBinaries` (already ships) + `run: services` starts them |
| 2 `make migrate` + `make rls-check` against local PG | derived `DB_URL`; `check-rls.sh` bindir rung via `pg-psql` |
| 3 DB tests pass; `ORCHICON_TEST_DSN` override | `test` untouched; `ORCHICON_TEST_DSN` stays outside the unset list (harness asserts) |
| 4 per-instance ports, no collision | one port table (`install_ports.go:162` ↔ `container.sh:95`); `db-url` derives; `run` exports the derived DSN/NATS |
| 5 idempotent start | `servicesUp` returns early on `servicesReady` (`install_host.go:393`) |
| 6 stop leaves no orphans | `servicesDown` = `stopHostServices` + `portFree` verification (`install_ports.go:278`) |
| 7 test isolation unbroken | no `services` prereq on `test`; unset list intact; `ORCHICON_TEST_DSN` reachable |
| 8 `make help` rows | `##` comments on all three targets; `awk` help at `Makefile:102` |

## Numbered implementation list (mechanically executable — start here)

1. **`scripts/container.sh`**: add `services_port()`, `db_url()`, `pg_psql()` functions (body in
   "Modified files" #1/#3), place them beside `logs_instance()` (~`:1635`).
2. **`scripts/container.sh`**: in the `case "${1:-}"` dispatch (after `:1677` `plane-status)`), add
   `services-port) services_port "${2:-dev}" "${3:-postgres}" ;;`, `db-url) db_url "${2:-dev}" ;;`,
   `pg-psql) pg_psql "${2:-dev}" ;;`.
3. **`scripts/container.sh`**: wrap the 3rd-arg residency guard at `:1648-1658` so
   `services-port`/`db-url`/`pg-psql` bypass it (nested `case "${1:-}"`); extend the `:1702` usage
   string.
4. **`Makefile:77`**: insert `instance ?= dev` and replace the `DB_URL` literal with the derived
   `$(shell scripts/container.sh db-url $(instance) … || echo <fallback>)`.
5. **`Makefile:192`**: add the `services` prerequisite to `run` and the export block
   (`ORCHICON_INSTANCE`, `ORCHICON_DATA_DIR`, `ORCHICON_POSTGRES_DSN="$(DB_URL)"`, `ORCHICON_NATS_URL`).
6. **`Makefile`** after `:342`: add `.PHONY: services services-down services-status` + the three
   targets (bodies above), each with a `##` comment.
7. **`Makefile:329`**: extend the `migrate` help comment with the `instance=` note.
8. **`Makefile:789`**: insert `local-services-test` into `ci-go` before `cache-trim-test`; add the
   `local-services-test` target.
9. **`Makefile:222-224`**: add the isolation comment only (no behavioural change).
10. **`scripts/check-rls.sh`**: add the bindir `psql` rung in `run_sql` (`:31-42`) and update the
    `rc==3` message.
11. **`cmd/orchicon/services.go`** (new): `runServices`, `servicesPortsFor`, `servicesUp`,
    `servicesDown`, `servicesStatus` per "New files" #2, reusing `startHostServices`,
    `stopHostServices`, `servicesReady`, `hostServicesAlive`, `hostServicesPIDFile`, `portFree`,
    `activeInstallPorts`, `defaultFor`, `hostDataDirFor`, `hostServicesPortEnvs`.
12. **`cmd/orchicon/main.go`**: add the `case "services"` dispatch (`:76`) **and** the `printHelp`
    row (both, or `help_test.go` fails).
13. **`ARCHITECTURE.md`**: add the §Host-residency inner-loop subsection.
14. **`scripts/tests/local-services/run.sh`** (new): the offline assertions listed in "New files" #1.
15. **Verify**: `make local-services-test`, `make vet`, `make test` (help_test + ports tests),
    `make -n run instance=prod` shows the `:5433` DSN, `make services && make services-status &&
    make services-down` on a clean host, and `make migrate`/`make rls-check` against the started PG.
    Known environment caveat: `make migrate` currently fails inside the `orchicon-dev` runtime image
    because the atlas build there rejects migration versions (recorded fact) — that is an
    image/atlas issue, not this feature; verify migrate by DSN resolution + the `rls-check` path if
    the sandbox atlas refuses.
