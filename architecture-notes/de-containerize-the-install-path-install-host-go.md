# De-containerize the install path (`install_host.go`)

Branch: `de-containerize-the-install-path-install-host-go-np1my88kwsmhmy8c` (base = develop `f5f41ec454`).
All `path:line` references were read in this session against that base.

## 0. Context and the one scope decision

`cmd/orchicon/install_host.go` implements the HOST-resident plane but still *asks a container* what shape
an instance is (`containerResidency`), *waits on a container* for postgres/nats (`servicesReady`), *copies a
docker volume* (`migrateHostDataDir`), *inspects the docker bridge* (`bridgeIP`), and *overrides an image's
healthcheck* (`healthArgs`). Every one is either dead or wrong once the instance container stops being the
carrier, and a `docker inspect`/`docker version` in an install path is exactly the thing that fails silently
on a host with no engine.

**DECISION (revisitable) — how to read AC1 "no `docker` invocation remaining anywhere in `cmd/orchicon/**`".**
It cannot mean "delete the container shape": AC4/AC5/AC6/AC7 explicitly keep an *with-an-engine* install and
"runtime containers in use", and the epic's own sibling branch
(`origin/supervisor-residency-aware-service-manager-container-host-wfbbs8k9nrtmx9nf`) keeps container
residency as a first-class shape. So AC1 is read as: **the two named functions are gone, and NO `docker`
invocation is reachable on the host/no-engine path — a host with no engine never runs `docker` at all.** The
container-ENGINE path keeps its (now explicitly gated) `docker run`/`docker pull`. A test asserts the gate
(step 9). If a reviewer insists on the literal reading, that conflicts with AC7 and must be raised, not
silently resolved by deleting the container shape.

