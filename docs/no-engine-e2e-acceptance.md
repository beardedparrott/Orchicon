# The no-container-engine developer session — acceptance runbook

This is the procedure that proves one claim end to end:

> On a clean checkout, with **no container engine installed**, a developer can build,
> start, migrate, connect to, change, inspect, run two instances of, clean up and stop
> Orchicon — and at **no point** needs a container command, or needs to read source, to
> know *what is running and why*.

Every individual script in the inner loop can pass its own test while the *combination*
leaves a gap (a `make run` nothing can talk to, a "rebuild" that serves the previous UI, a
status that cannot say what is up). Only a full session catches that, so this is a session
procedure, not a unit test — it takes minutes, not seconds.

Automated form: [`scripts/no-engine-e2e.sh`](../scripts/no-engine-e2e.sh). Run it, or type
the steps below by hand; they are the same commands, and the harness's evidence files are
named after the step numbers below.

```bash
scripts/no-engine-e2e.sh                 # full session, dev + prod
scripts/no-engine-e2e.sh --dry-run       # print the plan, execute nothing
scripts/no-engine-e2e.sh --preflight-only # only step 0: is this a valid no-engine host?
```

Evidence lands in `qa-evidence/no-engine-e2e/<UTC>/` (`results.tsv`, `defects.tsv`,
`summary.md`, one log per step). Exit codes: `0` every assertion passed, `1` at least one
assertion failed (each failure names the `file:line` that owns it), `2` the run is not a
valid proof (an engine is installed, or a port we need is already held) — nothing ran.

---

## Before you start: the validity precondition (step 0)

The claim is *no container engine*. If one is installed, a passing run proves nothing, so
the harness refuses to start:

```bash
command -v docker podman nerdctl docker-compose    # all four must print nothing
git status --porcelain                             # a clean checkout
```

Two more conditions decide whether a run is meaningful:

- **Ports must be free.** The instance's Postgres/NATS/control ports must not already be
  bound by something else — otherwise a PASS could be a different database. Check without
  `ss`/`lsof` (they are absent on a bare host):

  ```bash
  for p in 5432 4222 8080 5433 4223 8091; do
    awk -v want="$(printf '%04X' "$p")" 'NR>1 && $4=="0A" {n=split($2,a,":"); if (a[n]==want) print "HELD " want}' \
      /proc/net/tcp /proc/net/tcp6
  done
  ```

  If a port is held (another plane, a system Postgres), move **this** run onto free ports
  with the same knobs the port table already honours — `DB_URL` and `make run`'s exports
  move with them:

  ```bash
  scripts/no-engine-e2e.sh --pg-port 15432 --nats-port 14222 --control-port 18080 \
                           --prod-pg-port 15433 --prod-nats-port 14223 --prod-control-port 18091
  ```

- **Frontend deps.** A fresh worktree has no `frontend/node_modules`; `make build` needs
  `make fe-install` once (network on first use).

---

## The session, step by step

### 1. Cold start

```bash
make build            # binaries into bin/; the frontend bundle is go:embed-ed into them
make services         # this instance's Postgres + NATS, as YOUR user, no engine (downloads pinned binaries on first use)
make run              # starts `services` first, then the plane with the instance's DSN/NATS/HTTP addr exported
```

Independent evidence that it worked — not the scripts' own report:

```bash
curl -fsS http://127.0.0.1:8080/healthz          # {"status":"ok"}
bin/orchicon services status dev                 # "services for dev: up (postgres 127.0.0.1:5432, ...)"
```

**What it proves:** the plane came up *against locally-started services* with no engine in
the picture. **Blind spot if it fails:** a `make run` that reports success while nothing
answers `/healthz`.

### 2. Migrate

```bash
make migrate          # applies db/migrations through the repo's own engine (tools/atlas-ci)
make migrate          # a second run must be a clean no-op
make rls-check        # the RLS gate
```

`make rls-check` must prove the **engine-free rung**: it resolves the instance's *own*
downloaded `psql` (`scripts/check-rls.sh`), not a host `psql`. On a host that has `psql`
installed you must remove it from `PATH` for this check to prove anything — the harness
does exactly that and records the unshimmed run beside it. A `RLS gate SKIPPED` line means
the gate did not run; it is not a pass.

**What it proves:** the DSN the inner loop derives reaches the database the local services
actually bound.

### 3. Connect

```bash
bin/orch --url http://127.0.0.1:8080 --token <token>   # or pick the profile with /connect
```

The token lives in `~/.orchicon/config` (or `ORCHICON_TOKEN`); the harness records only
whether one resolved, never its value. Capture the screen under a PTY
(`script -q -c '…' out.txt`) so the assertion is on rendered output, not on an exit code.

**What it proves:** the client talks to the plane that is running, not to a default URL.

### 4. Change something and rebuild — is it actually live?

The repo has a documented history of a rebuild that "succeeds" while serving the previous
UI. `container-rebuild` exists partly because it forces `force-fe=1`. On the engine-free
path you must assert the change in the **served bytes**, never in `frontend/dist`:

