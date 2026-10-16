# Local services for the inner loop (make run / migrate / DB_URL with no container)

**Work item:** local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container
**Step:** 1 — Principal Software Architect (implementation plan; no code written)
**Branch:** `local-services-for-the-inner-loop-make-run-migrate-db-url-with-no-container-qa6q1nw8eda5mfzy`

## Context

`make run` is `$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon` (`Makefile:187-188`) — it boots the
plane with **no backends**. `make migrate` uses `DB_URL ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable`
(`Makefile:77`, consumed at `Makefile:326`) and `make rls-check` passes the same `$(DB_URL)`
(`Makefile:337`). Today both work only because the dev instance's container publishes `127.0.0.1:5432`
(`scripts/container.sh` `instance_info`, `SERVICE_PORTS` ~`container.sh:206`). Remove the container and
the whole DB-backed inner loop breaks: `make run`, `make migrate`, `make rls-check`, and every
`ORCHICON_TEST_DSN`-gated Go test.

The supervisor already knows how to run the services **without** the plane: `cmd/orchicon/container.go`
implements services-only mode (`ORCHICON_CONTAINER_SERVICES_ONLY=1`, read by `servicesOnlyFromEnv` at
`container.go:436`), which starts postgres→nats→telemetry and skips the plane child
(`container.go:119-125`). But that code path is written for a container: it runs `postgres`/`initdb`
via `setpriv` as uid 70 (`postgresProc` `container.go:640`, `initPostgres` `container.go:270`),
defaults its data dir to `/var/lib/orchicon` (`container.go:67`), and adds docker-bridge `pg_hba`
rules (`ensurePostgresHBATrust` `container.go:391`). There is **no host path** and no way to acquire
the service binaries when postgres/nats are not installed on the host.

## Decision

Add a **host services supervisor** as a first-class `orchicon services up|down|status` subcommand that
reuses the existing `supervisor`/`managedProc` machinery in `cmd/orchicon/container.go` through a new
`hostMode` branch, and wire it into the Makefile inner loop. Points decided (one line each):

- **DECISION:** reuse `container.go`'s supervisor rather than write a second one — it already models
  dependency-ordered start, readiness probes, restart-with-backoff, and group shutdown; a host branch is
  a small delta (`hostMode` fields + branch in `prepare`/`postgresProc`/`natsProc`).
- **DECISION:** a NEW `orchicon services` subcommand (not an env flag on `container`) — the host path has
  a different lifecycle (detached + PID file + `down` verb) and must be reachable without any Docker at
  all. The container path stays exactly as-is.
- **DECISION:** acquire service binaries into `$ORCHICON_DATA_DIR/services/bin/` on demand, SHA256-pinned
  (mirrors the `make tools` buf pattern at `Makefile:136-149`), so a host with no Postgres installed
  still works. PostgreSQL from the embedded-postgres (zonky) Linux tarball; NATS from its release tarball.
  This is the integration point with the sibling "binary-acquisition" task: `ensureServiceBinaries` is the
  single function that task can replace or call.
- **DECISION:** telemetry is **off by default** for host services (`ORCHICON_TELEMETRY=none`); only
  postgres + nats start unless the operator opts in. Rationale: a dev inner loop needs the DB and the bus,
  not a 384 MiB observability stack; `up` stays fast and the port set stays small.
- **DECISION:** `DB_URL` is **derived** from the instance's postgres port via a new pure
  `scripts/container.sh db-url <inst>` verb, not hand-maintained. One source of truth ⇒ two instances can
  never collide and `DB_URL` cannot drift from the port actually bound.
- **DECISION:** only `make run` gains a `services` prerequisite; `migrate`/`rls-check`/`test` do **not**.
  CI runs them after `container up` against the container's DB — forcing services there would double-bind
  `5432` and break CI. They keep consuming `$(DB_URL)`, which now resolves per instance.
- **DECISION:** `services up` is idempotent by **port probe**, not just PID file — if something already
  answers on the instance's postgres port, `up` reports "already served by X" and exits 0.
