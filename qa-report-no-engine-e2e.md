# QA report — no-container-engine developer session (acceptance)

Status: **RUN PARTIALLY BLOCKED by the QA runtime's disk budget — defects D1/D2/D3/D4 observed
live and filed; steps requiring a cold build could not execute.** Not a template any more: every
statement below is copied from an evidence file in `qa-evidence/no-engine-e2e/`.

Procedure: [`docs/no-engine-e2e-acceptance.md`](docs/no-engine-e2e-acceptance.md).
Harness: [`scripts/no-engine-e2e.sh`](scripts/no-engine-e2e.sh).

Evidence directories for this QA pass:

| dir | what it is |
|---|---|
| `qa-evidence/no-engine-e2e/20261017T053116Z/` | harness run #1 — **INVALID**: the cold build hit `ENOSPC`, so its FAILs are environment cascade, *not* defects |
| `qa-evidence/no-engine-e2e/run2/` | harness run #2 (GOCACHE/GOTMPDIR relocated to `/tmp`) — same `ENOSPC` on `go build` |
| `qa-evidence/no-engine-e2e/observed-no-engine/` | **the decisive evidence**: the no-engine defects reproduced directly, with no build in the way |

## 1. Verdict

| AC | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Every documented step works on a clean checkout with no container engine | **BLOCKED** — the QA container cannot build (2×2 GB tmpfs, see §2); not a verdict on the feature | `20261017T053116Z/step-1.log`, `run2/step-1.log` |
| 2 | `make run` against locally-started services; `make migrate`/`make rls-check` against them | **PARTIAL** — `make services` proven engine-free; migrate/rls unreachable (no binaries) | `20261017T053116Z/step-1.log` (`dev services ready`), `/proc/net/tcp` check |
| 3 | A code change is verifiably live after the rebuild path; stale-`dist` mode excluded | **UNPROVEN** — `make build` never succeeded here | `run2/step-1.log` (`ENOSPC`) |
| 4 | Status reports real state and never claims unverified health | **FAIL (D2)** | `observed-no-engine/A-status.txt` |
| 5 | Logs reachable through the documented command | **FAIL (D1)** | `observed-no-engine/B-logs.txt` (rc=127, `docker: command not found`) |
| 6 | Two instances coexist, independently controllable | **UNPROVEN** — needs two planes | — |
| 7 | Cleanup prunes only Orchicon's artifacts, safe no-op otherwise | **FAIL (D4)** | `observed-no-engine/C-clean-docker.txt` (rc=2 twice) |
| 8 | Stopping leaves no orphaned service process | **UNPROVEN** — `services-down` needs a built binary | — |
| 9 | No container command / no source reading needed to know what is running | **FAIL (D1+D2+D3)** — `status` prints two `docker: command not found` lines and `not created`; `logs` exits 127 | `observed-no-engine/` |
| 10 | Evidence attached | **PARTIAL** — everything above is attached; steps 3-9 have no evidence | this report |
| 11 | Failures filed against the owning sibling, never patched here | **MET** — D1/D2/D3/D4 filed in §4; no production file touched by this step | `git status` clean outside `qa-evidence/` |

## 2. Environment (why runs #1 and #2 are not evidence)

- Runs at `2026-10-17T05:31Z` and `2026-10-17T05:32Z`, host Linux `9ee83146c36a`
  (7.2.0-1-cachyos), HEAD `1a2a09392`, branch
  `e2e-proof-full-developer-session-with-no-container-engine-never-blind-j7bwx5hcmvrjhd4c`.
- **No container engine — the validity precondition holds:**
  `command -v docker podman nerdctl docker-compose` printed nothing, and
  `20261017T053116Z/step-0.log` records
  `engines: none found (GOOD — this is the claim being proven)`.
- Ports: the in-container sandbox plane holds 5432/4222/8080, so both runs used the documented
  overrides — run #1 dev `15432/14222/18080`, prod `15433/14223/18091`; run #2 dev
  `25432/24222/28080`. Preflight passed its port assertions in both
  (`the instance ports are free before the session starts` PASS).