```bash
# baseline: what the browser would execute right now
curl -fsS http://127.0.0.1:8080/ | grep -oE 'src="[^"]+\.js"'      # the entry chunk
# edit an observable string in frontend/src/…
make build force-fe=1     # force-fe=1 is the engine-free equivalent of what container-rebuild forces
# restart the plane — the bundle is go:embed-ed, so a rebuild WITHOUT a restart cannot be live
curl -fsS http://127.0.0.1:8080/assets/index-<hash>.js | grep -F 'YOUR-MARKER'
```

The **adversarial control** is what makes this a proof rather than a happy path: restore
the source, force `frontend/dist/index.html` to look *newer* than every source file
(`touch -d '+1 hour' frontend/dist/index.html`), rebuild, restart, and re-fetch. The served
bundle must **not** carry the old change. If it does, the mtime stamp check
(`Makefile` `fe-build`) shipped a stale dist — that is a defect against the build, not a
harness problem, and the documented rebuild command stays `make build force-fe=1`.

**What it proves (AC3):** a code change is verifiably live after the documented rebuild
path, with the stale-`dist` failure mode specifically excluded.

### 5. State

```bash
scripts/orchicon.sh status dev
```

Compare it against independent ground truth you can read without any tool that may be
absent:

```bash
cat /proc/net/tcp /proc/net/tcp6 | awk '$4=="0A"'      # what is LISTENing
tr '\0' ' ' < /proc/<pid>/cmdline; echo                # what each process actually is
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz
bin/orchicon services status dev
```

A status report passes only when it (a) makes no container-derived claim it could not
verify (no "not created" on a host with no engine), (b) names the instance and its port,
(c) claims health **only** where `/healthz` answered, and (d) also reports the **services**
half — Postgres/NATS are half of "what is running".

### 6. Logs

```bash
scripts/orchicon.sh logs dev      # bound it: timeout 10 scripts/orchicon.sh logs dev
```

The assertion is on **content**: the capture must show activity from steps 1-4. "The
command ran" is not evidence. The host plane's log belongs at
`~/.local/share/orchicon-<inst>/serve/logs/orchicon.log` — the path every other verb
already knows.

### 7. Two instances

```bash
make services instance=prod
make run instance=prod                       # plane on 127.0.0.1:8091, Postgres 5433, NATS 4223
make migrate instance=prod                   # must apply to 5433, never dev's 5432
curl -fsS http://127.0.0.1:8091/healthz      # prod up…
curl -fsS http://127.0.0.1:8080/healthz      # …and dev untouched
make services-down instance=dev              # stopping dev must not disturb prod
curl -fsS http://127.0.0.1:8091/healthz      # prod still 200
```

**What it proves:** the two instances are independently controllable, with separate
databases, data dirs and ports.

### 8. Hygiene

```bash
mkdir -p ~/.local/share/e2e-sentinel        # something that is not ours
make clean-docker                           # twice
ls -la ~/.local/share                       # the sentinel is still there; nothing else vanished
```

With no engine this must be a **clean no-op** (exit 0, nothing pruned) — not a
`docker: command not found` failure. Running it a second time must also be clean.

### 9. Stop

```bash
make services-down instance=dev
make services-down instance=prod
```

Then verify what actually matters, which is not the exit code:

```bash
for p in 5432 4222 8080 5433 4223 8091; do … /proc/net/tcp …; done    # every port free
bin/orchicon services status dev                                      # "down", exit 1
```

An orphaned Postgres still holding 5432 is exactly what makes the *next* start confusing,
so **the ports coming free is the assertion**.

### 10. No blind spots

For each step above, ask: *could I answer "what is running, and why?" from a documented
command's output alone?* The harness writes that table to `10-blindspots.md`. Any cell that
reads *"only knowable by reading source or running a container command"* is a FAIL of AC9
and names the sibling file that owns it.

---

## What each command proves (the "never blind" map)

| Question the developer asks | The documented answer | Independent evidence beside it |
|---|---|---|
| Is the backend up? | `make services-status` | Postgres/NATS listening in `/proc/net/tcp` |
| Is the plane up? | `make run` output, `scripts/orchicon.sh status` | `GET /healthz` → 200; pid owning the port from `/proc/<pid>/fd` |
| Is it *healthy*? | `status` | `{"status":"ok"}` body, verbatim |
| Which instance is this? | `status`, `services status <inst>` | the resolved port + data dir for that instance |
| Did my frontend change land? | `make build force-fe=1` + restart | the **served** asset bytes contain the new marker |
| What has it been doing? | `scripts/orchicon.sh logs <inst>` | the plane log file's recent lines |
| What did it touch / what is left? | `make clean-docker`, `make services-down` | `/proc/net/tcp` free, `~/.local/share` diff |

## Known limits (state them, never paper over them)

- Steps 5 and 6 are assertions **about** `scripts/orchicon.sh`; when they fail, the owning
  file is named in `defects.tsv` and the defect is **filed, never patched here** (AC11).
  Filing is the acceptance procedure's job; fixing belongs to the sibling that owns the file.
- `--pg-port/--nats-port/--control-port` move the run onto free ports on a shared host; the
  defaults are exercised on a host with nothing else listening.
- Step 3 records SKIP when no client token is resolvable. A SKIP is a gap in the proof, not
  a pass — mint a token (TUI `/connect`, or `ORCHICON_TOKEN`) and re-run.
