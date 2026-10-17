# Local services for the inner loop (`make run` / `migrate` / `DB_URL` with no container)

Implementation plan — step 1 (Principal Software Architect), branch
`local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container-bnwmrgcqymtqd6h9`
(HEAD = `623c74ccb`, current `develop`).

## 1. What develop ALREADY ships (read this session — reuse, never rebuild)

The **supervisor** half is on develop; only the **inner-loop wiring** is missing.

| Capability | Proof (this branch, current line numbers) |
|---|---|
| Services-only supervisor mode (the `ORCHICON_CONTAINER_SERVICES_ONLY=1` equivalent) | `cmd/orchicon/container.go:64` `servicesOnlyEnv`, `:86` `servicesOnly := servicesOnlyFromEnv() \|\| residency == residencyHost`, `:135` `if servicesOnly` skips the plane child |
| Per-instance port table (dev 5432/4222, prod 5433/4223, test 5434/4224) | `scripts/container.sh:95` `instance_info()`, `:111-112` dev `PG_PORT=5432` `NATS_PORT=4222`, `:131-132` prod 5433/4223, `:120`/`:140` `HOST_DATA_DIR`; Go half `cmd/orchicon/residency.go:158` `servicePortsFor`, `cmd/orchicon/install_ports.go` `installerPorts` |
| Host services start/stop, idempotent, PID file, port envs | `cmd/orchicon/install_host.go:538` `startHostServices`, `:588` `stopHostServices`, `:393` `servicesReady`, `:482` `hostServicesPIDFile`, `:494` `hostServicesAlive` |
| Binary acquisition (no Postgres installed → download pinned PG 16 + NATS) | `cmd/orchicon/binaries.go` (`infra-binaries.json` record, cited at `:89`) |
| Ports record | `cmd/orchicon/install.go:436` `ports.json` in the instance state dir |

## 2. What is MISSING (the gap this work item closes) — verified absent on HEAD

- `Makefile:105` `DB_URL ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` — a **dev literal**; `grep -c services Makefile` = 0; there is **no `instance` variable** (only `container-rebuild:546` / `full-rebuild:611` guards that test `$(instance)`).
- `Makefile:220` `run: fetch-tags fe-build` starts **no services** and exports **no** DSN/NATS/HTTP_ADDR — the plane falls back to its config defaults (`internal/config/config.go` dev 5432/4222 and wildcard `:8080`), so `instance=prod` would reach dev's DB and dev's port.
- `Makefile:357` `migrate` shells out to the Atlas **CLI** (`curl -sSfL https://atlasgo.sh | sh`) — a network install an offline, container-less host does not have.
- `Makefile:370` `rls-check: scripts/check-rls.sh "$(DB_URL)"` — passes no instance, so the script cannot find the instance's *downloaded* psql.
- `scripts/check-rls.sh:33` `run_sql()` has only two rungs: host `psql` on PATH, then `docker exec` — **both unavailable** on a container-less host with no Postgres.
- `scripts/container.sh` has **no** `services-port` / `db-url` / `pg-psql` verbs, so the Makefile cannot derive the DSN from the per-instance table.
- `cmd/orchicon/main.go:54-83` has no `services` case; `commands/service.go`-style entry point absent (`cmd/orchicon/services.go` does not exist).
- `scripts/tests/local-services/` does not exist.

## 3. DECISION: port the proven implementation, do not rebuild

An earlier run of this same work item produced a **complete, reviewed, harness-pinned** implementation on
`origin/local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container-49d1jjyn8j31gf52`
(commits `1e04b9fec5` plan → `10bc7d484a` impl → `00b5908f8f`, `2d9489be85` (PR-review fixes),
`9725013578`, `3668629f50`). Merge-base with this branch is `8865d24fb33a`; this branch is **6 commits
ahead** of it, and of the 11 files that feature touches **only `Makefile` was also touched on develop**
(verified by per-file `git diff --quiet 8865d24fb33 HEAD`). So the port is mechanical.

