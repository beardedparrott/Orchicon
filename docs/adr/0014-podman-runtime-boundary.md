# ADR-0014: The Podman plane ADDRESS boundary — what changes with the engine, and what must not

## Status

Accepted. Companion to ADR-0013 (plane runtime boundary schema), which owns the
**engine seam** (`internal/runtime/engine.go`, `Engine`, `ResolveEngineFromEnv`,
`createFlagArgs`, `CreateBaseArgs`, published loopback ports, the pool shape key).
This ADR owns the half ADR-0013 leaves to the **lifecycle/plane-address** layer:
the URL an instance hands its own workers. No behaviour change for Docker installs.

## Context

`planePublicURL()` (`internal/runtime/lifecycle.go`) is what a run's minted plane
credential advertises as `ORCHICON_PLANE_URL`. It encodes a *Docker topology* in
one branch that breaks **silently**:

- **Container residency** (`ORCHICON_CONTAINER_MODE=1`): `containerIPAddress()`
  parses the plane container's `<ip> <hostname>` out of `/etc/hosts`. Rootless
  Podman (slirp4netns/pasta) — and always across the podman-machine VM boundary on
  macOS/Windows — writes no such entry, and the historical guard fallback
  `172.17.0.1` is a **Docker** bridge address that does not exist there. A run
  handed that URL does not fail at the mint; it fails inside a worker's MCP call,
  far from the cause.

What did *not* need to change, and must not: the **port** derivation
(`planeHTTPPort`) — dev and prod migrate independently and can sit on different
ports, so the `(address, port)` pair an instance advertises is what keeps one
instance's workers off another instance's plane.

## Decision

1. **The host alias is engine-INDEPENDENT** (already ADR-0013 D2.4, re-affirmed
   here because it is the plane URL's other half): the daemon emits
   `--add-host host.docker.internal:host-gateway` for **both** engines
   (`Engine.CreateBaseArgs`), so `planePublicURL`'s host-residency fallback stays
   the single spelling `http://host.docker.internal:<port>`. Podman resolves it
   through that mapping and also injects `host.containers.internal` natively; no
   per-engine branch exists in the URL string.

2. **Container residency is engine-DEPENDENT, and refuses rather than guesses.**
   `containerResidencyNeedsOverride()` (`engine.go`) is true for Podman:
   with no derivable own address, `planePublicURL()` returns **empty** instead of
   the Docker gateway. Docker keeps its historical guard unchanged.

3. **An underivable URL fails at the mint, before anything is written.**
   `planeURLForRun()` turns the empty derivation into an actionable error naming
   `ORCHICON_PLANE_PUBLIC_URL`, and `mintPlaneCredentialUncached` calls it
   **before** the transaction that creates the service identity and commits a live
   role-scoped API key — so an undeliverable plane URL can never leave an orphaned
   credential behind.

4. **Instance scoping is unchanged** and is ADR-0013's: every runtime container
   carries `orchicon.instance=<dev|prod>` (`Engine.CreateBaseArgs`) and the reapers
   filter on it with `--filter label=k=v`, which Podman and Docker accept
   identically. dev and prod sharing one daemon (or one podman machine) still never
   reap each other's runtimes.

5. **No lifecycle invariant is re-implemented per engine.** Lease-on-`pending` /
   release-on-terminal, the 30 s adopt sweep, the run-start serve gate (L1
   `/global/health` + a real session-create round-trip, bounded by
   `ORCHICON_RUNTIME_SERVE_READY_TIMEOUT` = 120 s), the serve-less `no-serve`
   sentinel short-circuit (an all-native run creates **no container**), the
   execution-liveness reaper, dispatch self-heal (`ORCHICON_SESSION_REPAIR_ATTEMPTS`
   / `ORCHICON_SESSION_INFRA_THRESHOLD`), stuck-run detection and the nil-client
   loud error from `EnsureForRun` are **engine-independent** and untouched. The
   engine decides *where a container runs* and *how it addresses the plane*, never
   *when* a container is created or reaped.

6. **Superseded on merge, deliberately.** This change originally carried its own
   engine seam, a `host.containers.internal` alias branch, a `--add-host`-less
   podman create, and a dual-written `io.orchicon.instance` label. ADR-0013's seam
   landed first and owns those decisions, so they were **dropped rather than
   resurrected**: one seam, one alias spelling, one instance-label key. Only the
   plane-address decisions above survived the merge.

## Consequences

- A container-resident plane under rootless Podman **must** set
  `ORCHICON_PLANE_PUBLIC_URL`; without it the mint fails loudly with the exact
  variable to set.
- Host-resident planes behave identically under both engines.
- Two instances can never hand a worker the other's plane: the alias is shared by
  design, the port is per instance, and the credential is instance-bound.
- A real-engine integration tier for podman is not executable in the workflow
  runtime container (no engine binary, no user namespaces), so the boundary is
  pinned by unit tests over the derivation: `plane_podman_test.go`
  (refusal + loud override + per-instance port), `plane_url_test.go` and
  `plane_reachability_test.go` (docker topology), and `lifecycle_gate_test.go`
  (serve gate + `no-serve` sentinel).
