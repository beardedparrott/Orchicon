# The runtime-container session — acceptance runbook

This is the procedure that proves the feature's central claim end to end:

> **The same workflow runs with no container engine present, then runs inside a
> container after the project opts in** — and the booking is independently
> visible: the engine's own state, the container's own filesystem, the step's own
> hostname.

Every component of this feature can pass its own test while the claim itself stays
unreachable: a container that is created but never dispatched into, an opt-in that
does not change where work runs, a provisioning step that reports success over a
broken engine. Only the whole path catches that, so this is an **acceptance
procedure, not a component test**. It takes twenty-plus minutes and needs a
machine with **no engine installed** at step 0.

Automated form: [`scripts/runtime-e2e.sh`](../scripts/runtime-e2e.sh). Run it, or
type the steps below by hand; they are the same commands, and the harness's
evidence files are named after the step numbers below.

```bash
scripts/runtime-e2e.sh --dry-run           # print the plan, execute nothing
scripts/runtime-e2e.sh --preflight-only    # step 0 only, then stop
scripts/runtime-e2e.sh                     # the whole session, into qa-evidence/runtime-e2e/<UTC>/
scripts/runtime-e2e.sh --image-id ID --custom-image REF     # step 6 driven against a real image
```

Evidence lands in `qa-evidence/runtime-e2e/<UTC>/` (`results.tsv`, `defects.tsv`,
`summary.md`, one log per step). Exit codes: `0` every assertion passed, `1` at
least one assertion failed (**each failure names the `file:line` that owns it** —
this procedure files defects against its siblings, it never patches them), `2`
the run is not a valid proof (an engine is already installed, or a port we need is
already held) — nothing ran.

**A `SKIP` is never a pass.** The harness counts SKIPs separately in
`summary.md`, because an absence-shaped claim ("no container was created") is
trivially true of a capture that failed to run. `make runtime-e2e-assertions-test`
pins that rule offline.

---

## Before you start: the validity precondition (step 0)

The claim is *"with no engine present"*. If the machine already has `docker`,
`podman`, `nerdctl` or `docker-compose` on `PATH`, the run proves nothing and the
harness exits `2` without touching anything.

```bash
command -v docker podman nerdctl docker-compose   # must print NOTHING
```

Anything else it needs:

- **ports free**: the instance's Postgres, NATS and plane ports (dev: 5432/4222/8080).
  Held ports are exit `2` — a plane reached over a port something else holds cannot
  prove that *this instance's* plane was driven. Shift the run with
  `--pg-port/--nats-port/--control-port` instead of sharing a port.
- **disk**: ≥ 10 GiB free — an engine, two images, and a built image.
- **network on first use**: verification and the stock images pull; an air-gapped
  run passes `ORCHICON_ENGINE_VERIFY_IMAGE` a locally-present image and says so.
- **`make fe-install`** if the Runtime Images page is driven in step 6.
- A **disposable sandbox plane**, never a real instance. The plane, Postgres and
  NATS are host processes of the invoking user.

---

## The session, step by step

### 1. A local run, with no engine at all (AC1)

```bash
scripts/orchicon.sh start dev            # services + plane, host processes
# create the project with execution_mode=local, start the workflow, wait for terminal
```

**What it proves:** the run reaches `WORKFLOW_RUN_STATUS_COMPLETED` with the
project's `execution_mode` read back as `EXECUTION_MODE_LOCAL`, on a host where no
engine binary exists and the plane log carries no `workflow runtime ensured` lease.

**Blind spot if it fails:** a container appeared or an engine was invoked for a
local-mode run — owner `internal/execution/service.go:1365-1409` /
`internal/runtime/lifecycle.go:105-107`. Evidence: `01-local-run.txt`,
`01-plane-log-tail.txt`, `01-engine-ps.txt`.

### 2. Opt in — and provisioning you can watch (AC2)

```bash
# flip the project to runtime (the same update the UI performs)
ORCHICON_PROVISION_ENGINE=1 scripts/orchicon.sh restart dev   # install rungs, verbatim
curl -sS "$PLANE/enginez?refresh=1"                            # {available,state,reason,...}
```

**What it proves:** the install prints the command before running it, walks its
rungs (detect → install → minimum → `podman machine` → **verify by running a
container** → ready), and `GET /enginez` reports `available=true`, `state=ready`
**only after** the verify succeeded.

**Adversarial control (this is the half that matters):** restart with
`ORCHICON_ENGINE_VERIFY_IMAGE` set to an unrunnable ref. The install must NOT print
`container engine ready`, and `/enginez` must NOT report `ready`. If it does, the
availability surface is claiming an engine the install refused — owner
`internal/api/engine_status.go:49` + `internal/containerengine/detect.go`.

Evidence: `02-install.log`, `02-enginez.json`, `02-install-adversarial.log`,
`02-enginez-adversarial.json`.

### 3. The same workflow, now inside a container (AC3)

```bash
# start the SAME workflow with the project in runtime mode
podman ps -a --format '{{.Names}}'                       # a NEW container appeared
podman ps -a --filter label=orchicon.workflow            # it carries our labels
podman inspect --format '{{.Config.Hostname}}' <name>     # the container's own hostname
podman exec <name> hostname                               # what the work sees
hostname                                                  # the HOST's hostname
```

**What it proves:** *not merely that a container exists* — the in-container
hostname differs from the host's, so the work ran inside it. The `ps` diff is the
engine's ground truth, taken before and after the run.

**Blind spot:** `executions_.$id.tsx` carries no container field today, so the
execution detail view cannot show residency; that gap is a filed defect against
the run-view surface, never patched here. Evidence: `03-container-residency.txt`,
`03-in-container-hostname.txt`, `03-engine-ps-after.txt`.