**DECISION (revisitable):** land it by `git cherry-pick` of the feature commits onto this branch (or
`git merge origin/<that branch>` if the cherry-pick is noisy), then re-anchor the Makefile hunks against
the current line numbers below. Rationale: it is already proven end-to-end (plane boots against host
services, 45 tables, RLS gate green, 49 harness assertions), so re-deriving it would repeat a reviewed
design and re-introduce the three bugs the PR review already found (per-instance `ORCHICON_HTTP_ADDR`,
`ORCHICON_INSTANCE` travelling with the RLS DSN, record-first port derivation).

Do **not** resurrect either prior sibling branch (`...-qa6q1nw8eda5mfzy`, `...-5g2t2z592f7g5ete`): they
predate develop's residency/binaries/install_host layer and add a conflicting 597-line
`services.go` + `services_acquire.go` + a `hostMode` branch in `container.go`.

## 4. File-level delta against THIS branch's line numbers

### 4.1 `Makefile` (only file with develop-side churn — re-anchor every hunk)

| Where | Change |
|---|---|
| `:105` (replace `DB_URL ?=`) | add `instance ?= dev`, `DB_URL ?= $(shell scripts/container.sh db-url $(instance) 2>/dev/null \|\| echo postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable)`, and `instance_given = $(filter command line environment override,$(origin instance))`. **The `instance_given` indirection is mandatory**: `instance ?= dev` otherwise makes the `test -n "$(instance)"` guards at `:546` and `:611` pass vacuously, turning a bare `make full-rebuild` into a silent dev rebuild. |
| `:220` `run:` | `run: services fetch-tags fe-build` and export `ORCHICON_INSTANCE=$(instance)`, `ORCHICON_DATA_DIR="$${ORCHICON_DATA_DIR:-$$HOME/.local/share/orchicon-$(instance)}"`, `ORCHICON_HTTP_ADDR="127.0.0.1:$$(scripts/container.sh services-port $(instance) control)"` (else the plane binds the wildcard `:8080` on every interface — AC4 + LAN exposure), `ORCHICON_POSTGRES_DSN="$(DB_URL)"`, `ORCHICON_NATS_URL="nats://localhost:$$(scripts/container.sh services-port $(instance) nats)"`. |
| `:223` `test:` | **comment only** (no behavioural change): state that `services` is deliberately NOT a prerequisite and `ORCHICON_SERVICE_RESIDENCY` is deliberately not in the `:250` unset list, because CI runs `test` after `container up` against the container DB (a prerequisite would double-bind 5432) and the opt-in variables (`ORCHICON_TEST_DSN`, `ORCHICON_SKIP_NETWORK_TESTS`, `ORCHICON_LIVE_*`) must stay reachable. |
| `:268` `clean:` | `@rm -f $(BIN_DIR)/atlas-ci` (new artifact dir entry). |
| `:357` `migrate:` | apply via the repo's own engine: `@$(GO) build -o $(BIN_DIR)/atlas-ci ./tools/atlas-ci` then `DB_URL="$(DB_URL)" $(BIN_DIR)/atlas-ci`. The Atlas **CLI** cannot read `db/migrations` at all (paired `*_down.sql` read as duplicate versions) and needs a network install. `tools/atlas-ci` exists on this branch (`tools/atlas-ci/main.go`). |
| `:370` `rls-check:` | `ORCHICON_INSTANCE=$(instance) scripts/check-rls.sh "$(DB_URL)"` — the instance must travel with the DSN or `rls-check instance=prod` resolves **dev**'s psql record. |
| after `:372` (`synth-data`), new block | `services` / `services-down` / `services-status` targets invoking `$(GO) run ./cmd/orchicon services up\|down\|status $(instance)` (`ORCHICON_TELEMETRY=none` on `up`), each with a `##` comment for `make help` (the `awk` at `:132` only lists `^[a-zA-Z_-]+:.*##`, so the comment must sit on the target line). |
| after `:450` `host-residency-test` | `local-services-test:` running `bash scripts/tests/local-services/run.sh`, with `##` comment. |
| `:830` `ci-go:` | add `local-services-test` to the prerequisite list (keep the existing union of script gates). |
| `:546`, `:611` | change the guards from `test -n "$(instance)"` to `test -n "$(instance_given)"`. |

