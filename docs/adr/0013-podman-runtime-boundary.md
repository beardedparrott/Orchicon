# ADR-0013: The Podman runtime boundary — one engine seam, per-engine plane address, unchanged scoping

## Status

Accepted. Adds Podman as a first-class container engine for the runtime plane. No behaviour change for Docker installs.

## Context

The runtime layer (`internal/runtime/lifecycle.go`, `daemon.go`, `pool.go`) was written against Docker and encodes **three Docker-specific assumptions** in places that break *silently*:

1. **The engine binary was hardcoded.** `cmd/orchicon/runtime.go:75` set `Daemon.DockerBin = "docker"`. Every exec site (`daemon.go:1213 docker(args...)`, `:372/:441/:1016/:1196` exec, `images.go:216` build) funnels through that one field, so the seam already existed — it just could not be pointed anywhere.

2. **A host-resident plane was reached by a Docker-only name.** `planePublicURL()` returned `http://host.docker.internal:<port>` with no extra bind, relying on `--add-host=host.docker.internal:host-gateway` at container create (`daemon.go:633`). Inside a Podman container that name does not resolve; Podman injects `host.containers.internal` instead. On macOS/Windows the runtime containers live in the *podman machine* VM, so the host alias is the only address that crosses the VM boundary at all.

3. **A container-resident plane derived its own address from `/etc/hosts`.** `containerIPAddress()` parses `<ip> <hostname>`. Rootless Podman (slirp4netns/pasta) writes no such entry, and the historical guard fallback `172.17.0.1` is a **Docker** bridge address that does not exist there — a URL that can only time out, far from the cause.

What did *not* need to change, and must not: instance scoping. Runtime containers are labelled `orchicon.instance=<dev|prod>` (`daemon.go:625`) and the reapers filter with `--filter label=orchicon.instance=<instance>` (`daemon.go:1175`). That is also valid Podman syntax, so two instances sharing one daemon (or one podman machine) still never reap each other's runtimes.

## Decision

1. **One engine seam: `internal/runtime/engine.go`.** `engineKind()` resolves `EngineDocker` / `EnginePodman` from `ORCHICON_RUNTIME_ENGINE` first, then from the basename of `ORCHICON_CONTAINER_BIN`, defaulting to Docker. The env var wins because the binary name is *not* evidence of the engine (`podman-docker` ships a `docker` shim; Docker Desktop ships `docker` in front of a VM). An unrecognised value falls through to Docker rather than silently selecting the other engine's networking.

2. **The daemon's exec target is derived, never literal.** `cmd/orchicon/runtime.go` sets `DockerBin: runtime.EngineBinary()`; every existing exec site is unchanged.

3. **The plane host alias is per engine.** `planeHostAlias()` returns `host.docker.internal` (Docker) or `host.containers.internal` (Podman), overridable with `ORCHICON_PLANE_HOST_ALIAS` for a remote/custom engine. `planePublicURL()` uses it for the host-residency fallback; the **port derivation (`planeHTTPPort`) is untouched**, so an instance can still only ever hand out its own `(address, port)` pair.

4. **The create arg follows the engine.** `hostGatewayArgs()` maps the alias to `host-gateway` under Docker and returns nothing under Podman (Podman injects `host.containers.internal` natively; older builds reject the `host-gateway` special value, which would fail container creation).

5. **Podman container residency requires an explicit override.** `containerResidencyNeedsOverride()` is true for Podman: with no derivable IP, `planePublicURL()` returns **empty** rather than a Docker gateway, and `mintPlaneCredential` then fails **loudly** naming `ORCHICON_PLANE_PUBLIC_URL`. Docker keeps its historical guard. A wrong plane URL is worse than no plane URL — it fails inside a worker's MCP call, not at the mint.

6. **Instance scoping is dual-written, filter unchanged.** `instanceLabelArgs()` writes `orchicon.instance=<v>` **and** `io.orchicon.instance=<v>` (reverse-DNS form for external tooling: `podman ps --filter label=io.orchicon.instance=dev`). The daemon keeps filtering on the historical key, so switching engines cannot make existing containers invisible to the reapers.

7. **No invariant is re-implemented per engine.** Lease-on-`pending`/release-on-terminal, the 30 s adopt sweep, the run-start serve gate (L1 health + session-create, `ORCHICON_RUNTIME_SERVE_READY_TIMEOUT` = 120 s), the serve-less/no-serve short-circuits, the execution-liveness reaper, dispatch self-heal (`ORCHICON_SESSION_REPAIR_ATTEMPTS` / `..._INFRA_THRESHOLD`), stuck-run detection and the nil-client loud error are **engine-independent** and untouched. The engine seam changes *where a container runs* and *how it addresses the plane*, never *when* a container is created or reaped.

## Consequences

- Podman becomes selectable with `ORCHICON_RUNTIME_ENGINE=podman` (or a `podman`-named `ORCHICON_CONTAINER_BIN`); Docker installs are bit-for-bit unchanged, including the shipped default.
- A container-resident plane under rootless Podman **must** set `ORCHICON_PLANE_PUBLIC_URL` — the failure is a loud, actionable mint error instead of a run whose plane-channel MCP calls all time out.
- Host-resident Podman works without extra flags: containers dial `host.containers.internal`, which Podman injects. Across a podman machine VM this is the only address that works.
- Instance scoping needs no key change; external tooling gets a conventional label without the reapers losing theirs.
- A full engine-agnostic *integration* test tier (real podman, real container) is not executable in the workflow runtime container (no engine binary, no user namespaces), so the seam is pinned by unit tests over the resolvers, the create arg list and the plane-URL derivation (`engine_test.go`, `plane_podman_test.go`, `instance_label_test.go`, `lifecycle_gate_test.go`).
- `orchicon.instance` remains the canonical key. If it is ever renamed, the dual write means only the filter has to move — the labels on live containers already carry both.
