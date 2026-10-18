# Plane control from the desktop app, the TUI and the CLI — acceptance procedure

> **The claim.** Plane control works from the **desktop app** and the **TUI**, and both reach the
> **same shared mechanism** as the CLI. A Stop that passes its own unit test but cannot actually take a
> running plane down, and a Start that only works because a plane was already up, both go green while a
> user is stuck. So this procedure walks the whole cycle against a real instance and believes none of it
> until an **independent probe** agrees.

This work item ships a **procedure**, not a feature. It changes no product code: a failure found on a
surface is filed as a defect against the sibling task that owns it (see §8).

---

## 1. The automated form, and its exit codes

```bash
scripts/plane-control-e2e.sh --dry-run        # print the plan; starts nothing
scripts/plane-control-e2e.sh --preflight-only # the validity precondition (step 0) only
scripts/plane-control-e2e.sh                  # the whole procedure
scripts/plane-control-e2e.sh --instance prod --pair dev prod --with-desktop
```

| exit | meaning |
|---|---|
| `0` | every assertion passed — the cycle is proven |
| `1` | at least one assertion **FAILED**; `defects.tsv` names the `file:line` of the sibling task that owns it |
| `2` | this run is **not a valid proof** — a surface is absent, the instance is not installed, or a port was already held |

Exit `2` is a verdict, not an error: a procedure that goes green when the thing under test is absent is
worse than no procedure. The steps that *could* run still run, and still leave their evidence.

The harness's own honesty is gated offline:

```bash
scripts/tests/plane-control-e2e-assertions/run.sh   # exits 0 only if every probe agrees with its fixture
```

That gate is what stops the harness green-lighting a broken plane: it proves `health_down` says DOWN
only for a *refused* connection, that `port_listening`/`port_free` agree with the socket table in both
directions, that an absent port record is a FAIL and never an empty-and-therefore-free list, and that a
merely *healthy* status line is not read as a *current* binary line.

---

## 2. The validity precondition (step 0)

The run is not a proof unless all of these hold. Each is an assertion with an owner.

| id | precondition | if it does not hold |
|---|---|---|
| A0.1 | the binary under test builds and runs | the harness is broken — this item |
| A0.2 | the instance is installed (`~/.local/share/orchicon-<I>` exists) | **invalid host** (exit 2), no defect filed |
| A0.3 | that instance has a non-empty `ports.json` | invalid host; the remedy is `orchicon install` |
| A0.4 | **no** recorded port is held before the run starts | invalid host — evidence from a dirty host proves nothing |
| A0.5 | a **TUI lifecycle surface** exists (`internal/tui`/`cmd/orch` reach `internal/planecontrol`) | **BLOCKED + defect against the TUI task** (exit 2) |
| A0.6 | a **Wails desktop app** exists and a display is available | **BLOCKED + defect against the Wails task** (exit 2) |
| A0.7 | no container engine is required | recorded, never gated |

The two surfaces in A0.5/A0.6 are checked mechanically because they are exactly what cannot be exercised
by a unit test: if the surface is not there, there is nothing to prove and the run says so.

---

## 3. The steps, as commands a human can type

Notation: `<I>` = instance, `<P>` = `ORCHICON_CONTROL_PORT` from
`~/.local/share/orchicon-<I>/ports.json`, `$D` = `~/.local/share/orchicon-<I>`.

### Step 1 — TUI stop (AC1)

1. Baseline: `curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:<P>/healthz` → `200`;
   `readlink /proc/$(cat $D/serve/pids/orchicon.pid)/exe`.
2. From `orch`, use the stop action (the harness drives it through a real pty; the binding is one
   variable, `PLANE_E2E_TUI_STOP_KEYS`, so a mismatch costs one export).
3. **Down** means `curl -sS http://localhost:<P>/healthz` **exits 7 with an empty body** — a refused
   connection, not a 5xx.
4. The PID file is gone or names a dead PID.
5. **Every** port in `ports.json` is free: `awk` over `/proc/net/tcp{,6}` (`ss`/`lsof` are not assumed).
   A process that exited while a port stayed held is a **FAIL**, and the harness says which port.

### Step 2 — desktop app start (AC2)

Launch the packaged app, use the **offline state's start action**. Then:
`/healthz` → `200` **and** the window renders the UI (attach `02-desktop-start.png`). The app's own
status line is never the proof.

### Step 3 — the reverse direction (AC3)

Desktop **stop**, TUI **start**. The discriminating claim is that both surfaces drive the **same PID and
the same state dir**: the PID file written by the TUI must be the PID the desktop app kills, and vice
versa. Two surfaces using different state dirs would each "work" and never control the same plane.

### Step 4 — CLI parity (AC4)