### 4.2 `cmd/orchicon/main.go`

- `:77` (after `case "restart":`) add `case "services": os.Exit(runServices(os.Args[2:]))`.
- `printHelp()` (`:255`) add a `{cmd: bin + " services", desc: …}` row **after the `restart` row** — `cmd/orchicon/help_test.go` derives its expectations from what `printHelp` prints, so a missing row fails the `cmd/orchicon` test package.

### 4.3 New file `cmd/orchicon/services.go` (≈238 lines — take from the reference branch verbatim)

`runServices(args []string) int` (:39, verb dispatch `up|down|status` + instance arg),
`servicesPortsFor(instance string) (map[string]int, error)` (:78, **record-first**: trusts
`<hostDataDir>/ports.json` only when it carries BOTH postgres and nats, else defaults + `ORCHICON_*_PORT`
overrides; must never call `resolveInstallPorts`, which **prompts on /dev/tty**),
`servicesDSN(ports)` (:110), `telemetryOffForTheInnerLoop()` (:123), `servicesUp` (:130,
`startHostServices` → idempotent via `servicesReady`), `servicesDown` (:164, `stopHostServices` +
**port-free verification**, AC6), `servicesStatus` (:215).

### 4.4 New file `cmd/orchicon/services_test.go` (≈69 lines)

Pins: the postgres arg carries `-c unix_socket_directories=…` under the data dir (root cause: the compiled-in
`/var/run/postgresql` is unwritable for a non-root user), record-first vs defaults+override port derivation,
and DSN shape.

### 4.5 `scripts/container.sh` (1710 lines on HEAD)

- Insert three pure query verbs (`_recorded_port`, `services_port`, `db_url`, `pg_psql`) **after
  `logs_instance()`** (ends ~`:1640`), before the third-positional guard at `:1645`.
- `:1645-1657` — wrap the residency/`--yes` third-positional guard in `case "${1:-}" in services-port|db-url|pg-psql) ;; *) …` so `services-port dev nats` is not refused as a bad shape word (guard error text is at `:1653`).
- Dispatch: add `services-port)`, `db-url)`, `pg-psql)` cases after `plane-status)` (`:1671`-ish), and extend the usage string at the `*)` arm with the three verbs.

### 4.6 `scripts/check-rls.sh` (88 lines on HEAD)

- Near `:27-28` (`URL="$1"`, `CONTAINER=`) add `REPO_ROOT` and `INSTANCE="${ORCHICON_INSTANCE:-dev}"`.
- In `run_sql()` (`:33`) add the rung **between** host-psql and `docker exec`: `the_bindir_psql()` →
  `scripts/container.sh pg-psql "$INSTANCE"` (or `$ORCHICON_PG_PSQL`), executed only when it exists and
  `SELECT 1` succeeds. This is what makes AC2 reachable on a host with no system psql.
- Update the `rc -eq 3` skip text (~:72) to name the missing downloaded psql and the instance.

### 4.7 `ARCHITECTURE.md`

Insert an `#### The inner loop with no container: make services` subsection immediately after the
published-port-table paragraph (~`:1036`, the sentence ending “ports do not depend on which entry point
created it.”): the command table, “one implementation, not two”, “DB_URL is DERIVED”, “`services` is a
prerequisite of `run` alone”, the RLS downloaded-psql rung, and the verification pointers.

### 4.8 New file `scripts/tests/local-services/run.sh` (≈240 lines, 49 assertions)