### 4. Pristine reset (AC4)

```bash
podman exec <run-A-container> sh -c 'echo MARKER > /tmp/MARKER; mkdir /tmp/MARKER-dir'
# start a second run on the same environment, then:
podman exec <run-B-container> sh -c 'test -e /tmp/MARKER && echo PRESENT || echo ABSENT'
```

**What it proves:** state created in run A is **absent** in run B — the pristine
guarantee is the reset-then-recreate, not an in-container cleanup. A container that
still carries A's file is a FAIL naming `internal/runtime/pool.go:312-375`.
Evidence: `04-pristine-reset.txt`, `04-state-probe.txt`.

### 5. Warm reuse, and a host input forcing a fresh one (AC5)

```bash
# run with an unchanged environment -> the SAME pooled container is re-leased (no new ps entry)
# then change a host input (git_strategy) -> a NEW container name appears
```

**What it proves:** the pool key really is f(image, mounts, serve config, git
strategy, adapter demand, host fingerprint) — `internal/runtime/pool.go:98-155`.
Evidence: `05-pool-reuse.txt`.

### 6. The image path (AC6)

Create the image on the **Runtime Images** page, then drive the machine-checked
half:

```bash
scripts/runtime-e2e.sh --image-id <id> --custom-image <tag>
```

**What it proves:** the build log **streams** (`BuildRuntimeImage` is a server
stream: more than one chunk arrives), a re-deploy of an up-to-date spec says
`already up to date` (idempotent), and a run naming the image starts and pins it.
Evidence: `06-image-build.log`, `06-image-redeploy.txt`, `06-run-final.json`.

**Blind spot:** without `--image-id` those three assertions are **SKIP**, not pass
— the harness will not invent the image spec API.

### 7. The image gate (AC7)

```bash
curl --unix-socket /tmp/orchicon-runtime/runtime.sock -X POST \
  -d '{"workflow_id":"gate-probe","image":"docker.io/library/alpine:3.20"}' \
  http://localhost/v1/runtimes/            # must answer: image not allowed: ...
```

**What it proves:** the gate **rejects** an image that is neither the base, an
operator-allowlisted stock variant, nor locally-present-with-the-marker
(`internal/runtime/daemon.go:540-552`, `images.go:86-121`), and the engine lists no
container for that ref afterwards. Evidence: `07-gate-reject.txt`,
`07-engine-ps.txt`.

### 8. The all-native path (AC8) — and where the claim cannot hold

```bash
# all-native workflow, project in LOCAL mode -> no container at all
# same workflow, project in RUNTIME mode -> what actually happens, recorded
```

**What it proves (a):** an all-native workflow in `local` mode creates **no
container** — the reconciler skips `EnsureForRun` (`internal/scheduler/bridge.go:60-68`).

**Known limit (b), stated openly:** `internal/runtime/lifecycle.go:958-975` is
*always-container* — a container is created for **every** runtime-mode run
regardless of adapter kind, native sessions included. So AC8 as written cannot
hold in runtime mode on this tree. The harness records the contradiction as a FAIL
naming `internal/runtime/lifecycle.go:958-975` and files it; it does not
reinterpret AC8, and it does not patch the lifecycle. Evidence: `08-all-native.txt`.

---

## What each command proves (the "never blind" map)

| Claim | Independent evidence beside it | Owner |
|---|---|---|
| AC1 local run, no engine | `command -v` finds no engine; plane log has no lease; run status read back | `internal/execution/service.go:1365-1409`, `internal/runtime/lifecycle.go:105-107` |
| AC2 provisioning, verify-before-ready | the install's own stdout; the engine's `--version`; `/enginez` | `cmd/orchicon/install_engine.go:74-162`, `internal/api/engine_status.go:13-64` |
| AC3 ran *inside* | `podman ps` diff + labels + in-container `hostname` vs host | `internal/runtime/daemon.go:802-811,1373-1375` |
| AC4 pristine reset | the container's own `/tmp` | `internal/runtime/pool.go:312-375` |
| AC5 reuse / fresh | `podman ps` diffs across runs | `internal/runtime/pool.go:98-155`, `hostfp.go` |
| AC6 image path | the build stream itself; the run's `runtime_image` | `internal/runtimeimage/service.go:496-747` |
| AC7 gate | the daemon's rejection + the engine's `ps` | `internal/runtime/daemon.go:540-552`, `images.go:86-121` |
| AC8 all-native | `podman ps` diff, both modes | `internal/scheduler/bridge.go:60-68`, `internal/runtime/lifecycle.go:958-975` |

## What the harness refuses to call a pass

- An **empty or missing capture** can never PASS. Every content assertion is gated
  (`gated_absent`/`gated_contains`): empty ⇒ SKIP with the reason recorded.
- A **run that never became terminal** is not a completed run — the status is read
  back, never assumed from a `200` on `StartWorkflow`.
- A **container that exists** is not "the work ran inside it" — the hostnames must
  differ.
- An **engine that did not answer** is not "no container was created".
- `make runtime-e2e-assertions-test` pins all four rules, structurally, offline.

## Known limits (state them, never paper over them)

1. **AC8 in runtime mode** contradicts the always-container rule (step 8 above).
   Filed against the owning item; not patched here.
2. **AC3 from the detail view**: the execution detail view has no container field,
   so residency is proven from the engine's state and the in-container hostname.
3. **Step 6** needs an image created on the Runtime Images page first; without it
   the three image assertions are SKIP.
4. This procedure needs a host with **no engine** and permission to **install**
   one. A rootless container cannot prove it: provisioning installs a system
   package. Run step 0 on the target machine before blaming the feature.
