# Iteration 2 — QA re-verification of the H1 fix (independent of the engineer's run)

- Tree: `dd2d953b7` (branch `e2e-proof-full-developer-session-with-no-container-engine-never-blind-j7bwx5hcmvrjhd4c`)
- Host: the no-engine runtime container (`docker`/`podman`/`nerdctl`/`docker-compose` absent)
- Scope: re-test **the fix only** (H1's vacuous-PASS class) and re-check the defect
  ledger. No production code, no Makefile, no harness edits — none was needed.

## 1. The vacuous-PASS rule, verified THREE independent ways

### 1a. The repo's own gate (`make no-engine-e2e-assertions-test`) — 34 passed, 0 failed
Pins the rule structurally: step 4 calls no raw `check_absent`, its single raw
`check_contains` is the presence assertion (fail-closed on an empty capture), and
every served/built capture is gated.

### 1b. MY OWN probe against the real functions (`mutation-probe.sh`, this directory)
Sourced `scripts/no-engine-e2e.sh` (its `main` is guarded), then drove the gate
directly — independent of the repo's test of the same functions:

```
--- M1 empty capture, absence assertion (old behaviour: PASS = vacuous)
  SKIP M1 served bundle lacks marker — why-empty
--- M2 empty capture, presence assertion
  SKIP M2 served bundle carries marker — why-empty
--- M3 non-empty, marker genuinely absent (must PASS)
  PASS M3 stale control
--- M4 non-empty, marker present, absence assertion (must FAIL)
  FAIL M4 stale control caught (found 'XMUTPROBE' in .../withmarker.txt)   <- owner: owner:file:1
--- M5 non-empty, marker present, presence assertion (must PASS)
  PASS M5 change is live
=== verdict
PROBE OK: empty captures SKIP, real content is judged
```

(My first probe run reported `PROBE BROKEN` — that was a bug in the probe: a
single-quoted heredoc left the needle literal `XMUT-$$` in the fixture while the
expectation expanded to the PID. Fixed fixture, re-run, both directions correct. Recorded
because a probe that only ever prints OK would itself be the failure mode under test.)

### 1c. The real run's ledger (`qa-evidence/no-engine-e2e/h1-fix/results.tsv`, step 4)
With a correct bundle embedded but the served fetch failing (D8), the four
served-content assertions are `SKIP`, and the byte captures are 0 bytes — i.e. the
class of PASS that H1 exposed is gone:

```
4  FAIL  the served bundle is fetchable (independent: HTTP GET of the served asset)
4  SKIP  the marker is NOT in the served bundle before the edit (clean baseline)
4  SKIP  THE CHANGE IS LIVE: the SERVED bundle carries the new marker
4  SKIP  STALE-DIST CONTROL: a dist newer than src does not ship the previous UI (SERVED)
4  SKIP  the escape hatch clears the marker from the SERVED bundle
4  PASS  the BUILT dist carries the new marker after make build (the build half of AC3)
4  FAIL  STALE-DIST CONTROL (BUILT): ... (found 'NO-ENGINE-E2E-...-122703' in 04-dist-bundle-stale-control.txt)
```

## 2. AC3 — the serve path re-proven by me, byte-for-byte (no build needed)

Same single binary (`bin/orchicon`, embeds the real `frontend/dist`), two entry points,
both on loopback ports owned by the QA run:

```
== default path (what 'make run' executes: no subcommand) port 19091
healthz=200
GET / -> HTTP 401
body: {"error":"missing credentials"}
== 'orchicon serve' subcommand port 19092
GET / -> HTTP 200
served index names: /assets/index-9azVquGO.js
asset fetch: HTTP 200 bytes=2712115
```

The served asset is byte-identical to the build's product, so the served bytes cannot
be a stale dist:

```
81f6a97d088833fd8da60eb5fdd2135d  /tmp/orchicon/probe/d8-asset.js      (SERVED)
81f6a97d088833fd8da60eb5fdd2135d  frontend/dist/assets/index-9azVquGO.js (BUILT)
frontend/dist/index.html names: assets/index-9azVquGO.js
```

⇒ **D8 independently confirmed**: the rebuild→embed→serve chain is live, and the
*documented* engine-free entry point (`make run`, `Makefile:272-278` → the default path
in `cmd/orchicon/main.go:110-116`, which never applies `withFrontend`) serves the API's
401 where the UI belongs. Filed, not patched (AC11). AC3's served half through the
documented path therefore remains **UNPROVEN**, and the harness says exactly that.

## 3. Standing defects re-observed live by QA this iteration (no build required)

| defect | command | observed now |
|---|---|---|
| D1 | `scripts/orchicon.sh logs dev` | `rc=127`, `container.sh:1635: docker: command not found` |
| D2 | `scripts/orchicon.sh status dev` | `dev: not created`, two `docker: command not found` leaks, no services half; plus a bridge-resolution refusal and a bare `serve is not running` |
| D4 | `make clean-docker` (x2 documented) | `rc=2`, `Makefile:415: clean-docker Error 127` ← `docker: command not found` |
| D5 | plane log path | `$HOME/.local/share/orchicon-dev/serve/logs/` does not exist — `make run` writes no plane log |
| D6 | (build-level, from the run ledger) | stale-dist control FAILs: a dist newer than src skips the required rebuild (`Makefile:574-581`) |
| D8 | §2 above | `/` → 401 via the `make run` path, 200 via `serve` |

D3 (host-plane launcher needs a docker bridge address) is visible in the same `status`
output (`cannot resolve the docker bridge address ... refusing to guess or bind 0.0.0.0`).
D7 (`make rls-check` without a host psql) is unchanged from `qa2-full`/`h1-fix`
(`02-rls.log`).

Note for the next run: the runtime container's own sandbox plane holds `127.0.0.1:8080`
(`/healthz` → 200 in this container), so `scripts/orchicon.sh status dev` reports
"dev host plane healthy at http://localhost:8080" for a plane that is NOT dev's. Any
harness re-run must keep the port overrides (`--control-port 18080`, ...) — otherwise
step 5's health claim and the preflight's port check are being made against a foreign
plane.

## 4. Verdict of this iteration

- Harness honesty (H1): **FIXED and verified** — structural, behavioural, and by my own
  independent mutation probe.
- Feature acceptance: **still FAILS** on D1, D2, D4, D5, D6, D7 and D8. All are filed
  against their owning file:line; none is patched here (AC11, and this task changes no
  production code). The harness reports exactly this — it never claims health it has
  not verified.