Offline harness (no Docker, no DB): derived `DB_URL` per instance + `ORCHICON_POSTGRES_PORT` override,
record-first vs partial-record fallback, `services` is a prerequisite of `run` **only**, `run` exports the
per-instance `ORCHICON_HTTP_ADDR`, the migrate recipe uses the Go engine, `rls-check` passes
`ORCHICON_INSTANCE`, the `test` isolation unset list still contains the six ambient vars and still leaves
`ORCHICON_TEST_DSN` reachable, and the three new targets have `##` help comments.

### 4.9 `internal/askorchicon/chat_turn_db_test.go` + `robustness_db_test.go` (test-only)

Add `waitForMessageSessionID` (`chat_turn_db_test.go:<waitForMessage>`, after
`func waitForMessage`) and use it at `TestStartConversationTurnServeLossFreshSessionFallback` and
`TestInterjectRecyclesWedgedSession`. Running the DB-backed suite against the **locally started** services
(AC3) exposes the existing race: the row is written twice (partial mirror, then terminal finalize) and
`waitForMessage` returns on the first. Not part of the feature — ship it as its own `test(ask):` commit,
flag it in the PR body as a flake fix the local-services run surfaced.

## 5. Acceptance-criteria mapping

| AC | Evidence |
|---|---|
| AC1 no container / no Postgres | `make services` → `startHostServices` → `binaries.go` detect-else-download; then `make run` |
| AC2 migrate + rls-check | `make migrate` (Go engine over `db/migrations`), `make rls-check` (downloaded-psql rung) |
| AC3 DB tests + `ORCHICON_TEST_DSN` | `ORCHICON_TEST_DSN="$(scripts/container.sh db-url dev)" make test` — the opt-in var is read directly by the tests (`internal/adapter/runsteps_test.go:26`, `internal/aigateway/usage_pagination_test.go:29`) and is not in the `Makefile:250` unset list |
| AC4 per-instance ports | `services_port`/`db_url` read `instance_info` + `ports.json`; prod 5433, test 5434; `run` exports `ORCHICON_HTTP_ADDR` on the instance's own loopback port |
| AC5 idempotent up | `servicesReady` probe in `install_host.go:393`; `servicesUp` prints “already running” |
| AC6 no orphans | `servicesDown` = `stopHostServices` + port-free verification, non-zero exit naming the port |
| AC7 test isolation | `test:` unset list unchanged (`Makefile:250`); `services` not a prerequisite of `test` |
| AC8 `make help` | `##` comments on all three new targets + `local-services-test` |

## 6. Numbered implementation steps (mechanically executable — no re-planning needed)

1. `git fetch origin`; confirm the branch is `local-services-for-the-inner-loop-…-bnwmrgcqymtqd6h9` at `623c74ccb`.
2. `git cherry-pick 9c9ef2f56f 10bc7d484a 00b5908f8f 2d9489be85 9725013578` from
   `origin/local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container-49d1jjyn8j31gf52`
   (**skip** `1e04b9fec5`, the old plan doc, and `3668629f50`, the askorchicon flake fix — step 9 replays it).
   Expect conflicts only in `Makefile`; resolve using section 4.1 against the CURRENT line numbers
   (`:105`, `:220`, `:223`, `:250`, `:268`, `:357`, `:370`, `:372`, `:450`, `:546`, `:611`, `:830`).
   Fallback if the cherry-pick is noisy: `git merge --no-ff origin/<that branch>` and resolve the same hunks.
3. `Makefile:105` — `instance ?= dev`, derived `DB_URL ?= $(shell scripts/container.sh db-url $(instance) …)`,
   and `instance_given`. Then change **both** destructive guards (`:546` `container-rebuild`, `:611`
   `full-rebuild`) from `$(instance)` to `$(instance_given)`, and verify: `make full-rebuild` (no arg) must
   print its usage line and exit 1; `make full-rebuild instance=dev` must proceed.