- **DECISION (revisitable):** host postgres runs as the **current user** from a private bindir and a
  private data dir (`$ORCHICON_DATA_DIR/services/postgres`), with `listen_addresses=127.0.0.1` — no
  `setpriv`, no uid 70, no `/var/lib/orchicon`. If a future task installs a system postgres service this
  branch can defer to it; today owning our own bindir is the only dependency-free path.

---

## File-level evidence (read this session)

| Change site | Path:line | What is there now |
|---|---|---|
| Plane run | `Makefile:187` | `run: fetch-tags fe-build ## Run the control plane from source` |
| DSN literal | `Makefile:77` | `DB_URL ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` |
| Migrate | `Makefile:324-326` | `migrate:` uses `$(DB_URL)` |
| RLS gate | `Makefile:336-337` | `rls-check:` → `scripts/check-rls.sh "$(DB_URL)"` |
| Test isolation | `Makefile:218` | `for v in ORCHICON_GUARD_POLICY … ORCHICON_SERVE_STATE_DIR; do unset "$$v"; done` |
| Services-only flag | `cmd/orchicon/container.go:63,436` | `servicesOnlyEnv` const + `servicesOnlyFromEnv()` |
| Services-only branch | `cmd/orchicon/container.go:119-125` | skips plane child, logs loudly |
| prepare | `cmd/orchicon/container.go:227-256` | data dirs, embedded configs, `initPostgres`, `ensurePostgresHBATrust` |
| initdb | `cmd/orchicon/container.go:270-289` | `setpriv --reuid=70 … initdb -U orchicon --auth=trust` |
| postgres proc | `cmd/orchicon/container.go:640-664` | `setpriv … postgres -D <dd> -p 5432 -c listen_addresses=…`, `createdb` postReady |
| nats proc | `cmd/orchicon/container.go:666-674` | `nats-server -js -sd <dd>/nats -m 8222` |
| supervisor/child types | `cmd/orchicon/container.go:143-208` | `managedProc`, `startAndWait`, `monitor`, `shutdown` |
| subcommand dispatch | `cmd/orchicon/main.go:52-76` | `case "serve" … case "restart"` |
| help rows | `cmd/orchicon/main.go:275-320` | hand list; `help_test.go` derives rows from the switch |
| Detach/stop pattern | `cmd/orchicon/serve.go:407-500` | `serveDetach` (PID file + `setProcAttrBackground`), `serveStop` (SIGTERM→wait→SIGKILL) |
| Port table (Go) | `cmd/orchicon/install_ports.go:162-188` | `installerPorts()` — `ORCHICON_POSTGRES_PORT` 5432/5433/5434 |
| Port default lookup | `cmd/orchicon/install_ports.go:190-196` | `defaultFor(instance)` (refuses unknown instance) |
| Port table (bash) | `scripts/container.sh:96-144` | `instance_info()` PG_PORT dev 5432 prod 5433 test 5434 |
| Service publishes | `scripts/container.sh:206-208` | `SERVICE_PORTS` loopback publishes |
| Host data dir | `scripts/container.sh:113` | `HOST_DATA_DIR="$HOME/.local/share/orchicon-dev"` |
| RLS fallback | `scripts/check-rls.sh:20,33-42` | `CONTAINER=orchicon-cnt-dev`; host psql OR `docker exec` |
| Ambient isolation | `internal/testfixtures/ambient.go` | `AmbientConfigEnv` (group switches only; opt-ins stay reachable) |
| Port doc | `ARCHITECTURE.md:1016-1030` | dev/prod published-port table |

## New files

