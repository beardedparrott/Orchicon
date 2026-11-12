# H1 resolved in the harness; H1's real cause is D8 (the plane `make run` starts does not mount the UI)

Run: `scripts/no-engine-e2e.sh --evidence-dir qa-evidence/no-engine-e2e/h1-fix
--pg-port 15432 --nats-port 14222 --control-port 18080 --prod-pg-port 15433
--prod-nats-port 14223 --prod-control-port 18091`
Host: the disposable no-engine runtime container (docker/podman/nerdctl/docker-compose absent).

## 1. H1 (iteration-2 blocker) — fixed, and it was two things

### 1a. Harness: a 0-byte capture produced three vacuous PASSes
Iteration 1 shipped step 4 with `check_absent`/`check_contains` over
`04-served-bundle*.txt` and no gate on fetchability, so a 0-byte capture made
"the marker is NOT in the bundle", "STALE-DIST CONTROL" and "the escape hatch
clears the marker" all PASS on a plane that served no UI at all.

Fixed (no production code, harness only):
* fetchability is a **gate** — when the baseline fetch fails, every
  bundle-content assertion is `SKIP` ("AC3's served half is UNPROVEN"), never PASS;
* a second, independent product is captured — `04-dist-bundle*.txt` (the bytes in
  `frontend/dist`, i.e. what the build made) beside `04-served-bundle*.txt` (the
  bytes the server returned), so a miss bisects build vs embed instead of blurring;
* preflight now requires a **toolchain** (`frontend/node_modules/.bin/{tsc,vite}`),
  not a directory — a partial extraction is `RUN INVALID` (exit 2) naming
  `cd frontend && npm ci`, because `npm install` does not repair a half-extracted tree;
* preflight detects the realised stale-dist state (a `frontend/dist/index.html` that
  exists but carries no bundle) and repairs it as SETUP via `make fe-build force-fe=1`,
  recorded in `results.tsv`;
* step 1 asserts the UI half of the cold start (`01-served-index.txt`: the HTTP
  status of `/` plus the hashed assets the returned HTML names), so a frontendless
  binary can no longer look like a mysterious empty fetch in step 4.

### 1b. Environment: the QA worktree could not build the frontend at all
`frontend/node_modules` was a partial extraction (no `.bin/`, `lucide-react` with no
`.d.ts`) and `frontend/dist/index.html` was a **0-byte untracked file**, so every
build embedded an empty index.html. Repaired with `npm ci` + `npm run build` on a
volume with room; `frontend/dist/index.html` is now 1640 bytes naming
`/assets/index-D6pz2Smk.js`.

## 2. What the corrected step 1 now reports — D8 (new, sibling-owned)

    GET http://127.0.0.1:18080/ -> HTTP 401
    hashed js the served HTML names:
    --- first 400 bytes of the body ---
    {"error":"missing credentials"}

…while the build is fine: `frontend/dist/index.html` names
`/assets/index-D6pz2Smk.js` and `bin/orchicon` embeds it (`grep -c` -> 2 hits).

Root cause: **the plane `make run` starts never mounts the SPA.**
`make run` (Makefile:272-278) runs `go run ./cmd/orchicon` with no subcommand, so
it takes the DEFAULT path, `cmd/orchicon/main.go:110-116`, which builds the server
and calls `srv.Run(ctx)` — `withFrontend(...)` is only applied on the `serve`
subcommand (`cmd/orchicon/serve.go:386`). `/` therefore falls through to the auth
middleware (`internal/middleware/auth.go:95`) and answers 401
`{"error":"missing credentials"}` — the exact credentials-shaped message for a
build/serving problem that `cmd/orchicon/serve_state.go:124-141` was written to
explain. Owner: `cmd/orchicon/main.go:110-116` (+ `Makefile:272-278`).
This is the "a `make run` that starts a plane nothing can talk to" half of the
defect family this task names; it is FILED, not patched (AC11).

## 3. AC3 — proven through `orchicon serve`, blocked through `make run`

Live proof (transcript, same tree, same binary):

    # edit: frontend/src/main.tsx gains the marker NO-ENGINE-E2E-SERVE-PROBE
    make build force-fe=1 instance=dev            -> rc=0
    ORCHICON_HTTP_ADDR=127.0.0.1:18080 bin/orchicon serve
    GET /                       -> HTTP 200
    served index names          -> /assets/index-9azVquGO.js
    marker in that served asset -> 1 hit

So the rebuild→embed→serve chain is verifiably live: the bytes a browser would
receive after `make build force-fe=1` carry the change. The documented entry point
(`make run`) cannot show it because of D8, which is why step 4's served assertions
are SKIP in `results.tsv` rather than PASS.

## 4. D6 is now proven at the build level, independently of serving

   4  PASS  the BUILT dist carries the new marker after make build (the build half of AC3)
   4  FAIL  STALE-DIST CONTROL (BUILT): a dist newer than src does not keep the previous bundle

With `frontend/src` restored and `frontend/dist/index.html` forced 1h into the
future, `make build` kept the PREVIOUS bundle (marker still present in
`04-dist-bundle-stale-control.txt`): the mtime stamp at `Makefile:569-581` skipped a
rebuild that was required. Owner: `Makefile:574-581` (only `container-rebuild` sets
`force-fe=1`).

## 5. Standing defects (unchanged by this iteration)

D1 `logs` exit 127 (`scripts/container.sh:1632`), D2 `status` "not created" + no
services half (`scripts/container.sh:1603-1630`), D3 host-plane launcher needs a
docker bridge, D4 `clean-docker` rc=2 (`Makefile:413-416`), D5 `make run` writes no
plane log (`Makefile:272-278`), D6 above, D7 `make rls-check` needs a host psql
(`scripts/check-rls.sh:49-78`), **D8 above**. Steps 1-cold-start (services/healthz),
2-migrate, 7-two-instances and 9-no-orphan remain PASS.