4. `Makefile:220` — `run: services fetch-tags fe-build` + the five `ORCHICON_*` exports (instance, data dir,
   loopback `ORCHICON_HTTP_ADDR`, `ORCHICON_POSTGRES_DSN=$(DB_URL)`, `ORCHICON_NATS_URL`).
5. `Makefile:357` — `migrate` builds and runs `bin/atlas-ci` (Go engine) instead of the Atlas CLI;
   `:370` — `ORCHICON_INSTANCE=$(instance) scripts/check-rls.sh "$(DB_URL)"`; `:268` — `rm -f bin/atlas-ci`.
6. Insert the `services` / `services-down` / `services-status` block after `:372` (`synth-data`), and
   `local-services-test` after `:450` (`host-residency-test`); add `local-services-test` to `ci-go:830`.
   Every target line carries a `##` comment (AC8 — the help `awk` at `:132` matches `^[a-zA-Z_-]+:.*##`).
7. Create `cmd/orchicon/services.go` (4.3) and `cmd/orchicon/services_test.go` (4.4) — copy from the
   reference branch. Add the `main.go:77` `case "services"` and the `printHelp` row in the SAME commit
   (`cmd/orchicon/help_test.go` fails without the row).
8. `scripts/container.sh`: insert the four pure functions after `logs_instance()` (~`:1640`), scope the
   third-positional guard (`:1645-1657`) to the non-query verbs, add the three dispatch cases, extend usage.
9. Replay `3668629f50` (the `waitForMessageSessionID` flake fix) as its own `test(ask):` commit — it is
   needed for the DB-backed packages to be reliably green against the locally started services (AC3).
10. `scripts/check-rls.sh`: `REPO_ROOT` + `INSTANCE` (`:27-28`), the `the_bindir_psql` rung inside
    `run_sql()` (`:33`), updated skip text (~`:72`).
11. Create `scripts/tests/local-services/run.sh` (4.8) with its 49 assertions.
12. `ARCHITECTURE.md`: the `#### The inner loop with no container: make services` subsection (~`:1036`).
13. Run section 7's verification, fix whatever it surfaces, then hand off — do NOT open or merge the PR
    (that is a later step's contract).

## 7. Verification (each maps to an AC; run in this order)

```bash
# static gates first (fast, offline)
make local-services-test                              # 49 assertions: AC4, AC5 shape, AC7, AC8
make help | grep -E 'services|local-services-test'    # AC8
bash -n scripts/container.sh scripts/check-rls.sh     # shell syntax
go build ./... && go vet ./cmd/orchicon

# AC1/AC2 — the real inner loop, no container, no system Postgres
make services        # FIRST run downloads the pinned PG + NATS (binaries.go detect-else-download)
make services        # SECOND run: idempotent no-op that says so (AC5)
make services-status # exit 0 while up; prints the derived DSN
make migrate         # AC2: Go engine over db/migrations
make rls-check       # AC2: downloaded-psql rung
make run instance=dev # AC1: plane boots against the host services on 5432/4222

# AC4 — a second instance never collides
make services instance=prod && make services-status instance=prod   # 5433/4223, distinct DSN
make services-down instance=prod

# AC3 — DB-backed tests against the locally started services
ORCHICON_TEST_DSN="$(scripts/container.sh db-url dev)" \
  go test ./internal/adapter ./internal/aigateway ./internal/askorchicon -count=1

# AC6 — no orphans
make services-down && scripts/container.sh services-port dev postgres  # port freed, no pg process left
```

**Never** add `services` as a prerequisite of `migrate`, `rls-check` or `test`: CI runs all three after
`container up` against the container DB, so a prerequisite there would double-bind 5432 and break CI.
Sandbox note: in the `orchicon-dev` runtime container ports 5432/4222 are already held by the in-container
sandbox plane, so exercise `make services` with `ORCHICON_POSTGRES_PORT`/`ORCHICON_NATS_PORT` overrides
(e.g. 55432/54222) and `instance=test`, or rely on `make local-services-test` (pure shell, no binds).