1. **`cmd/orchicon/services.go`** — the host services supervisor entrypoint.
   - `func runServices(args []string) int` — verb dispatch `up|down|status` (default `status`).
   - `func newHostSupervisor(instance string) (*supervisor, error)` — resolves the instance's
     `$ORCHICON_DATA_DIR` (reuse `hostDataDirFor`, `cmd/orchicon/install.go:392`), the per-instance
     postgres/nats ports from `installerPorts()`/`defaultFor` (`install_ports.go:190`), and the
     acquired bindir; returns a `supervisor` with `hostMode=true`.
   - `func servicesUp(instance string) int` — **idempotence guard first**: if
     `tcpReady(ctx, "127.0.0.1:<pgPort>")` is true, print `already running (port <pgPort> answers)` and
     exit 0; else build the host supervisor, `prepare` (binaries + initdb as current user), `startAndWait`
     postgres then nats, then `monitor` under a detached process, writing `services-PIDFile`.
   - `func servicesDown(instance string) int` — read PID file, `SIGTERM` the **process group**
     (`syscall.Kill(-pgid, SIGTERM)`), wait `serveStopGrace` (`serve.go:467`), escalate to `SIGKILL`,
     then **verify the port is free** (`!tcpReady`) and delete the PID file only when confirmed gone
     (mirrors `serveStop`'s "report only what you verified" contract, `serve.go:474-500`).
   - `func servicesStatus(instance string) int` — PID file + port probe, prints pg/nats state and DSN.
   - `func servicesPIDFile(instance string) string` — `$ORCHICON_DATA_DIR/services/services.pid`.
   - `func servicesLogFile(instance string) string` — `$ORCHICON_DATA_DIR/services/services.log`.

2. **`cmd/orchicon/services_acquire.go`** — binary acquisition (the sibling-task interface).
   - `func ensureServiceBinaries(dataDir string) (binDir string, err error)` — returns
     `<dataDir>/services/bin`, downloading + SHA256-verifying pinned PostgreSQL (zonky
     embedded-postgres linux-x86_64/arm64 tarball) and NATS server tarball **only when the binary is not
     already present**; idempotent (a present, verified binary is a no-op). Prefers `psql`/`pg_ctl` from
     the host PATH when a system install already provides them (so an installed Postgres is used, not
     duplicated).
   - `func fetchVerified(url, sha256hex, dest string) error` — curl-equivalent (`http.Get` + `io.Copy` to
     a temp file + `crypto/sha256` compare + rename), the Go twin of `Makefile:145-147`.
   - `const postgresPinnedVersion`, `const natsPinnedVersion`, and their per-arch SHA256 tables.

3. **`cmd/orchicon/services_test.go`** — unit tests (no real services):
   - host postgres args use the instance's port and `listen_addresses=127.0.0.1` (no `setpriv`, no uid 70);
   - `servicesPIDFile`/`servicesLogFile` resolve under the instance data dir;
   - the idempotence guard returns "already running" when the port probe is stubbed true;
   - `ensureServiceBinaries` is a no-op when the binary exists.

4. **`scripts/tests/local-services/run.sh`** — offline harness (no Docker, no DB), matching the
   `scripts/tests/host-residency/run.sh` convention:
   - asserts `scripts/container.sh services-port dev postgres` = 5432, `prod` = 5433, `test` = 5434;
   - asserts `scripts/container.sh db-url prod` contains `:5433/` and `db-url dev` contains `:5432/`;
   - asserts the Makefile's `DB_URL` default no longer contains a bare `5432` literal (it is derived);
   - asserts `services`/`services-down` appear in `make help`.

## Modified files (with exact insertion points)

1. **`cmd/orchicon/container.go`** — add host mode to the existing supervisor.
   - `supervisor` struct (`container.go:143-172`): add `hostMode bool`, `pgPort int`, `natsPort int`,
     `natsMonPort int`, `binDir string`.
   - `prepare` (`container.go:227`): at the top, `if s.hostMode { return s.prepareHost() }`.
   - new `func (s *supervisor) prepareHost() error` — mkdir `<dataDir>/services/{postgres,nats}`;
     `ensureServiceBinaries`; `initdb` **as the current user** (`s.binDir/initdb -D <dd> -U orchicon
     --auth=trust -E UTF8`) when `PG_VERSION` is absent; write loopback-only `pg_hba` (initdb's default
     127.0.0.1/::1 rules already suffice on the host — no `ensurePostgresHBATrust`).
   - `postgresProc` (`container.go:640`): `if s.hostMode { return &managedProc{name:"postgres",
     command: filepath.Join(s.binDir,"postgres"), args: {"-D", dd, "-p", itoa(s.pgPort), "-c",
     "listen_addresses=127.0.0.1"}, ready: tcpReady "127.0.0.1:<pgPort>", postReady: createdb via
     s.binDir, restart:true} }`.
   - `natsProc` (`container.go:666`): host branch uses `s.natsPort`/`s.natsMonPort` and `s.binDir`.
   - `postgresListenAddr` (`container.go:444`): return `"127.0.0.1"` when `hostMode` (loopback; no
     published port to widen for).

2. **`cmd/orchicon/main.go`** — wire the subcommand.
   - dispatch (`main.go:57`): insert `case "services": os.Exit(runServices(os.Args[2:]))`.
   - `printHelp` rows (`main.go:288-310`): add a row in the "Setting up and driving an instance:" section:
     `{cmd: bin + " services", desc: []string{"Start/stop local Postgres + NATS on the HOST (no container)",
     "services up | down | status — the inner-loop backend for `make run`"}}`. `help_test.go` derives the
     expectation from the switch, so this row is mandatory and the test enforces it.

3. **`scripts/container.sh`** — add two pure verbs (no docker, echo only), in the `case "$cmd"` block near
   `shape`/`plane-bind` (`container.sh:1670-1690`), reusing `instance_info`:
   - `services-port <inst> <svc>` → echo the resolved port for `postgres|nats|nats-mon`.
   - `db-url <inst>` → `instance_info`; echo `postgres://orchicon:orchicon@localhost:$PG_PORT/orchicon?sslmode=disable`.
   Both honour `ORCHICON_POSTGRES_PORT` overrides automatically (applied at `container.sh:179-196`).

4. **`Makefile`** — derive DSN + add the services targets.
   - add near the top (before line 77): `instance ?= dev`.
   - replace `Makefile:77` with:
     `DB_URL ?= $(shell scripts/container.sh db-url $(instance) 2>/dev/null || echo postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable)`.
   - new targets in the Database section (after `Makefile:337`):
     ```
     .PHONY: services services-down services-status
     services: ## Start local Postgres + NATS for $(instance) on the host (no container engine)
     	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services up $(instance)
     services-down: ## Stop the local Postgres + NATS services for $(instance)
     	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services down $(instance)
     services-status: ## Show local services state + the DSN for $(instance)
     	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services status $(instance)
     ```
   - `run` (`Makefile:187`): `run: services fetch-tags fe-build ## Run the control plane from source (starts local services first)`.
   - `test` (`Makefile:218`): add a one-line comment that the services are **not** started here and
     `ORCHICON_SERVICES_*` is deliberately not in the unset list; no behavioral change (opt-ins stay
     reachable). **Do not** add a `services` prereq to `migrate`/`rls-check`/`test`.

5. **`scripts/check-rls.sh`** — add a third reachability fallback.
   - `run_sql` (`check-rls.sh:33-42`): after the host-`psql` branch, try the acquired bindir:
     `if [ -x "${ORCHICON_PG_BINDIR:-$HOME/.local/share/orchicon-${ORCHICON_INSTANCE:-dev}/services/bin}/psql" ];
     then <that psql> "$URL" ...` before the `docker exec` branch. `make` passes `ORCHICON_PG_BINDIR` in
     via the `rls-check` recipe (`scripts/check-rls.sh "$(DB_URL)"` with the env exported by `services`).

6. **`ARCHITECTURE.md`** — add a short subsection under §Host residency (~`ARCHITECTURE.md:1044`)
   documenting `orchicon services up|down|status`, the derived `DB_URL`, and that telemetry is off by
   default for the host services.

## Acceptance-criteria → mechanism map

| AC | Mechanism |
|---|---|
| 1 no engine/no Postgres → `make run` | `ensureServiceBinaries` acquires postgres+nats; `run: services` starts them |
| 2 `make migrate` + `make rls-check` against local PG | derived `DB_URL`; `check-rls.sh` bindir psql fallback |
| 3 DB-backed tests pass; `ORCHICON_TEST_DSN` still works | tests are untouched; `ORCHICON_TEST_DSN` already outside `AmbientConfigEnv` (`ambient.go`) |
| 4 per-instance ports, no collision | single port table (`install_ports.go:162`, `container.sh:96`); `db-url` derives |
| 5 idempotent start | port-probe guard in `servicesUp` before any start |
| 6 no orphaned processes | group SIGTERM→wait→SIGKILL in `servicesDown`, verified by port re-probe |
| 7 test isolation unbroken | `Makefile:218` unchanged; services env passed only to the child |
| 8 `make help` rows | `##` comments on the three new targets; `scripts/tests/local-services/run.sh` asserts them |

---

## Numbered implementation list

1. **Create `cmd/orchicon/services_acquire.go`** with `ensureServiceBinaries(dataDir) (string, error)`,
   `fetchVerified(url, sha, dest)`, and pinned-version/SHA tables for PostgreSQL (zonky embedded
   linux-x86_64 + arm64) and NATS. Prefer a host `psql`/`pg_ctl` on PATH; else download into
   `<dataDir>/services/bin`, verify SHA256, `os.Rename` into place. Idempotent when the binary exists.
2. **Extend `cmd/orchicon/container.go`**: add fields `hostMode/pgPort/natsPort/natsMonPort/binDir` to
   `supervisor` (struct at `container.go:143`); add `prepareHost()`; branch `prepare` (`container.go:227`)
   into it; add host branches to `postgresProc` (`container.go:640`) and `natsProc` (`container.go:666`);
   make `postgresListenAddr` (`container.go:444`) return `127.0.0.1` in host mode.
3. **Create `cmd/orchicon/services.go`**: `runServices`, `newHostSupervisor`, `servicesUp` (port-probe
   idempotence guard → prepare → startAndWait pg → startAndWait nats → detach + PID file → monitor),
   `servicesDown` (group SIGTERM → wait `serveStopGrace` → SIGKILL → verify port free → rm PID file),
   `servicesStatus`, `servicesPIDFile`, `servicesLogFile`. Reuse `setProcAttrBackground`
   (`procattr_unix.go`) and `serveStopGrace` (`serve.go:467`). Telemetry: skip unless
   `ORCHICON_TELEMETRY=embedded`.
4. **Wire dispatch + help in `cmd/orchicon/main.go`**: add `case "services": os.Exit(runServices(os.Args[2:]))`
   after `case "restart"` (`main.go:76`); add the `services` help row (`main.go:288`) so `help_test.go`
   passes.
5. **Add `scripts/container.sh` verbs** `services-port <inst> <svc>` and `db-url <inst>` in the
   `case "$cmd"` block; both call `instance_info` and echo — no docker.
6. **Edit `Makefile`**: add `instance ?= dev`; replace `DB_URL` (line 77) with the `db-url`-derived form;
   add `services`/`services-down`/`services-status` targets with `##` comments; add `services` as a
   prerequisite of `run` (line 187) only.
7. **Edit `scripts/check-rls.sh`**: insert the bindir-`psql` fallback into `run_sql` (`check-rls.sh:33`)
   before the `docker exec` branch, keyed off `ORCHICON_PG_BINDIR` (default under the instance data dir).
8. **Create `scripts/tests/local-services/run.sh`** offline harness asserting the two new verbs' per-instance
   values, the derived `DB_URL` (no bare `5432`), and the `make help` rows; wire it as
   `local-services-test` and add it to the `ci-go` prerequisite list (`Makefile:729`).
9. **Create `cmd/orchicon/services_test.go`** with the four unit tests listed under New files.
10. **Document in `ARCHITECTURE.md`** (new subsection near line 1044) and add a `local-services-test`
    bullet to the CI-gate prose.