- **The blocker: the QA runtime has `2.0 GB` tmpfs on `$HOME` and `2.0 GB` tmpfs on `/tmp`,
  and a cold Go+frontend build does not fit in 4 GB combined.** Verbatim:
  - `make fe-install` → `npm error code ENOSPC … no space left on device, write` (`step-0.log`)
  - `make build` → `compile: writing output: write $WORK/b657/_pkg_.a: no space left on device`
    (`run2/step-1.log`)
  - `make migrate` → `write /home/beardedparrott/.cache/go-build/d2/…: no space left on device`
    (`20261017T053116Z/step-2.log`)
  After `go clean -cache` (freed 1.3 GB) and relocating `GOCACHE`, `GOTMPDIR` and the npm cache
  to `/tmp` (`run2`), the build still hits `ENOSPC`: `GOMODCACHE` is 686 MB and lives on `$HOME`,
  and a cold `GOCACHE` for this repo alone reached 1.6 GB of `/tmp` before dying.
  **Every FAIL inside `20261017T053116Z/` and `run2/` is this cascade** (no `bin/`, so no plane,
  no `/healthz`, no migrate, no served bundle). They are *not* defects and must not be filed —
  including the misleading `the served bundle is fetchable <- internal/api (SPA serving)`.
- Skips: one — step 3 `SKIP no client token resolvable (~/.orchicon/config or ORCHICON_TOKEN)`.
  `$HOME/.orchicon/` does not exist in this container, so the client half was never proven.

## 3. Steps 1-10 — what was actually observed

### Step 1 — cold start (PARTIAL)
`make build` **failed on `ENOSPC`**, so `bin/orchicon` and `bin/orch` were absent. But the
engine-free half that needs no build worked, verbatim from `20261017T053116Z/step-1.log`:

```
starting the host services for dev (postgres, nats, telemetry as processes of this user) …
dev services ready (postgres 127.0.0.1:15432, nats monitor 127.0.0.1:8222)
services for dev are up. DSN: postgres://orchicon:orchicon@localhost:15432/orchicon?sslmode=disable
```