```bash
orchicon start <I>          # then: /healthz 200
orchicon plane status <I>   # names the instance, the pid, the serving binary and healthy/NOT answering
orchicon stop <I>           # then: connection REFUSED and every port free
orchicon stop <I>           # again: idempotent, exit 0
orchicon restart <I>        # new pid, /healthz 200
orchicon serve --status     # PID only — see the note below
```

`serve --status` reports a PID and nothing else (`cmd/orchicon/serve.go:<serveStatus>`). It cannot say
*which* instance, *which* binary or *whether the binary drifted*, so **drift is asserted on
`plane status`** (correction C2). Whether `serve --status` should delegate is the CLI-verbs task's call;
this procedure records the narrowness as an observation.

### Step 5 — drift (AC5), asserted on `plane status`

```bash
orchicon plane status <I>                 # → "binary current"
go build -o "$BIN.new" ./cmd/orchicon && mv "$BIN.new" "$BIN"   # RENAME unlinks the old inode
orchicon plane status <I>                 # → "binary superseded", still answering /healthz 200
orchicon plane start <I>                  # → restarts on the installed binary; new pid; "binary current"
serve --status                            # recorded beside it: it cannot answer this question
```

`mv` (not `cp`) is deliberate: overwriting a running executable is `ETXTBSY`-dependent and would prove
the kernel instead of `internal/planecontrol/hostproc_unix.go:<Drift>`. A status surface that only probed
the port would print "healthy" here — which is the failure this step discriminates.

### Step 6 — the fail-safe, both halves (AC6)

**Half 1 — our own plane, unidentifiable:**

```bash
mv $D/serve/pids/orchicon.pid $D/serve/pids/orchicon.pid.bak
orchicon plane restart <I>   # → "could not identify the process; leaving it", exit 0
readlink /proc/<same pid>/exe   # unchanged; /healthz still 200 → LEFT ALONE
mv $D/serve/pids/orchicon.pid.bak $D/serve/pids/orchicon.pid
```

The proof is the **same process still serving**, not the message. The message is convenient
(`internal/planecontrol/planecontrol.go:<Start>`, the `default:` arm) and is matched separately, so a
harness that only matched the wording would fail this step.

**Half 2 — a FOREIGN holder is reported, never killed:**

```bash
orchicon services down <I>
python3 -m http.server <pg-port> --bind 127.0.0.1 &     # not ours, carries no instance name
orchicon services down <I> --force                      # exit 1
kill -0 <foreign pid>                                   # STILL ALIVE
```

Expected output (`cmd/orchicon/services.go:236-245`): `did not release postgres :N — and the holder is
NOT this instance's`, then `pid N /path/to/exe`, then `nothing was killed`. `--force` reaches only **this
instance's own** leftovers (`cmd/orchicon/reclaim.go:<ourInstanceProcesses>`): our own leftover is
reclaimed, a stranger is left alone.

### Step 7 — two instances (AC7)

Start both. `orchicon stop <A>` → A refused and A's ports free, while **B still answers 200, keeps its
own PID**, and **B's log is not written to** (inode, size and mtime unchanged). With dev and prod side by
side, every line of evidence names the instance: "the plane" is ambiguous.

### Step 8 — the cross-instance environment leak (AC8, correction C4)

The regression this repo actually hit — a plane started for `dev` from a shell carrying `prod`'s
environment wrote its log into **prod's** log file and never wrote a PID file:

```bash
ORCHICON_SERVE_STATE_DIR="$HOME/.local/share/orchicon-prod/serve" \
ORCHICON_SERVE_LOG_FILE="$HOME/.local/share/orchicon-prod/serve/logs/orchicon.log" \
ORCHICON_SERVE_DETACHED=1 ORCHICON_INSTANCE=prod \
  orchicon start dev
```

It passes only if: dev has **its own** PID file; that PID is the process serving dev; dev's log was
written; **prod's log is byte-for-byte untouched** (inode/size/mtime identical); `plane status` reports
only each instance's own process; and the planted `ORCHICON_SERVE_DETACHED=1` did **not** turn the start
into a foreground plane. The strip site is `internal/planecontrol/planecontrol.go:<instanceEnvKeys>` —
`getenv` returns the *first* match, so an ambient value that is appended to rather than stripped wins.

### Step 9 — the web app gained nothing (AC9)

1. `go test ./internal/api -run 'TestAPISurfaceDidNotGrow|TestProtoDeclaresNoLifecycleService'` — no
   lifecycle RPC exists.
2. A grep over `frontend/src` for a **lifecycle-shaped identifier or call** must be empty, and only
   after a **control** pattern (`useToastStore`) has matched — an empty grep that could never have
   matched is a SKIP, not a PASS. (Prose in the settings help — "plane restart" — is a sentence, not a
   control; it is recorded as an observation, not a failure.)