**DECISION (revisitable) — foundation dependency.** The coherent no-engine shape ("services run as HOST
processes") is provided by the epic's foundation PRs, which are on `origin` but NOT in develop:
- `origin/supervisor-residency-aware-service-manager-container-host-wfbbs8k9nrtmx9nf` — adds
  `cmd/orchicon/residency.go` (service residency: `ORCHICON_SERVICE_RESIDENCY`, `servicePortsFor`,
  `supervisor.isHost`, `asUser`, `defaultDataDirFor`) and makes `container.go` residency-aware (#719).
- `#720` infra-binary acquisition (`acquireInfraBinaries`, `loadInfraBinaries`, `infra-binaries.json`),
  `#721` per-platform Postgres privilege model (`ensureHostPostgresPrivileges`, `privilege_*.go`),
  `#722` host telemetry configs (`deploy/configs/`).

The install path reuses the `container.go` supervisor for host services (section 3), so those are hard
prerequisites. **Rebase this branch onto `origin/supervisor-residency-aware-service-manager-container-host-*`
(plus its #720–#722 ancestors) before implementing**; step 0 of the numbered list checks this. If that rebase
is not available, implement sections 2, 4, 5, 6, 7 in full (they are self-contained) and leave step
`install.go` §4b host-services start behind a single seam (section 4, item 5) that returns a clear
"no host-services provider" error — never a `docker` call.

### Ground truth — every container dependency, read this session

| # | Site | What it does today | Container-free replacement |
|---|------|--------------------|----------------------------|
| 1 | `install_host.go:46-58` `containerResidency` | `docker inspect` env for `ORCHICON_CONTAINER_SERVICES_ONLY=1`; returns `(residency, ok)`, ok=false ⇒ "leave it alone" | record the shape in the instance state dir; read it back (`recordedResidency`) |
| 2 | `install_host.go:118-127` `healthArgs` | overrides the IMAGE `:8080` healthcheck for host residency | DELETE (no image in host shape) |
| 3 | `install_host.go:285-289` `servicesReady` | `docker exec <name> pg_isready …` + `docker exec … curl …8222/healthz` | direct `pg_isready` on the RESOLVED binary + direct HTTP probe |
| 4 | `install_host.go:268-281` `waitServicesReady` | announce → 60×1s → warn → return bool | same posture, port-driven, plus a no-host-endpoint "not probeable ⇒ true" arm |
| 5 | `install_host.go:225-256` `migrateHostDataDir` | `docker volume inspect` + `docker run --rm alpine cp -a` | `ensureHostDataDir`: KEK guard only, no engine |
| 6a | `install_host.go:75-100` `bridgeIP` | `docker network inspect bridge` → `docker0`; HARD error | soft: `("", nil)` when no bridge/engine; keep the no-`0.0.0.0` refusal for the container case |
| 6b | `install_host.go:136-182` `planeEnv` | `ORCHICON_HTTP_EXTRA_BIND` + `ORCHICON_PLANE_PUBLIC_URL` from bridge, unconditionally | one `planeBindEnv(ports)` call; present only when runtime containers are in use |
| 7 | `install.go:578` (calls `containerResidency`), `install.go:636` (`healthArgs`), `install.go:266` (`docker restart` repair), `install.go:54` (`docker version`), `install.go:605,670,690,753,792,796,806,814`, `install_ports.go:332`, `restart.go:51,77` | container instance / images / port read-back | gate on `containerEngineAvailable()`; no-engine path uses host services + recorded residency |
| 8 | `scripts/tests/host-residency/run.sh` (Makefile `host-residency-test`, `Makefile:410-412`) | extracts `container_residency_from_env`, `bridge_ip`, `bridge_bind_env`, `plane_env` from `scripts/container.sh` | repoint at the Go implementation |

## 1. New files

- **`cmd/orchicon/residency.go`** *(from foundation PR #719; add it here if the rebase is unavailable)*.
  Contents: `const serviceResidencyEnv = "ORCHICON_SERVICE_RESIDENCY"`; `serviceResidencyFromEnv()` (only
  `"host"` ⇒ host, everything else container — the zero value stays container so no existing call site/test
  changes behaviour); `func (s *supervisor) isHost() bool`; `func (s *supervisor) asUser(uid,gid int, command
  string, args []string) (string, []string)` — the ONE launch seam, emits `setpriv` only for non-host,
  non-root; `servicePorts` struct + `servicePortDefaults()` + `servicePortsFor(residency)`; and
  `defaultDataDirFor(residency)`.
  **Why shared**: `container.go`'s supervisor ALREADY implements exactly the "run postgres/nats/telemetry as
  direct child processes" shape (`container.go:88` `runContainer`, `container.go:725-807` proc defs). Writing a
  second supervisor in `install.go` would be a second implementation of the same thing.

- **`cmd/orchicon/install_host_plane.go`** *(optional split; see step 6)* — move `planeDrift`,
  `servingPlanePID`, `stopHostPlane`, `servingPlaneAlive`, `startHostPlane` (currently
  `install_host.go:310-482`) out of the file so `install_host.go` is only the de-containerized surface. Both
  functions are already docker-free and need no change; a split is cosmetic and may be skipped.

## 2. `cmd/orchicon/install_host.go` — the core deliverable

Exact edits (paths refer to the read state on `f5f41ec454`):

1. **DELETE `containerResidency`** (`install_host.go:34-58`). **ADD**, immediately after `planeDriftState`'s
   declaration or at the top of the file:
   ```go
   const residencyRecordFile = "residency"
   func residencyRecordPath(instance string) (string, error)        // <hostDataDir>/residency
   func recordedResidency(instance string) (string, bool)           // reads it; ok=false ⇒ "leave it alone"
   func recordResidency(instance, residency string)                 // best-effort write, mirrors writeResolvedPorts
   ```
   `recordedResidency` returns `ok=false` for absent/empty/unrecognised content and **surfaces no error** —
   this is the direct successor of the old contract ("`the caller must NOT treat an unreadable shape as a
   mismatch`", `install_host.go:43-45`). `recordResidency` is best-effort (warn + return), exactly like
   `writeResolvedPorts` (`install.go:333-353`).

2. **DELETE `healthArgs`** (`install_host.go:102-127`) and its call site (`install.go:633-636`). The image's
   `:8080` HEALTHCHECK is only wrong because a services-only container runs no plane; with the container gone
   from the host path there is no healthcheck to override. `scripts/container.sh` keeps its own HEALTH_ARGS for
   the container-engine shape.

3. **ADD** the engine probe next to `planeEnv`:
   ```go
   const runtimeContainersEnv = "ORCHICON_RUNTIME_CONTAINERS"
   func containerEngineAvailable() bool { _, err := exec.LookPath("docker"); return err == nil }
   func runtimeContainersInUse() bool
   ```
   `containerEngineAvailable` is a **PATH probe, never `docker version`** — absence must be answerable with no
   daemon. `runtimeContainersInUse` order: (1) `ORCHICON_RUNTIME_CONTAINERS` (`0|off|no|false|none` ⇒ false,
   anything else ⇒ true); (2) non-empty `ORCHICON_DOCKER_BRIDGE_IP` ⇒ true (a pinned bridge is proof of
   intent); (3) else `containerEngineAvailable()`.

4. **REWRITE `bridgeIP`** (`install_host.go:75-100`) to return `("", nil)` for the no-bridge/no-engine case
   instead of a hard error. Keep, verbatim, the refusal to fall back to `0.0.0.0`
   (`install_host.go:98-99`) for the case where a bridge IS expected. Add
   ```go
   func planeBindEnv(ports map[string]int) (bind, url string, err error)
   ```
   — the ONE place both values are computed; returns `("","",nil)` when `!runtimeContainersInUse()`, otherwise
   `bridgeIP()` then `fmt.Sprintf("%s:%d", ip, ports["ORCHICON_CONTROL_PORT"])` for `bind` and
   `"http://"+bind` for `url`. An unresolvable bridge while containers ARE in use stays an error (a plane its
   workers cannot reach is worse than one that refuses to start — `install_host.go:453-456`).

5. **REWRITE `planeEnv`** (`install_host.go:136-182`): replace the `bridgeIP()` call (line 137) with
   `bind, url, err := planeBindEnv(ports)`; delete the two unconditional `ORCHICON_HTTP_EXTRA_BIND` /
   `ORCHICON_PLANE_PUBLIC_URL` entries (lines 153-154) and **append them only when `bind != ""`** at the end
   of the function. `ORCHICON_HTTP_ADDR=127.0.0.1:<control>` stays unconditional (the loopback primary bind is
   load-bearing, lines 144-150). Single-source-of-truth and per-instance-ness are preserved because both
   values come from the ONE `planeBindEnv` call, on the same control port.

6. **REPLACE `migrateHostDataDir`** (`install_host.go:214-256`) with
   ```go
   func ensureHostDataDir(instance, hostDataDir string, out io.Writer) error
   func hostDataDirInstanceState(hostDataDir string) []string   // {"blobs","serve","postgres","config"}
   ```
   Rules, all preserved from the current doc-comment (`install_host.go:217-220`) plus one addition:
   - an existing `<dataDir>/secrets/kek` ⇒ print "leaving it untouched", return nil (never overwrite);
   - `ORCHICON_SECRETS_KEK` non-empty ⇒ print "supplied by ORCHICON_SECRETS_KEK", return nil
     (**an operator-managed KEK is not a lost KEK** — `secretcrypto.ResolveKEK` returns the override verbatim
     and never writes the file; without this arm the guard would refuse a healthy env-KEK re-run);
   - `MkdirAll(<dataDir>/secrets, 0o700)`;
   - state present (`hostDataDirInstanceState` non-empty) with **no** KEK ⇒ **FATAL** `error` naming the KEK
     path and "ORPHANS EVERY TENANT SECRET" (the current failure is fatal, `install.go:201-203` — keep it);
   - `ports.json` / `infra-binaries.json` are deliberately NOT in `hostDataDirInstanceState` (install.go
     writes both earlier in the same run — `install.go:117` — counting them would refuse every fresh install);
   - drop the `volume string` parameter (no container-era volume source).

7. **REPLACE `waitServicesReady`/`servicesReady`** (`install_host.go:258-289`):
   ```go
   func waitServicesReady(instance string, ports map[string]int, out io.Writer) bool
   func waitServicesReadyFor(instance string, ports map[string]int, tries int, sleep time.Duration, out io.Writer) bool
   func servicesReady(instance string, ports map[string]int) bool
   func servicesProbeableFromHost(ports map[string]int) bool
   func pgIsReadyBin(instance string) string
   ```
   - `servicesReady`: `exec.Command(pgIsReadyBin(instance), "-h", "127.0.0.1", "-p",
     strconv.Itoa(ports["ORCHICON_POSTGRES_PORT"]), "-U", "orchicon").Run() == nil` **and**
     `httpHealthy(fmt.Sprintf("http://127.0.0.1:%d/healthz", ports["ORCHICON_NATS_MONITOR_PORT"]), 3*time.Second)`.
   - **Fail-safe arm (a real bug the old boolean hides):** for a container-RESIDENT plane the service ports
     are NOT in the resolved set (`install_ports.go` gives them `ContainerBind: bindNone`), so
     `servicesProbeableFromHost` is false and `servicesReady` must return **true**, not false. Returning false
     would make the repair gate (`install.go:257`) fire on every healthy container-resident re-run.
   - `waitServicesReadyFor` announces first (a silent minute reads as a hang, `install_host.go:269-270`), with
     NO wait at all when `!servicesProbeableFromHost` (announcing a wait on port 0 is a wait on nothing);
     times out with a **warning** and returns **false** (unchanged posture).
   - `pgIsReadyBin`: prefer the `pg_isready` recorded in the instance's infra-binary record
     (`loadInfraBinaries(hostDataDirFor(instance))`, foundation PR #720); fall back to `"pg_isready"` on PATH.

## 3. `cmd/orchicon/container.go` — residency-aware supervisor (foundation; only if not already present)

`runContainer` (`container.go:88`) sets `servicesOnly := servicesOnlyFromEnv() ||
serviceResidencyFromEnv() == residencyHost`; replace the direct `setpriv` argv construction in
`postgresProc` (`container.go:718-733`) and `planeProc` (`container.go:813-880`) with the `asUser` seam;
resolve ports via `servicePortsFor(residency)` and template the embedded configs from it; use
`defaultDataDirFor(residency)`. Container residency must remain **byte-for-byte** today's behaviour (its zero
value is container). This is PR #719's content — rebase rather than re-implement where possible.

## 4. `cmd/orchicon/install.go` — the caller

1. `install.go:52-56` — replace the unconditional `docker version` with
   `containersUsed := containerEngineAvailable()`; run `docker version` **only if** `containersUsed`
   (preserve the "installed but not answering" error); else print the one-line "services will run as HOST
   processes; runtime containers unavailable" notice.
2. after `residencyForInstall()` (`install.go:82-85`) — if `!containersUsed && residency == residencyContainer`,
   **warn and coerce to `residencyHost`** (loud, never fatal).
3. `install.go:119-168` — wrap the whole image-ensure block in `if containersUsed { … } else { announce
   "skipping the image pull — no container engine" }`.
4. `install.go:194-204` — call `ensureHostDataDir(instance, hostDir, os.Stdout)` (drops the `dataVolume` arg).
5. `install.go:206-221` — split by carrier:
   - `containersUsed`: keep `ensureInstallContainer(...)` (with its `healthArgs` call removed at
     `install.go:636`) and the container wait;
   - `!containersUsed`: **ADD `startHostServices(instance, ports, hostDataDir, out)`** — re-execs THIS binary
     as `container` with `ORCHICON_SERVICE_RESIDENCY=host`, `ORCHICON_INSTANCE`, `ORCHICON_DATA_DIR`, and the
     twelve `hostServicesPortEnvs`; strips `ORCHICON_CONTAINER_MODE`/`servicesOnlyEnv`; idempotent (returns
     early if `servicesReady` already true); records `<dataDir>/serve/services.pid`.
   Then `waitServicesReady(instance, ports, os.Stdout)` and `startHostPlane(...)` for both.
6. `install.go:257-278` — repair pass: gate the `docker restart` arm behind `containersUsed`; for the host
   carrier restart the host plane (existing `stopHostPlane`/`startHostPlane`) and, if host services are used,
   re-run the host-services start.
7. `install.go:568-675` `ensureInstallContainer` — remove the `containerResidency` call (line 578) and the
   shape-mismatch branch: replace with `recordedResidency(instance)` (ok=false ⇒ treat as matching, as
   today's comment at `install.go:575-577` requires), and record the shape with `recordResidency(instance,
   residency)`. Remove the `healthArgs(residency)` append (line 636).
8. `install.go:830-999` `printInstallInfo` (and the run's tail) — conditionally print the host-vs-container
   management lines and the `docker stop/start/logs` hints only when `containersUsed`; add
   `recordResidency(instance, residency)` on the success path (after `writeResolvedPorts`, `install.go:117`).
9. `imagePresent`/`ensureImage`/`pullImage`/`containerImageCurrent`/`containerRunning`/`containerExists`
   (`install.go:688-820`) — unchanged, but now reachable only through the `containersUsed` gate.

## 5. `cmd/orchicon/install_ports.go`

- `runningInstancePorts` (`install_ports.go:330-337`, the `docker inspect` at 332) — call it only under the
  `containerRunning` guard already present at `install_ports.go:529`; add the `containersUsed` guard at the
  `resolveInstallPorts` entry so a no-engine install never reaches `docker`.
- ADD three per-instance rows to `installerPorts()` (`install_ports.go:130-150`):
  `ORCHICON_TEMPO_OTLP_GRPC_PORT` (14317/14318/14319), `ORCHICON_TEMPO_OTLP_HTTP_PORT`
  (14318/14319/14320), `ORCHICON_OTEL_HEALTH_PORT` (13133/13134/13135), all `bindNone`, all
  `ContainerPort` = the container literal. These give host service residency a per-instance offset so two
  instances do not collide, and `TestInstallPortDefaultsMatchContainerScript` (`install_ports_test.go`) must be
  extended with the three rows (its `wantDefaults` map is the gate).

## 6. `cmd/orchicon/restart.go`

- `restart.go:51` `containerResidency(name)` → `recordedResidency(instance)`; update the two error-message
  lines (`restart.go:52-54`) from "no container named…" to "no install record for…". `restart.go:77`'s
  `docker restart` is then reachable only for a RECORDED container shape.

## 7. `cmd/orchicon/serve_state.go`

No change required: `serveStateDirEnv` (`serve_state.go:28`), `serveStateDir()` and `servePIDFile()`
(`serve_state.go:31-51`) are already docker-free and per-instance; `planeDrift`/`servingPlanePID` keep
reading `<dataDir>/serve/pids/orchicon.pid`, which `planeEnv` still sets via `ORCHICON_SERVE_STATE_DIR`.

## 8. Wiring

| New piece | Registered at |
|-----------|---------------|
| `recordedResidency` / `recordResidency` | `install.go` after `writeResolvedPorts` (write) and in `ensureInstallContainer` + `restart.go` (read) |
| `containerEngineAvailable` / `runtimeContainersInUse` | `install.go:52` gate; `planeBindEnv`; `resolveInstallPorts` |
| `planeBindEnv` | `planeEnv` (`install_host.go:137`) |
| `ensureHostDataDir` | `install.go:201` |
| `servicesReady(instance,ports)` | `install.go:217,257,268`; `startHostServices` |
| `startHostServices` | `install.go` §4b no-engine branch; repair pass |
| `residency.go` (`serviceResidencyFromEnv`,`asUser`,`servicePortsFor`) | `container.go:88` `runContainer`; `postgresProc`/`planeProc` |
| host-residency harness | `Makefile:410` `host-residency-test` |

## 9. Tests (AC7: no-engine install, with-engine install, KEK present/absent)

- `cmd/orchicon/install_host_test.go`
  - DELETE `TestHealthArgsOverrideTheImageProbeForHostResidency` (218-238) and
    `TestHealthArgsMatchContainerScript` (240-262).
  - REPLACE `TestPlaneEnvIsPerInstanceAndComplete` (143-190) / `TestPlaneEnvRefusesWithoutABridge` (192-215)
    with: (a) with `ORCHICON_RUNTIME_CONTAINERS=1` + pinned bridge ⇒ `EXTRA_BIND`/`PUBLIC_URL` present and on
    the resolved port; (b) with `ORCHICON_RUNTIME_CONTAINERS=0` ⇒ both **absent** and `planeEnv` returns **no
    error**; (c) never a wildcard.
  - REWRITE `TestMigrateHostDataDirSkipsWhenKekExists` (320-342) as `ensureHostDataDir` KEK-present (untouched,
    no error) / KEK-absent-with-state (FATAL) / KEK-absent-fresh (create `secrets/`, no error) /
    `ORCHICON_SECRETS_KEK` set (no-op, no file).
  - ADD `TestRecordedResidencyRoundTrip` (record → read `ok=true`; missing/garbage file ⇒ `ok=false`, no
    error) and `TestServicesReadyBoolean` using a stub `pg_isready` on `PATH` (`t.Setenv("PATH", …)`) plus an
    `httptest` server on the NATS-monitor port; and `TestWaitServicesReadyAnnouncesAndWarns` via
    `waitServicesReadyFor(inst, ports, 1, time.Millisecond, out)` asserting the announce line, the `warning:`
    line, and `return false`.
- `cmd/orchicon/install_converge_test.go` — extend with an install re-run convergence check for the recorded
  shape.
- `cmd/orchicon/ports_record_test.go` / `resolved_ports_test.go` — unaffected (assert on `install.go` text:
  `ensureImage`/`pullImage` present, `falling back to` present — keep both strings).
- NEW `cmd/orchicon/install_no_engine_test.go` (AC4/AC7): with `PATH` scrubbed of `docker` and
  `ORCHICON_RUNTIME_CONTAINERS=0`, assert `containerEngineAvailable()==false`,
  `runtimeContainersInUse()==false`, `planeBindEnv` returns `("","",nil)`, and `planeEnv` carries no
  `0.0.0.0`/`EXTRA_BIND`. Body must be pure-function calls — no `runInstall` (it pulls images/starts daemons).
- The "with an engine" shape is pinned by extending `install_ports_test.go`'s `wantDefaults` gate with the
  three new rows.
- `scripts/tests/host-residency/run.sh` — repoint the `container_residency_from_env` / `bridge_bind_env` /
  `plane_env` assertions at the Go implementation: keep the file's shape assertions but source them from
  `go test ./cmd/orchicon -run TestPlaneEnv|TestRecordedResidency|TestServicesReady`, or add a tiny
  `cmd/orchicon` test-only `shape` dump invoked by the harness. Run via `make host-residency-test`
  (`Makefile:410-412`).

## 10. Numbered implementation list (mechanically executable)

1. **Confirm the base.** `git merge-base --is-ancestor origin/supervisor-residency-aware-service-manager-container-host-wfbbs8k9nrtmx9nf HEAD` — if false, `git rebase origin/supervisor-residency-aware-service-manager-container-host-wfbbs8k9nrtmx9nf` (brings `residency.go`, `container.go` host mode, infra-binary acquisition, privilege model, `deploy/configs/`). If the rebase is impossible, skip steps 3 and 12 and stop at a compiling build with the seam in step 12 erroring clearly.
2. `cmd/orchicon/install_host.go`: delete `containerResidency` (34-58) and `healthArgs` (102-127); add `residencyRecordFile`, `residencyRecordPath`, `recordedResidency`, `recordResidency` per §2.1.
3. `cmd/orchicon/container.go`: make `runContainer` residency-aware (`servicesOnly || serviceResidencyFromEnv()==residencyHost`), route `postgresProc`/`planeProc` through `asUser`, resolve ports through `servicePortsFor`; add `cmd/orchicon/residency.go` if the rebase did not (contents per §1). No behaviour change for container residency.
4. `cmd/orchicon/install_host.go`: add `runtimeContainersEnv`, `containerEngineAvailable`, `runtimeContainersInUse` (§2.3); rewrite `bridgeIP` to soft-fail (§2.4) and add `planeBindEnv`; make `planeEnv` append `EXTRA_BIND`/`PUBLIC_URL` only when `bind != ""` (§2.5).
5. `cmd/orchicon/install_host.go`: replace `migrateHostDataDir` with `ensureHostDataDir` + `hostDataDirInstanceState` (§2.6).
6. `cmd/orchicon/install_host.go`: replace `servicesReady`/`waitServicesReady` with the port-driven pair + `servicesProbeableFromHost` + `pgIsReadyBin` (§2.7).
7. `cmd/orchicon/install.go`: gate the engine check (§4.1), coerce residency without an engine (§4.2), gate the image block (§4.3), call `ensureHostDataDir` (§4.4), split §4b into container vs host-services carriers and add `startHostServices` (§4.5), gate the repair `docker restart` (§4.6), remove the `containerResidency`/`healthArgs` uses and record the shape (§4.7), gate the management-print lines and record residency on success (§4.8).
8. `cmd/orchicon/install_ports.go`: add the `containersUsed` guard around `runningInstancePorts`; add the three new port rows; extend `install_ports_test.go`'s `wantDefaults`.
9. `cmd/orchicon/restart.go`: `containerResidency(name)` → `recordedResidency(instance)`; update messages.
10. Tests: apply all of §9.
11. `scripts/tests/host-residency/run.sh`: repoint at the Go implementation; run `make host-residency-test`.
12. Build + gate: `go build ./... && go test ./cmd/orchicon/... && make host-residency-test && make install-path-test`.
13. Confirm AC1 mechanically: `grep -rn 'exec.Command("docker"' cmd/orchicon/` — every remaining hit must be inside a `containersUsed`/`containerEngineAvailable()`/`containerRunning`-guarded branch; add `TestNoDockerOnTheHostPath` asserting `containerEngineAvailable()==false` ⇒ `planeBindEnv`/`planeEnv`/`servicesReady`/`ensureHostDataDir` never shell `docker` (assert via a `PATH` with a `docker` stub that fails loudly).

## 11. Risks

- **Rebase dependency** (step 1) is the biggest risk; without it, host services have no supervisor.
- `TestInstallPortDefaultsMatchContainerScript` will fail until `scripts/container.sh` carries the three new
  tempo/OTel port defaults; add them there or the gate blocks.
- `install_path_test.go` / `scripts/tests/install-path/*` stub `docker`; the no-engine branch adds a new path
  they must exercise.