Independent evidence (not the script's own report): the harness's `/proc/net/tcp` assertions
`Postgres is LISTENing on 15432` and `NATS is LISTENing on 14222` both PASS, and the listener
processes are `/usr/lib/postgresql/15/bin/postgres -D …/.local/share/orchicon-dev/postgres -p 15432`
and `/usr/local/bin/nats-server -js -sd …/orchicon-dev/nats -p 14222`. **Postgres+NATS for an
instance really do come up as this user's own processes with no container engine.** The plane,
`/healthz` and the port owner are UNPROVEN here (they need `bin/orchicon`).

### Step 2 — migrate + rls-check (UNPROVEN)
Both `make migrate` runs and the shimmed `check-rls.sh` died on `ENOSPC` in the Go build cache
(`step-2.log`). No `migrations applied OK`, no `RLS gate OK`, no `RLS gate SKIPPED` — the gate
never got to run, so nothing is claimed either way.

### Step 3 — connect (SKIP)
No `~/.orchicon/config` and no `ORCHICON_TOKEN` in this container → SKIP. A SKIP is a gap, not
a pass; the client half of the session is unproven on this host.

### Step 4 — change and rebuild (UNPROVEN)
The harness edited `frontend/src/main.tsx`, and `make build` then failed with
`npm error code ENOSPC` in `fe-build` (`Makefile:575`), so the served-bundle assertions had no
plane to read. The marker was restored by teardown (`git status` shows no change to
`frontend/src/main.tsx`). **AC3 and its stale-`dist` adversarial control are therefore NOT
verified in this pass** — the run could not reach the point where they mean anything.

### Step 5 — state (FAIL, D2)
`scripts/orchicon.sh status dev`, verbatim (`observed-no-engine/A-status.txt`, reproduced
identically in `20261017T053116Z/05-status-script.txt`):

```
Orchicon container instances
…/scripts/container.sh: line 1616: docker: command not found
…/scripts/container.sh: line 1620: docker: command not found
  dev: not created  plane:host
✗ cannot resolve the docker bridge address (docker network inspect bridge / docker0)
✗ pin it explicitly with ORCHICON_DOCKER_BRIDGE_IP=<bridge gateway ip>; refusing to guess or bind 0.0.0.0
✗ dev: cannot resolve the docker bridge — the host plane would bind its loopback address ONLY,
…   and its run containers could not reach it. Pin the address and retry:
serve is not running
✓ dev host plane healthy at http://localhost:8080
```

Three violations of AC4 in one output: (a) it makes a container-derived claim it cannot verify
(`not created`) on a host with no engine; (b) it never mentions the **services** half, although
dev Postgres and NATS were up and listening at that moment; (c) its last line claims
`dev host plane healthy` two lines after `serve is not running` — and it answers on the default
:8080, which in this container is a *different* plane (the QA sandbox), so the health verdict is
not tied to the instance whose status is being printed. `exit 0` throughout.

### Step 6 — logs (FAIL, D1)
`timeout 20 scripts/orchicon.sh logs dev` → **rc=127**, verbatim (`observed-no-engine/B-logs.txt`):

```
…/scripts/container.sh: line 1635: docker: command not found
```

No log content, no plane log path, nothing a developer could read. AC5 fails outright.

### Step 7 — two instances (UNPROVEN)
Needs two running planes; no plane could be started (`ENOSPC`).

### Step 8 — hygiene (FAIL, D4)
`make clean-docker` twice, with a sentinel at `~/.local/share/e2e-sentinel`
(`observed-no-engine/C-clean-docker.txt`):

```
bash: line 1: docker: command not found
make: *** [Makefile:415: clean-docker] Error 127
rc=2
… (second run identical)
rc2=2
/home/beardedparrott/.local/share/e2e-sentinel   # sentinel intact: nothing was pruned
```

It destroys nothing (the sentinel survives), but it is **not** a clean no-op: it fails twice with
rc=2 and a bare `docker: command not found`, which is exactly what AC7 forbids. Owner:
`Makefile:413-416` (the `docker` call is line 415).

### Step 9 — stop (UNPROVEN)
`make services-down` needs a built binary; not executed. Note for the DevOps step: the two runs
left host services running on 15432/14222 and 25432/24222 (`/proc/net/tcp`), which is itself a
live instance of the "next start is confusing" hazard AC8 targets — they are leftovers of the
killed harness, not a defect, but they must be reaped before a re-run.

### Step 10 — no blind spots (FAIL, D1+D2+D3)
From the step-5/6/8 output above, the answer to "what is running, and why?" on this host is
*not* obtainable from a documented command: `status` says `not created` and then `healthy`,
`logs` exits 127, and the one path that would work (`scripts/orchicon.sh start`) refuses with
`cannot resolve the docker bridge address … refusing to guess or bind 0.0.0.0`. The developer's
only remaining route is a container command they do not have, or reading source. AC9 FAIL.

## 4. Defects — confirmed and filed (never patched here, AC11)

| # | Symptom the developer hits | Owning file / sibling | Evidence | Issue |
|---|---|---|---|---|
| D1 | `scripts/orchicon.sh logs <inst>` exits 127 with `docker: command not found` — no way to read the plane's log without an engine | `scripts/orchicon.sh:101` → `scripts/container.sh:1635 logs_instance` (`docker logs -f`) | `observed-no-engine/B-logs.txt` | filed §4 |
| D2 | `status <inst>` prints `docker: command not found` ×2 and `not created` on a no-engine host, omits the services half, and ends with `✓ dev host plane healthy at http://localhost:8080` right after `serve is not running` | `scripts/container.sh:1616,1620` (`status_instances`, 1603-1630) + its `:8080` health check | `observed-no-engine/A-status.txt` | filed §4 |
| D3 | `scripts/orchicon.sh start <inst>` cannot be the documented cold start: `cannot resolve the docker bridge address (docker network inspect bridge / docker0)` … `refusing to guess or bind 0.0.0.0` | `scripts/container.sh` `bridge_ip` | `observed-no-engine/A-status.txt` (same refusal block) | filed §4 |
| D4 | `make clean-docker` is not engine-aware: rc=2, `docker: command not found`, on a host with nothing to prune — twice | `Makefile:413-416` (call at 415) | `observed-no-engine/C-clean-docker.txt` | filed §4 |
| D5 | `make run` is foreground and writes no plane log at `~/.local/share/orchicon-<inst>/serve/logs/orchicon.log` | `Makefile:272-278` | **not confirmed this pass** (no plane ever ran); `20261017T053116Z/step-1.log` records `plane log ABSENT` but with no `make run` behind it — do not file on this evidence | unfiled |
| D6 | only `container-rebuild` forces `force-fe=1`, so a stale `frontend/dist` is still reachable on the engine-free rebuild path | `Makefile:573-581` (`fe-build`), `:655` | **not confirmed this pass** — AC3's adversarial control never ran | unfiled |

D5 and D6 are predicted by the plan but were **not** observed here. Filing them on this run's
evidence would be speculation, so they are left for a pass that can build; the runbook already
documents `make build force-fe=1` as the engine-free rebuild command.

## 5. Known limits of this run

- The QA container's `$HOME` and `/tmp` are each a 2 GB tmpfs; `GOMODCACHE` (686 MB) plus a cold
  `GOCACHE` (~1.6 GB) plus `frontend/node_modules` do not fit. **A re-run needs a bigger scratch
  volume** (or a pre-warmed `GOCACHE`+`node_modules` volume) — that is the single blocking
  prerequisite for finishing AC1/2/3/6/8/10.
- Step 2's `psql`-masked rung and step 4's stale-`dist` control were never reached, so nothing in
  this report should be read as a pass for them.
- Port overrides were required (sandbox plane on 5432/4222/8080); the defaults are untested here.
- Nothing outside `qa-evidence/` and this report was modified by this step: D1/D2/D3/D4 are filed,
  not fixed.

### Harness delta after this run (SSE)

That blocker was itself a blind spot in the harness: it walked into an `ENOSPC` three minutes
in and reported a wall of FAILs that blamed no one. Step 0 now **measures** the cold-build
disk budget (`00-build-budget.log`) on every volume the build writes — checkout, `GOMODCACHE`,
`GOCACHE`, the npm cache, `TMPDIR`/`GOTMPDIR` — using the paths `make` will really write, and
relocates the three disposable ones (`GOCACHE`, the npm cache and `GOTMPDIR` into the git dir,
so the checkout stays clean) when a volume is short — `TMPDIR` is NOT moved: a longer `TMPDIR`
pushes unix socket paths past their 108-byte limit (`bind: invalid argument`), which is how the
first draft of this relocation broke `internal/claude`'s consent-socket tests. Otherwise it stops with `RUN INVALID`
naming the volume and the fix.

Measured on this same container-class host (warm caches): `checkout 174644 MB free / need 5100`,
`go-cache volume 360 MB / need 50`, `tmp 174644 MB / need 0` (after relocation) → step 0 exits
**0** here, where this run exited `2`. The correction that matters: the **module cache's demand
is never dropped** when `GOCACHE` moves off `$HOME` (a cold 686 MB module set has to download
where `GOMODCACHE` lives). That case is now measured, not assumed — pointing `GOMODCACHE` at an
empty directory on the same 360 MB volume makes step 0 exit **2** naming `GOMODCACHE` and
`go mod download`, instead of certifying the host and dying in `go build` three minutes later.

What this does *not* prove is the rest of the session — re-run it; no result in this report may
be read as a pass for steps it never reached.

---

## 6. Run #3 — the full session ACTUALLY RAN (acceptance verdict)

Evidence: `qa-evidence/no-engine-e2e/qa2-full/` (harness run `20261017T054835Z`, HEAD `1a39bbdf5`,
no container engine, dev `15432/14222/18080`, prod `15433/14223/18091`).
**Result: 49 PASS / 15 FAIL / 1 SKIP — the earlier disk blocker is gone** (step 0 measured
`checkout 174831/5100 MB, go caches 342/50, go $WORK 174831/0, tmp 582/200` and the cold build
completed). The engine-free session is now real end to end for the parts below.

Steps that **PASS outright** (independent evidence beside each): **1** (`make build` → `bin/orchicon`
+ `bin/orch`; `make services` → Postgres 15432 + NATS 14222 LISTENing per `/proc/net/tcp`;
`make run` → `/healthz {"status":"ok"}`, socket owned by a real pid), **2 migrate** (applies, second
run a clean no-op), **7** (dev + prod coexist, different DSNs/ports, independent start/stop, prod
survives dev's shutdown, prod's migrate never touches dev), **8 sentinel half** (nothing outside
Orchicon's artifacts disappears), **9** (stopping leaves every port free, `services status` → down).

Still-failing, now observed on a *running* session (filed, never patched here — AC11):

| # | AC | Symptom (verbatim from evidence) | Owner |
|---|---|---|---|
| D1 | 5, 9 | `logs dev` → rc=127, `scripts/container.sh: line 1635: docker: command not found`; no log content at all | `scripts/orchicon.sh:101` → `scripts/container.sh:1632` |
| D2 | 4, 9 | `status dev` prints `not created` + two `docker: command not found` lines on a no-engine host, never names the plane's port (18080), omits the Postgres/NATS half | `scripts/container.sh:1603-1630` |
| D4 | 7 | `make clean-docker` rc=2 twice with `docker: command not found` — not a clean no-op | `Makefile:413-416` |
| D5 | 5 | `make run` writes no plane log at `~/.local/share/orchicon-<inst>/serve/logs/orchicon.log` | `Makefile:272-278` |
| D7 | 2 | `make rls-check` fails with no host `psql`: no `RLS gate OK` (and the gate did not SKIP) | `scripts/check-rls.sh:49-78` |
| D6 | 3 | the stale-`dist` control still sees `==> frontend bundle up to date` with `dist` newer than `src` (`step-4.log:5-7`) — the mtime stamp skips the rebuild the documented path needs | `Makefile:569-581` |

**Harness defect H1 (this feature's own script, NOT a sibling's — must be fixed before AC3/AC10 can
be claimed):** step 4's served-bundle fetch wrote **0 bytes** in all four captures
(`04-served-bundle{,-after,-force-fe,-stale-control}.txt` are empty files), so "the served bundle is
fetchable", "the escape hatch clears the marker from the SERVED bundle" and the stale-dist control's
PASS are **vacuous** — an empty capture satisfies a `grep -F` that is only run on non-empty input.
**AC3 therefore stays UNPROVEN**: `make build force-fe=1` did rebuild (`==> building frontend bundle`)
and the plane restarted, but the served bytes were never read.

Step 3 remains **SKIP** (no client token in this container: no `~/.orchicon/config`, no
`ORCHICON_TOKEN`) — a gap in the proof, not a pass; mint one and re-run for the client half.

Verdict: **AC1/2/6/8/9 partly met, AC4/5/7 fail (D1/D2/D4), AC3 unproven (H1 + D6), AC10 partial.**
The next pass must (a) fix step 4's fetch so the served bytes are actually read, then (b) re-run;
the sibling defects above are filed against their owners.

Also observed, and worth a line for AC8/AC10: a leftover `orchicon container` supervisor from an
earlier harness run (pid 22952, `PPid: 1`) kept **respawning** `postgres -p 15432` / `nats -p 14222`,
so a later run's preflight found the ports held by its own predecessor (`HELD 15432 14222`). The
harness correctly refused (`RUN INVALID`) instead of lying — that half of AC10 works — but the
leftover supervisor must be reaped by hand before a re-run.

---

## Iteration 2 — H1 fixed; its real cause is a new defect, D8

**H1 (blocking, harness) is fixed.** Step 4 no longer reports anything about bundle
*content* unless the baseline fetch succeeded: fetchability is a gate, and the
four marker assertions become `SKIP` ("AC3's served half is UNPROVEN") instead of
vacuous PASSes on a 0-byte capture. Two further honesty fixes: the harness now
captures the **built** bytes (`04-dist-bundle*.txt`) beside the **served** bytes so a
miss bisects the build (`Makefile:569-581`) from the embed/restart, and preflight
requires the binaries the build runs (`node_modules/.bin/{tsc,vite}`) rather than the
`node_modules` directory, so a half-extracted tree is `RUN INVALID` naming
`npm ci` instead of dying mid-build with `tsc: not found`.

**The environment half of H1 is repaired**: `frontend/node_modules` was a partial
extraction and `frontend/dist/index.html` was a 0-byte untracked file, so every build
embedded an empty index; `npm ci` + `npm run build` now produce a real bundle
(1640-byte index naming `/assets/index-D6pz2Smk.js`).

**D8 (new, sibling-owned, blocking for AC3's documented path):** with a correct
bundle embedded, `GET /` on the plane `make run` starts answers **HTTP 401
`{"error":"missing credentials"}`**, because the default path in
`cmd/orchicon/main.go:110-116` never wraps the handler with `withFrontend` — that
wrap exists only on the `serve` subcommand (`cmd/orchicon/serve.go:386`). `make run`
(Makefile:272-278) runs the default path, so the documented engine-free session
serves the API where the UI should be. Independent evidence:
`qa-evidence/no-engine-e2e/h1-fix/01-served-index.txt`.

**AC3, corrected status:**
* build half — PROVEN: after `make build force-fe=1` the built dist carries the change
  (`04-dist-bundle-after.txt`), and the stale-dist control FAILS at the build level
  (the previous bundle survives a dist-newer-than-src `make build` — D6, now proven
  independently of serving);
* serve half — PROVEN through `bin/orchicon serve` (GET / -> **200**, the served asset
  carries the marker), i.e. the rebuild→embed→serve chain is live;
* through the **documented** entry point (`make run`) — BLOCKED by D8.

See `qa-evidence/no-engine-e2e/h1-fix/H1-and-D8.md` for the transcript and the
results ledger (`results.tsv`, `defects.tsv`).