3. Observation, needs a browser: with a tab open on the plane's origin, stop the plane from the TUI. The
   tab must show the SPA's **existing** connection-failure presentation (`frontend/src/main.tsx:15-44`,
   `ConnectError` on the failed refetch). Attach `09-webapp-offline.png` and `09-webapp-console.txt`.

---

## 4. What is real here

- `/healthz` over real HTTP, the real listening sockets in `/proc/net/tcp{,6}`, the real
  `/proc/<pid>/exe`, the instance's own `ports.json`, its own PID file, and its own log file.
- A start is proven by a **200**, a stop by a **refused connection AND every port free**, "left alone" by
  the **same PID still serving the same executable**, and drift by the **`(deleted)` suffix** of a
  replaced binary.
- **No verdict is ever taken from the control's own stdout.** The control's output is logged beside the
  independent probe so a disagreement is visible.

## 5. What this procedure refuses to call a pass

- A surface that does not exist (no TUI lifecycle control, no Wails app, no display) → BLOCKED + defect.
- An instance that is not installed, or ports held before the run → invalid host.
- "The process exited" → a held port is a FAIL naming the port.
- An empty grep → SKIP unless a control pattern matched first.
- A `serve --status` that reports "running" while the plane serves a **superseded binary** → that is
  drift, and it is asserted on `plane status`.

## 6. Where evidence lands

`qa-evidence/plane-control-e2e/<UTC>/`:

```
results.tsv                   step, verdict, assertion, owner
defects.tsv                   one row per failure/block, naming the owning file:line
summary.md                    per-step verdict table + the defects table + the file list
NN-<slug>.log                 every command, its exit code, and its verbatim output
02-desktop-start.png          (manual) the desktop app's UI after its start action
09-webapp-offline.png         (manual) the open tab after the TUI stop
09-webapp-console.txt         (manual) the tab's console/network evidence
```

## 7. Re-running it

Re-run the harness **unchanged** when the TUI task and the Wails task land. The only knobs are
`PLANE_E2E_TUI_STOP_KEYS`, `PLANE_E2E_DESKTOP_CMD` and `PLANE_E2E_DESKTOP_START_KEYS` /
`PLANE_E2E_DESKTOP_STOP_KEYS` — single variables by design, so a binding change is one export, not a
rewrite.

A **missing export** is not a product defect: if a surface exists but the keystrokes or the launch
command were never configured, A0.5b/A0.6 fail against **this item** ("harness configuration") and the
run is still exit 2, because a TUI nobody drove proves nothing. That distinction is deliberate — AC12
says a failure names the task that *owns* it, and a missing export is ours.

The desktop app is launched through one helper (`launch_desktop`) that closes the previous window
before the next step, so A2's start action and A3's stop action are never decided by a stale window
from the step before it.

## 8. Filing a defect (AC12)

A failure here is **never** patched by this work item. It is filed against the sibling that owns the
`file:line`:

| if this fails | file against | the `file:line` named |
|---|---|---|
| A0.5 / A1.* TUI surface or TUI stop | **TUI task** | `internal/tui/`, `cmd/orch/main.go:3` |
| A0.6 / A2.* / A3.* desktop surface or start/stop | **Wails desktop app task** | repo root (no `wails` reference anywhere) |
| A4.* CLI verbs, `serve --status` narrowness | **CLI verbs task** | `cmd/orchicon/lifecycle.go`, `serve.go:<serveStatus>` |
| A5.3 not `binary superseded` | shared control mechanism task | `internal/planecontrol/hostproc_unix.go:<Drift>` |
| A6.3 restarted anyway when unidentifiable | shared control mechanism task | `internal/planecontrol/planecontrol.go:<Start>` (`default:` arm) |
| A6.7 a foreign holder was killed | stop-reclaims-leftovers task | `cmd/orchicon/reclaim.go:<ourInstanceProcesses>` |
| A1.5 / A4.3c / A7.3b a port still held after a stop | stop-reclaims-leftovers task | `cmd/orchicon/reclaim.go:<stopInstanceContainer>`, `services.go:190-245` |
| A8.4 the other instance's log was written | shared control mechanism task | `internal/planecontrol/planecontrol.go:<instanceEnvKeys>` |
| A9.1 an RPC appeared | whoever added it — the surface test is the tripwire | `internal/api/lifecycle_surface_test.go:<TestAPISurfaceDidNotGrow>` |
| A0.2 / A0.3 no data dir or no `ports.json` | install task | `internal/planecontrol/state.go:<ReadPorts>` |
