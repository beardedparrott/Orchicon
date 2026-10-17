# ADR-0013: The plane↔runtime boundary schema (platform-valued)

Date: 2026-09-03
Status: Accepted — normative. **Blocks all Podman engine work.**
Supersedes: the *implicit* contract currently defined by Docker (see §0)
Related: `internal/runtime/daemon.go`, `internal/runtime/lifecycle.go`,
`internal/runtime/pool.go`, `internal/runtime/images.go`,
`internal/runtime/hostfp.go`, `cmd/orchicon/install_host.go`,
`cmd/orchicon/install_bridge.go`

---

## 0. Context — today the boundary is Docker's, not ours

Everything the plane and the runtime can say to each other is currently
supplied *for free* by Docker, and appears nowhere as a contract. The evidence:

| Concern | Where Docker defines it today | What we actually depend on |
|---|---|---|
| Control transport | `daemon.go:184-201` `ListenAndServe` → `net.Listen("unix", d.SocketPath)`, chmod `0660`; path from `cmd/orchicon/runtime.go:36` `ORCHICON_RUNTIME_SOCKET`, default `<tmp>/orchicon-runtime/runtime.sock`; dialled by `client.go:32-45` `NewClient` | AF_UNIX + filesystem permissions; the socket is bind-mounted into a containerized plane |
| Addressing | `daemon.go:1135` `containerIP` reads `{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`; `daemon.go:633` `--add-host host.docker.internal:host-gateway`; `lifecycle.go:544-597` `planePublicURL`; `install_bridge.go:35` `bridgeIP` | A *routable container IP on a shared L2 bridge*, and a host alias docker injects |
| File/credential ingress | `daemon.go:783` `standardHostMountArgs`, `daemon.go:656-661` `-v` loop, `--tmpfs` (`:889` `tmpfsOwned`); restriction in `daemon.go:511` `validateCreate` → `:573` `withinAllowedRoots`, roots from `runtime.go:31` `ORCHICON_RUNTIME_ALLOWED_ROOTS` | Bind mounts whose *source is a host path*, plus `-e` env for secrets |
| Instance scoping | `daemon.go:625` `--label orchicon.instance=<id>`, filtered in `daemon.go:1172` `listRuntimes`, `pool.go:455` `sweepPreRestartOrphans` | Docker labels as a queryable ownership marker |
| Pristine reset | `pool.go:301` `resetAndPool` → `rm -f` + recreate identical spec; `--tmpfs /tmp` (`daemon.go:645`) | `docker rm -f` discards the container's writable layer; tmpfs dies with it |
| Image validation | `images.go:20` `baseImageLabel = "org.orchicon.runtime-base"`, gate at `images.go:98-108` `imageAllowed` via `image inspect --format '{{index .Config.Labels ...}}'`; `spec-version` at `images.go:355`, `:377` | Label inheritance through `FROM`, readable via Docker's inspect JSON shape |

**The failure this ADR prevents** is already documented in the codebase:
`cmd/orchicon/binaries.go:33` cites `planeEnv`'s warning about *"an instance
whose shape depends on which launcher created it"*. Three engines each
growing their own ad-hoc boundary is the same defect with three launchers.

**Non-goal:** no engine code changes here (AC 9). This ADR is the contract the
engine work implements.

---

## 1. The schema, in one sentence per platform

Every value below is **single-valued**. Where a value was ever ambiguous, the
ambiguity is resolved here and the resolution is the one an implementer must
write, not a menu.

**Platform support matrix**

| | Engine | Plane residency | Control transport |
|---|---|---|---|
| **Linux** | Podman **rootless** (primary); Docker (grandfathered, same interface) | host or container | AF_UNIX |
| **macOS** | Podman in a `podman machine` VM | **host only** | AF_UNIX on the host |
| **Windows** | Podman in a `podman machine` (WSL2) VM | **host only** | **named pipe** |

Two platform-wide invariants that every concern below inherits:

- **INV-1 — No container IP crosses the boundary.** The plane never learns an
  address inside the engine's network namespace. Every plane→runtime
  connection is either (a) a request over the control transport, or (b) a
  TCP connection to `127.0.0.1:<published port>`. This single rule is what
  makes one schema cover rootless Linux, a macOS VM and a Windows VM: none of
  the three has a bridge IP the host can route.
- **INV-2 — The plane never binds a non-loopback address.** The plane listens
  on `127.0.0.1:<control port>` and nothing else. Runtime containers reach it
  through the engine's host alias (concern 2), which terminates on the host
  loopback. `ORCHICON_HTTP_EXTRA_BIND` is therefore **not set** under Podman.

---

## 2. Concern 1 — Control transport

### Docker-defined behaviour being replaced
A unix socket at `ORCHICON_RUNTIME_SOCKET` (default
`<os.TempDir()>/orchicon-runtime/runtime.sock` — `cmd/orchicon/runtime.go:36`);
`daemon.go:192` listens on it, `:199` chmods `0660`; `client.go:33-36` dials
`unix`. It is advertised to the plane purely through the env var that
`install_host.go:planeEnv` writes (`ORCHICON_RUNTIME_SOCKET=...`). For a
*containerized* plane the same path is bind-mounted in, which is why the
path had to be a stable host path under `/tmp`.

### Schema

| Linux | macOS | Windows |
|---|---|---|
| `unix://$XDG_RUNTIME_DIR/orchicon-runtime/<instance>.sock`, mode `0600` | `unix://$TMPDIR/orchicon-runtime-<instance>.sock`, mode `0600` | `npipe:////./pipe/orchicon-runtime-<instance>`, DACL = owning user SID |

**Decisions, with rationale:**

- **D1.1 — Windows uses a named pipe, not localhost TCP.** Rationale: a named
  pipe is access-controlled by the OS ACL, which is the *same* security model
  as the unix socket's filesystem mode — no new credential, no listening TCP
  port. **Security consequence, named explicitly (AC 4):** a localhost TCP
  socket has no identity, so *any* local process could reach a daemon that can
  create containers and mount host paths; it would require a bearer token
  minted and distributed by the plane, which is a credential that does not
  exist in today's model. A named pipe removes that credential entirely, at
  the cost of a Windows-only dependency (`go-winio`) behind a build tag.
- **D1.2 — The endpoint is a URL with a scheme; the scheme is mandatory.**
  Insertion point: `client.go:33` — the dial function parses
  `scheme://path` and dispatches (`unix`/`npipe`; `tcp` rejected). New file
  `internal/runtime/transport.go` holds `Dial(endpoint string) (net.Conn,
  error)` plus `DefaultEndpoint(instance string) string` (per-GOOS defaults
  above) — one place, so the three platforms cannot drift.
- **D1.3 — Advertisement is the env var, and only the env var.**
  `install_host.go:planeEnv` already writes `ORCHICON_RUNTIME_SOCKET`
  (`install_host.go:167-215`); it now writes the *schemed* URL. The daemon
  refuses to start if the env value's scheme contradicts the platform default
  scheme (fail-closed). There is **no** discovery file, no port file, no
  fallback to TCP — a second advertisement channel is exactly how two
  launchers start disagreeing.
- **D1.4 — Per-instance socket name replaces the shared path.** Today both
  instances share `runtime.sock` and are separated only by a daemon-internal
  label; the file mode `0660` is the only access control. `<instance>` in the
  path (a) gives OS-level separation as a second layer (concern 4) and
  (b) makes the socket path derived, not configured. `--instance` / the
  daemon's existing `SocketPath` field (`daemon.go:34`) is preserved as the
  override so the containerized-plane bind mount keeps working on Linux.
- **D1.5 — Host-resident plane is the only containerized-plane-free case we
  must specify.** On Linux a *containerized* plane keeps working by
  bind-mounting the socket (unchanged). On macOS and Windows the plane
  **must be host-resident**; see finding **BF-1**.

---

## 3. Concern 2 — Addressing

### Docker-defined behaviour being replaced
Three separate Docker gifts, all of which disappear:

1. `daemon.go:1135` `containerIP` — the container's bridge IP, used to build
   `CreateResponse.ServeURL` (`daemon.go:createContainer`, the
   `fmt.Sprintf("http://%s:%d", cip, port)` site) and `PlaneURL`
   (`http://<cip>:8080`, `sandboxPlanePort`). `daemon.go:647-654` deliberately
   *avoids* port publishing because `docker-proxy` binds only when the
   container's port appears, and a container→gateway connection can be dropped
   by bridge NAT.
2. `daemon.go:633` `--add-host host.docker.internal:host-gateway` — the
   runtime→plane direction, relied on by `lifecycle.go:573-597`
   (`planePublicURL`'s final branch).
3. `install_bridge.go:35` `bridgeIP` — the host plane binds the docker bridge
   address (`ORCHICON_HTTP_EXTRA_BIND`) so bridge containers can reach it
   (hairpin NAT makes the published port useless).

### Schema

| Direction | Linux (rootless podman) | macOS | Windows |
|---|---|---|---|
| plane → runtime control | AF_UNIX socket (§2) | AF_UNIX socket (§2) | named pipe (§2) |
| plane → runtime serve | `127.0.0.1:<host port>` from `podman port <name> <cport>/tcp`; container port published **explicitly on loopback**: `-p 127.0.0.1::<cport>` | same | same |
| plane → in-container sandbox plane (`PlaneURL`) | `127.0.0.1:<published port>` for the sandbox plane port, published the same way | same | same |
| runtime → plane | `http://host.docker.internal:<control port>` | same | same |
| host alias mechanism | `--add-host host.docker.internal:host-gateway` **and** `host.containers.internal` (podman sets the latter itself; we emit the former so `planePublicURL`'s string is platform-independent) | same | same |
| plane listen address | `127.0.0.1:<control port>` (INV-2) | same | same |
| `ORCHICON_HTTP_EXTRA_BIND` | **unset** | **unset** | **unset** |

**Decisions:**

- **D2.1 — Publish on loopback; never return a container IP.** `containerIP`
  (`daemon.go:1135`) is deleted from the Podman path; the response builder
  resolves the port instead. Rootless podman's container IPs live in a
  per-user network namespace and are *not host-routable*, so this is not a
  preference — the Docker approach is structurally unavailable.
- **D2.2 — The docker-proxy race that `daemon.go:647-654` avoids is answered
  by ordering, not by IPs.** Podman's rootless port forwarder
  (`rootlessport`) is created with the container, not lazily on first accept,
  so the race the comment describes does not reproduce. Belt-and-braces: the
  daemon resolves the published port **after** `startServe` returns (the
  in-container listener is already up) and validates it with the existing
  `serveUsableAt` probe (`daemon.go:1075`) before putting it in the response.
  A port that fails the probe is a hard create error — the same posture as the
  existing `startServe` failure path. If this proves racy in practice, see
  **BF-4**.
- **D2.3 — `planePublicURL` keeps its shape; its inputs change.**
  `lifecycle.go:578` order stays: (1) `ORCHICON_PLANE_PUBLIC_URL` override —
  set by `planeBindEnv` whenever runtime containers are in use (today gated on
  `if bind != ""` at `install_host.go:207`, which becomes `if url != ""`;
  §12 item 13); (2) container-mode own bridge IP —
  Linux-containerized-plane only, unchanged; (3) `ORCHICON_HTTP_EXTRA_BIND` —
  Docker-on-Linux only; (4) `host.docker.internal`. Under Podman, case 1
  supplies `http://host.docker.internal:<control port>`, case 3 never fires
  (INV-2), and case 4 is the manual-`orchicon serve` fallback. This keeps
  `plane_reachability_test.go` cases meaningful rather than deleting them.
- **D2.4 — One host alias, spelled `host.docker.internal`, on all three
  platforms.** Podman also sets `host.containers.internal`; we rely on neither
  *name* implicitly — the daemon emits `--add-host
  host.docker.internal:host-gateway` explicitly on every create, so the plane
  URL string is identical across engines and OSes and `planePublicURL`'s
  documented dependency on that arg (`lifecycle.go:573-577`) still holds.

---

## 4. Concern 3 — File and credential ingress

### Docker-defined behaviour being replaced
Two ingress channels, both Docker-supplied:

- **Bind mounts.** `daemon.go:656-661` turns each `MountSpec` into `-v
  <src>:<dst>[:ro]`; `daemon.go:783` `standardHostMountArgs` appends the
  adapter installs (`adapterHostMounts`), private adapter homes as uid-owned
  `--tmpfs` (`:889` `tmpfsOwned`), `.gitconfig`/`.git-credentials`
  (read-only, git-strategy-gated), gh config/state, `$HOME`,
  `$HOME/.local`, `$HOME/.config`, and `HOME=`; `daemon.go:createContainer`
  adds `-v <d.ExePath>:/usr/local/bin/orchicon:ro` (the never-bake binary).
- **Env.** `-e K=V` for `req.Secrets` (`daemon.go:createContainer` secrets
  loop), `GH_TOKEN` (`standardHostMountArgs`), adapter provider credentials
  (`adapterCredentialEnv`), `PATH`, `ORCHICON_ADAPTER_KINDS`.

### Schema

| | Linux | macOS | Windows |
|---|---|---|---|
| Bind mounts | `-v src:dst[:ro][,z]` — **`z` (shared SELinux relabel) added for every real bind**; tmpfs mounts never relabel | `-v src:dst[:ro]` — no relabel (no SELinux) | `-v <winpath>:dst[:ro]` — sources must be under a VM-shared location |
| Allowed-root check | `withinAllowedRoots` **and** `src` under a shared root (always true) | `withinAllowedRoots` **and** `src` under `$HOME`, `$TMPDIR`/`/private`, `/var/folders` (the `podman machine` shares `$HOME` and `/private`/`/tmp` only) | `withinAllowedRoots` **and** `src` under `%USERPROFILE%` (the WSL2 mount of `C:`) |
| Credentials by file | read-only binds, unchanged | same | same |
| Credentials by env | `-e K=V`, unchanged (podman accepts `-e`) | same | same |
| Binary ingress | `-v <stable copy>:/usr/local/bin/orchicon:ro`, unchanged (`runtime.go:copySelf`) | **must be under a shared root** — the stable copy moves from `<tmp>/orchicon-runtime/` to the same dir as the socket, which is inside `$TMPDIR` (shared) | stable copy next to the pipe is not mountable → copy into the instance data dir (shared) |
| Userns / ownership | `--userns=keep-id` + `--user <uid>:<gid>`; `--tmpfs …uid=,gid=` unchanged | same | same |

**Decisions:**

- **D3.1 — The mount-source restriction is preserved verbatim and gains a
  second, platform gate.** `validateCreate` (`daemon.go:511`) still calls
  `withinAllowedRoots` (`:573`) for every `MountSpec`; the roots still come
  from `ORCHICON_RUNTIME_ALLOWED_ROOTS` (`runtime.go:31`). **Purpose, stated
  because losing it is a security regression:** it is what stops the plane
  from asking the daemon to mount an arbitrary host path into a container it
  then execs commands in — i.e. it prevents host-filesystem read via the
  plane's own API. **New:** at daemon startup, every configured root must also
  lie inside the platform's VM-shared set (row 2 above); a root outside it is
  a **fatal** startup error, because a mount that silently resolves to an empty
  directory inside the VM is indistinguishable from a mounted-but-empty project
  — the exact class of failure this ADR exists to remove.
- **D3.2 — `:z`, not `:Z`.** `z` relabels to a shared container label; `Z` is
  private per container and would fight the warm pool, where several containers
  mount the same host `$HOME` trees. Consequence, named: on an SELinux-enforcing
  host, `z` **modifies the SELinux label on the host paths it mounts** (the
  project dir, adapter installs, `.gitconfig`, gh config). That is the standard
  rootless-podman contract and is reversible with `restorecon`; the alternative
  (`--security-opt label=disable`) weakens confinement. DECISION
  (revisitable): if a host rejects relabelling, operators set
  `ORCHICON_RUNTIME_ALLOWED_ROOTS` to a pre-labelled tree.
- **D3.3 — Credentials never traverse the control transport as a separate
  channel.** They ride inside the `CreateRequest` body (`req.Secrets`,
  `daemon.go:CreateRequest`), over the ACL-protected socket/pipe, exactly as
  today. The transport decision in §2 therefore *is* the credential-confidentiality
  decision: a named pipe inherits "only this user can talk to the daemon",
  which is what a localhost TCP socket would have lost.
- **D3.4 — The never-bake binary mount is a hard requirement on every
  platform.** `daemon.go:createContainer` already fails the create when
  `d.ExePath` is missing rather than degrading; that stays. The stable copy's
  location becomes a function of the platform (`copySelf` at
  `cmd/orchicon/runtime.go:copySelf`) so it always lands in a shareable
  directory — `<socket dir>/orchicon` on Linux/macOS, the instance data dir on
  Windows.

---

## 5. Concern 4 — Instance scoping

### Docker-defined behaviour being replaced
`daemon.go:625` writes `--label orchicon.instance=<id>` on every created
container; `daemon.go:1172` `listRuntimes` filters
`label=orchicon.instance=<instance>`; `pool.go:455` `sweepPreRestartOrphans`
uses that listing so a shared daemon never reaps another instance's
containers; `daemon.go:1152` `instanceID` normalises the empty value to
`dev`; the client sends its instance on every call (`client.go:NewClient`,
`Client.List`).

### Schema

| Layer | Linux | macOS | Windows |
|---|---|---|---|
| Container ownership marker | `--label orchicon.instance=<id>` (podman labels are identical) — **unchanged** | same | same |
| Transport namespacing | socket path `<instance>.sock`, mode `0600` | same | pipe name `orchicon-runtime-<instance>`, DACL own-SID |
| Shell-side namespacing | `orchicon-instance=<id>` label on the *plane* container (`cmd/orchicon/install.go:729`) — unrelated to the boundary, unchanged | n/a (host plane) | n/a (host plane) |

**Decisions:**

- **D4.1 — Labels stay the primary mechanism; path/pipe is the second layer.**
  Podman has labels, so the container-ownership question keeps the *same*
  answer it has today and `sweepPreRestartOrphans`'s reasoning
  (`pool.go:455-500`, including its refusal to treat empty as "all") survives
  unmodified. Deleting the label in favour of filesystem namespacing would
  lose the ability to enumerate containers and would silently widen the sweep.
- **D4.2 — Path/pipe namespacing is what replaces "labels have no
  equivalent".** The ADR's premise (named pipes have no labels) is answered
  without touching the container-ownership mechanism: the *transport* is
  per-instance, so a process holding instance A's endpoint cannot even issue a
  list request scoped to B. Both layers are single-valued, and neither is
  optional.
- **D4.3 — `instanceID("") == "dev"` is preserved as the only default.** No
  new default, no per-platform variation.

---

## 6. Concern 5 — Pristine reset

### Docker-defined behaviour being replaced
`pool.go:301` `resetAndPool`: `rm -f <old name>` **first**, then
`createContainer` with the identical spec (`image`, `mounts`, `serveCfg`,
`projectDir`, `adapterKinds`) under a fresh `poolName`, then the serve is
re-warmed. The guarantee is that nothing crosses between runs: the container's
writable layer is discarded by `rm`, `--tmpfs /tmp:rw,size=…,exec`
(`daemon.go:645`) and every `--tmpfs` `$HOME`/adapter-home (`:889`) die with
the container, and the project/worktree mounts are the *run's own* directory
by design (they are not "container state"). `pool.go:release` (the DELETE
route, `daemon.go:handleRuntime`) and `sweepPreRestartOrphans` are the other
two callers of the same removal primitive.

### Schema

**Mechanism (identical on all three platforms):**
`podman rm -f <name>` (no `--volumes` — see hard rule) → poll
`podman container exists <name>` until false (bounded, 5 s) → create with the
byte-identical spec under a fresh `poolName` → re-warm serve → probe with
`serveUsableAt` (`daemon.go:1075`) before the entry re-enters the clean pool.
A failed reset drops the entry (`pool.go:resetAndPool` already logs and drops);
the next checkout creates fresh. That keeps `pool_test.go` /
`pool_lease_test.go` semantics.

**HARD RULE — no named volumes, ever, for runtime containers.** Pristine reset
is *derived* from "the container's writable state is its own layer plus
tmpfs". A named volume on `$HOME`/`/tmp`/adapter homes would survive `rm` and
leak state between runs. Enforced by a create-arg assertion (no `-v` whose
source is a volume name; every `-v` source must pass `withinAllowedRoots`, which
already implies a host path) — this assertion is new and is named in the
implementation list.

**Decisions / findings:**

- **D5.1 — `rm -f` before create, never "restart and clean".** Restarting a
  container and emptying directories inside it cannot be proven to remove
  forgotten state (stray files outside the cleaned set, process-level caches,
  the serve's in-memory config). Removal is the only mechanism with a
  defensible guarantee, and it is what Docker gave us for free.
- **D5.2 — The name-conflict retry predicate must cover podman's wording.**
  `createContainer`'s retry loop matches `"Conflict"`, `"in progress"`,
  `"is already"` (`daemon.go:createContainer` retry block). Podman emits
  `name is already in use` and (after `rm -f`) can emit `container is in use`.
  The predicate is extended to include `"already in use"` and `"is in use"`;
  without it, a reset on macOS/Windows (VM-backed storage, slower teardown)
  fails the create and silently churns containers. This is a *behavioural*
  requirement of the schema, not a nicety.
- **D5.3 — Blocking findings for pristine reset:** the guarantee holds only
  under the stated preconditions. Recorded as **BF-2** (`--userns=keep-id` vs
  host-uid mapping) and **BF-3** (no named volumes). If either precondition
  fails on a target platform, pristine reset is **blocked** for that platform —
  not "best effort".

---

## 7. Concern 6 — Image validation

### Docker-defined behaviour being replaced
`images.go:20` `baseImageLabel = "org.orchicon.runtime-base"`; the create gate
`images.go:98-108` `imageAllowed` treats an image as usable iff it is the
configured base, is operator-allowlisted (`d.Images`), or
`docker image inspect --format '{{index .Config.Labels
"org.orchicon.runtime-base"}}'` returns `true`. `images.go:355`
`handleImageInspect` and `images.go:140` `ImageInfo` read
`org.orchicon.runtime.spec-version` the same way. The label is *inherited*
through `FROM`, and `rewriteDockerfileBase` (`images.go:364-379`) guarantees
every UI-built image carries it (`LABEL …=true` injected, `FROM` rewritten).

### Schema

| | Linux / macOS / Windows (podman) | Docker (grandfathered) |
|---|---|---|
| Gate query | `podman image inspect --format '{{index .Labels "org.orchicon.runtime-base"}}' <ref>` | `docker image inspect --format '{{index .Config.Labels …}}'` |
| Accept condition | value `== "true"` | identical |
| On inspect error / image absent | **reject** (`imageAllowed` returns false) | identical |
| `spec-version` read | `{{index .Labels "org.orchicon.runtime.spec-version"}}` | `{{index .Config.Labels …}}` |
| Build-time enforcement | unchanged: `rewriteDockerfileBase` + `podman build -t <tag> <ctx>`; `podman image prune -f --filter dangling=true` after success | `docker build` |
| Allowlist source | `d.Image` + `ORCHICON_RUNTIME_IMAGES` (`runtime.go:77`, `images.go:allowedImages`) — engine-independent | identical |

**Decisions:**

- **D6.1 — The gate is preserved with its purpose stated (AC 5).** It is what
  stops the plane from getting the daemon to *create a container from an
  arbitrary image* — which, combined with mount ingress and `docker exec`, would
  be host code execution as the operator's uid. Losing it is a security
  regression, not a simplification.
- **D6.2 — Podman's inspect JSON shape differs and the difference is
  load-bearing.** Docker nests labels under `.Config.Labels`; podman exposes
  `.Labels` at the top level. A copy-paste of the docker format string under
  podman yields `""` on a perfectly valid image — **fail-closed**, so the
  symptom is "every custom image is mysteriously rejected", not a bypass. The
  format string is therefore an *engine method*, not a constant: new
  `Engine.ImageLabel(ref, label string) (string, error)` with the docker and
  podman bodies living next to each other so neither can be edited alone.
- **D6.3 — Label inheritance is engine-independent.** Both engines inherit
  `LABEL`s through `FROM`, and the daemon never trusts an author's Dockerfile
  (`FROM` is stripped and re-emitted, `images.go:364-379`). No schema change is
  required; the requirement is stated so a future engine cannot quietly drop
  the rewrite.

---

## 8. The host-input fingerprint under the new boundary (AC 7)

**Role restated.** The fingerprint exists for one reason: a warm container
bakes read-once host inputs at create time — the daemon binary (`d.ExePath`,
content-hashed), opencode config, provider auth, adapter install trees, the
resolved GH token, the serve config, the git strategy and the boot profile —
and *none of them can change on a live container*. `hostInputsFingerprint`
(`daemon.go:972` → `hostfp.go:hostInputsFingerprint`) folds them into
`poolEnvKey` (`pool.go:96`), so any change forces a miss and a fresh create
instead of serving a stale value. `pool.go:resetAndPool` **re-derives** the key
from the current host state rather than reusing `old.envKey`, which is what
keeps a reset honest.

**Under the new boundary the fingerprint gains two terms, and both are
mandatory:**

- **D7.1 — `engine=<name>@<version>`.** A container created by docker cannot be
  managed by podman (different storage graph), and a podman upgrade changes
  create/exec semantics. The term is derived from
  `podman version --format {{.Client.Version}}` / `docker version --format …`,
  resolved once per daemon process and folded into `poolEnvKey` next to the
  existing `img=`/`mounts=`/`cfg=`/`gs=`/`demand=`/`host=` terms. Without it, an
  engine switch on a running host would hand out containers the new engine
  cannot describe.
- **D7.2 — The mount *set*, not just the mount-spec list, stays in the key.**
  Already true (`poolEnvKey` hashes `MountSpec`s plus the `ServeConfig`); the
  platform `:z` option and `--userns` flag are create-time shape and therefore
  also belong in the key as a `shape=` term so a schema change cannot reuse a
  warm container created under the old shape.

Nothing else about the fingerprint changes: it is still daemon-side, still
computed from `d.HostHome`/`d.ExePath` (the same trust domain as the mounts),
still gated on the same existence checks as `createContainer`, and still never
contains secret content (config/auth feed digests; the GH token feeds
length+prefix only — `hostfp.go:ghTokenFingerprint`).

---

## 9. Blocking findings (recorded, not deferred — AC 6)

These are preconditions the engine work must *prove* before the corresponding
schema cell is implemented. They are not risks to monitor; each one, if it
fails, converts that cell into a blocked platform.

- **BF-1 — Containerized plane on macOS/Windows has no supported transport.
  BLOCKING for that shape only.** The plane↔daemon socket is mounted into the
  plane container on Linux. On macOS/Windows the daemon (and its socket) live
  on the *host*, while containers live in a VM; exposing a host AF_UNIX socket
  into a VM-backed container is not a supported, stable `podman machine` path.
  **Consequence (accepted in this ADR):** macOS and Windows are host-plane-only.
  A containerized plane there would force the transport to localhost TCP + a
  bearer token — a different schema, so it must be a new ADR, not a patch.
- **BF-2 — Rootless uid mapping vs the mount contract. BLOCKING until
  verified on each target podman version.** The ingress schema assumes
  `--userns=keep-id` makes the runtime uid the invoking host user so the
  project/worktree mounts are writable at their *own* uid (today's `--user
  <uid>:<gid>` semantics under Docker). If the host's podman version/subuid
  config cannot provide that mapping, every mount row in §4 (and the
  `tmpfsOwned` ownership rule at `daemon.go:889`) is unproven, and the
  "container writes into the project dir" contract is at risk.
- **BF-3 — Pristine reset depends on per-container writable-layer isolation.
  BLOCKING for any platform that fails it.** §6's guarantee is derived from
  "`rm -f` discards the writable layer; tmpfs dies with the container". Two
  preconditions: (a) **no named volumes** on runtime containers (hard rule,
  §6); (b) the engine must not reuse an overlay upper between containers with
  the same pool name. If either fails on a target, pristine reset has **no**
  mechanism there and the platform is blocked — it may not be shipped as
  "best effort".
- **BF-4 — Published-port forwarding replaces the container IP
  (§3 D2.2). Not blocking, but bounded.** If `podman port` resolution plus the
  `serveUsableAt` probe proves racy under rootless networking, the fallback
  (drive the serve purely through the daemon's exec/stdio routes, i.e. no
  direct HTTP from the plane) changes the addressing schema — a re-review of
  this ADR, not a local workaround.

---

## 10. Test review (AC 8) — what must change, enumerated before engine work

The tests pin the boundary. §10 classifies each one against the schema so the
engine task cannot quietly re-baseline a guarantee.

**Decision D10.1 — `d.dockerFn` (`daemon.go:77`) stays the test seam; the
Engine is a thin argv/format provider above it.** This is what keeps the
majority of the suite green: the injected `dockerFn` still records the argv the
daemon would have run, so existing argv assertions keep working while the
Engine chooses *which* argv.

| Test (file:line) | Verdict | Why |
|---|---|---|
| `daemon_mounts_test.go:100` `TestStandardHostMountArgsConditionalOpencode` | **keep**, extend | argv flags (`-v`, `--tmpfs`, `-e`) are shared by podman; add engine cases |
| `daemon_mounts_test.go:168` `…OpencodePrivateDataHome` | **keep** | `--tmpfs …uid=,gid=,mode=` is engine-neutral |
| `daemon_mounts_test.go:194` `…NoHostHome` | **keep** | unchanged |
| `daemon_mounts_test.go:204` `…GitStrategyGate` | **keep** | unchanged |
| `daemon_mounts_test.go:274` `…ClaudeRWAndPath` | **keep** | unchanged |
| `daemon_mounts_test.go:325` `TestClaudeCreatePreflightFailsActionably` | **keep** | pre-auth check, no engine surface |
| `daemon_secrets_test.go:50` `TestCreateContainerInjectsSecretsAsEnv` | **keep** | `-e K=V` identical under podman |
| `hostfp_test.go:28` `TestDaemonBinaryFingerprintInvalidatesPoolKey` | **MUST CHANGE** | the key gains `engine=` + `shape=` (§8 D7.1/D7.2); fixtures must assert the new terms |
| `hostfp_test.go:87` `TestHostInputsFingerprintStabilityAndInvalidation` | **keep** | pure `hostfp.go`, no engine |
| `hostfp_test.go:169` `…EmptyAndExclusions` | **keep** | unchanged |
| `hostfp_test.go:193` `TestGhTokenFingerprint` | **keep** | unchanged |
| `hostfp_test.go:228` `TestGhTokenTTLCache` | **keep** | unchanged |
| `hostfp_test.go:312` `TestPoolCheckoutReuseAndInvalidation` | **MUST CHANGE** | golden pool keys, same reason as `:28` |
| `hostfp_test.go:444` `TestAdapterInstallFingerprintCoversClaude` | **keep** | unchanged |
| `pool_lease_test.go:90/152/178/209` (`TestLeaseRenewalKeepsLiveRun`, `…StaleLeaseContainerRemoved`, `…DanglingLeaseMappingReaped`, `…LeaseMaxAgeOverride`) | **keep**, re-run | assert `rm`/lease behaviour through the seam; removal primitive name is engine-supplied |
| `pool_sweep_test.go:74` `TestResetPoolTouchesNoDocker` | **keep** | asserts zero engine calls — still the right assertion |
| `pool_sweep_test.go:88` `TestSweepIsInstanceScoped` | **keep** | asserts `label=orchicon.instance=dev` appears in argv (§5 D4.1 preserves labels) |
| `pool_sweep_test.go:116` `TestSweepRefusesEmptyInstance` | **keep** | unchanged |
| `pool_sweep_test.go:131` `TestSweepReapsUntrackedOnly` | **keep** | unchanged |
| `pool_sweep_test.go:151` `TestSweepRunsOncePerInstance` | **keep** | unchanged |
| `pool_sweep_test.go:172` `TestSweepRetriesAfterAFailedListing` | **keep** | unchanged |
| `lifecycle_union_test.go:34` `TestBuildCreateRequestBakesRunUnionForTwoSteps` | **keep** | request baking, no engine |
| `lifecycle_union_test.go:212` `TestRunServeConfigProviderIsDeterministicForARun` | **keep** | unchanged |
| `plane_reachability_test.go:36` `TestPlaneHTTPPort` | **keep** | unchanged |
| `plane_reachability_test.go:59` `TestPlanePublicURLDerivation` | **MUST CHANGE** | add the podman case: `ORCHICON_HTTP_EXTRA_BIND` unset + no bridge bind ⇒ `http://host.docker.internal:<port>` (§3 D2.3/INV-2) |
| `plane_url_test.go:27/50` | **keep** | override + non-empty assertions hold |
| `images_test.go:13/34/55` (`TestRewriteDockerfileBase*`) | **keep** | pure text rewrite |
| `images_test.go:66` `TestImageTagPattern`, `:80` `TestSlugPattern` | **keep** | unchanged |
| `images_test.go:95` `TestAllowedImagesAlwaysIncludesBase` | **keep** | unchanged |
| *(new)* `images_test.go` `TestImageLabelFormatPerEngine` | **ADD** | pins `.Config.Labels` (docker) vs `.Labels` (podman) — §7 D6.2 |
| `install_host_test.go:146` `TestPlaneEnvIsPerInstanceAndComplete`, `:206` `TestPlaneBindAndShape` | **MUST CHANGE** | the bind/url pair must be empty (bind) and `host.docker.internal:<control>` (url) under the podman engine. The emitting gate is `if bind != ""` (`install_host.go:207`), which must become `if url != ""` or the podman URL is never written |
| `install_host_test.go:560` `TestInstallHostHasNoDockerInvocation` | **MUST CHANGE** | `containerEngineAvailable` (`install_host.go:106`) probes `exec.LookPath("docker")` only; the test's banned-callee list rejects the literal `"podman"` and pins that exact LookPath, so a podman-aware probe reads as a violation until the test is told podman is an engine *probe*, never an engine *call* |
| *(new)* `daemon_transport_test.go` | **ADD** | `DefaultEndpoint` per-GOOS + scheme parsing + rejection of `tcp` (§2 D1.2/D1.3) |
| *(new)* `daemon_createargs_test.go` | **ADD** | no named-volume mount may appear in the create argv (§6 hard rule) |

**Summary of what changes:** exactly five existing tests (`hostfp_test.go:28`,
`hostfp_test.go:312`, `plane_reachability_test.go:59`,
`install_host_test.go:146`+`:206`, `install_host_test.go:560`) plus three new ones. Every other boundary
test is preserved as-is — because the schema deliberately preserves labels,
the mount argv, and the remove-and-recreate reset, which is what those tests
pin.

---

## 11. Consequences

- The engine becomes swappable behind one interface, and no platform can grow
  a private variant of any of the six concerns: ambiguity was resolved here,
  in writing, per OS.
- **Net security posture improves:** the socket is per-instance and `0600`
  (was a shared `0660` under a world-writable `/tmp`); the Windows transport is
  ACL'd rather than an unauthenticated TCP port; the plane no longer binds a
  bridge-reachable address; and both the mount-source restriction and the
  image gate are preserved with a stated purpose and a format-string pin.
- **Cost:** published loopback ports replace direct container IPs, so the
  launcher must publish and resolve ports it previously did not need
  (`containerIP` disappears from the podman path).
- **macOS/Windows lose a shape:** containerized plane is not supported there
  (BF-1). That is a scope reduction recorded as a finding, not a silent gap.
- Rootless Linux hosts get `:z` relabelling on the host paths they mount —
  a visible, reversible host-side effect (D3.2).

---

## 12. Implementation list (mechanically executable, in order)

Nothing below belongs to *this* task (AC 9: contract only). Each item names the
file, the insertion point, and the schema decision it implements.

1. **Create `internal/runtime/engine.go`** — the interface the rest depends on:
   `type Engine interface { Name() string; Exec(ctx, args ...string) (string, error);
   CreateArgs(spec ContainerSpec) ([]string, error); ImageLabel(ref, label string)
   (string, error); PublishedPort(name string, containerPort int) (int, bool);
   Reset(name string) error; List(instance string) ([]string, error);
   Version(ctx) string; MountOption(ro bool) string; UsernsArgs() []string }`,
   plus `ContainerSpec` mirroring today’s `CreateRequest`-derived argv. Engine
   selection from `ORCHICON_RUNTIME_ENGINE` (`docker|podman`, default: podman on
   darwin/windows, docker on linux-as-today until the switch).
2. **Create `internal/runtime/engine_docker.go`** — body = today’s constants:
   `--add-host host.docker.internal:host-gateway` (`daemon.go:633`), no `-p`
   (`daemon.go:647-654`), `{{index .Config.Labels "X"}}` (`images.go:107`),
   `rm -f`, `ps -a --filter label=…` (`daemon.go:1172`). Returns
   `MountOption(ro)` = `":ro"` and `UsernsArgs()` = `nil`.
3. **Create `internal/runtime/engine_podman.go`** — implements §3/§4/§6/§7:
   `MountOption` = `":ro,z"` / `":z"` (D3.2); `UsernsArgs` = `--userns=keep-id`
   (BF-2); create args add `-p 127.0.0.1::<servePort>` and
   `-p 127.0.0.1::<sandboxPlanePort>` (D2.1); `PublishedPort` parses
   `podman port <name> <cport>/tcp`; `ImageLabel` uses
   `{{index .Labels "X"}}` (D6.2); `Reset` = `rm -f` + exists-poll (D5.1);
   retry predicate adds `"already in use"`/`"is in use"` (D5.2).
4. **Route `Daemon.docker` (`daemon.go:1213`) through `Engine.Exec`**, keeping
   `dockerFn` (`daemon.go:77`) as the test seam (D10.1) so argv-asserting tests
   (`daemon_mounts_test.go`, `daemon_secrets_test.go`, `pool_sweep_test.go`)
   stay green.
5. **`internal/runtime/transport.go` (new)** — `Dial(endpoint string)` and
   `DefaultEndpoint(instance string)` (§2 D1.2) with `unix` for all POSIX and
   `npipe` for windows; reject `tcp` (D1.3). Build-tagged dial implementations:
   `transport_npipe_windows.go` using `go-winio` with a security descriptor
   restricted to the owning SID (D1.1).
6. **`internal/runtime/daemon.go:184` `ListenAndServe`** — replace
   `net.Listen("unix", …)` (`:192`) + `Chmod(0660)` (`:199`) with
   `transport.Listen(d.SocketPath)`, mode `0600`, per-instance default path
   (D1.4); log the schemed endpoint at `:229`. Refuse startup when the env
   scheme contradicts the platform default (D1.3).
7. **`internal/runtime/client.go:32` `NewClient`** — dial via
   `transport.Dial(endpoint)`; keep `instance` scoping (`Client.List`) unchanged
   (D4.1).
8. **`internal/runtime/daemon.go:createContainer`** — replace the hardcoded
   `--add-host` (`:633`), the `-v` loop (`:656-661`), `--tmpfs` (`:645`,
   `tmpfsOwned` `:889`) and the binary mount with `Engine.CreateArgs(spec)`;
   drop `containerIP` from the response builder and set `ServeURL`/`PlaneURL`
   from `Engine.PublishedPort` + `serveUsableAt` (`:1075`) (D2.1/D2.2). Delete
   `containerIP` (`daemon.go:1135`) from the podman path.
9. **`internal/runtime/daemon.go:511` `validateCreate`** — add the
   platform-shared-root gate on top of `withinAllowedRoots` (`:573`), and the
   no-named-volume assertion (D3.1, §6 hard rule). Startup-side: validate
   `AllowedRoots` against the shared set in `cmd/orchicon/runtime.go:31` and
   fail fatally (D3.1).
10. **`internal/runtime/images.go`** — replace both `image inspect` format
    strings (`:107`, `:355`) and `d.DockerBin build/rmi/prune`
    (`images.go:handleBuild`, `handleImages`) with `Engine.ImageLabel` /
    `Engine.Exec`; keep `rewriteDockerfileBase` (`:364-379`) and the
    fail-closed reject on inspect error (D6.1–D6.3).
11. **`internal/runtime/pool.go:96` `poolEnvKey`** — add `engine=<name>@<version>`
    and `shape=<mountoption|userns>` terms (D7.1/D7.2); `resetAndPool` (`:301`)
    calls `Engine.Reset` instead of `d.docker("rm", "-f", …)`;
    `sweepPreRestartOrphans` (`:455`) calls `Engine.List` and keeps the
    label filter verbatim (D4.1).
12. **`internal/runtime/hostfp.go` / `daemon.go:972` `hostInputsFingerprint`** —
    fold the engine identity/version once per process; no change to the digest
    inputs or the secret-non-exposure rules (§8).
13. **`cmd/orchicon/install_host.go:146` `planeBindEnv` / `:167` `planeEnv`** —
    under the podman engine return `bind=""` and
    `url="http://host.docker.internal:<control>"` (INV-2, D2.3). Two edits ride
    with it: the emit gate `if bind != ""` (`install_host.go:207`) becomes
    `if url != ""`, and `containerEngineAvailable` (`install_host.go:106`)
    probes `podman` as well as `docker` (still a `LookPath`, never an engine
    call). Write the schemed `ORCHICON_RUNTIME_SOCKET` at `:195` (D1.3).
    **`install_bridge.go:35` `bridgeIP` is not called on the podman path.**
14. **`cmd/orchicon/runtime.go`** — engine selection + daemon struct wiring
    (`:75` `DockerBin` → `Engine`; `:77` `Images`), socket default per §2, `copySelf`
    destination inside a shared root (D3.4).
15. **Tests** — apply §10 exactly: modify the five listed files, add the three
    new ones. Do not regenerate golden pool keys wholesale.
16. **Findings BF-1…BF-4** — BF-2 and BF-3 must be proven on each target
    platform (a manual create/exec/mount/rm script is sufficient) *before* the
    platform is declared supported; BF-1 is recorded as a scope limit; BF-4 is
    proven by the `serveUsableAt`-gated checkout.

---

## 13. Citation audit (engineering pass)

Every `file:line` in this ADR was re-read against the tree that carries it. The
§10 test table was confirmed **exactly** right (all 30-odd `func Test…` line
numbers match). The following citations had drifted and are corrected in place;
the list exists so a reviewer can tell a stale line from a contested claim:

| Subject | Was | Now | What is actually at that line |
|---|---|---|---|
| socket default | `runtime.go:35` | `runtime.go:36` | `ORCHICON_RUNTIME_SOCKET` default |
| `NewClient` + its dial | `client.go:35-48`, dial `:36-39` | `:32-45`, dial `:33-36` | `func NewClient`, the `unix` dial closure |
| `-v` loop | `daemon.go:660-666` | `daemon.go:656-661` | `for _, m := range req.Mounts` |
| `--tmpfs /tmp` | `daemon.go:646` | `daemon.go:645` | the `/tmp:rw,size=…,exec` arg |
| no-publish rationale block | `daemon.go:648-654` | `daemon.go:647-654` | the "NO port publish" comment |
| image-inspect format | `images.go:106` | `images.go:107` | `{{index .Config.Labels …}}` |
| `rewriteDockerfileBase` | `images.go:363-379` | `images.go:364-379` | func + injected `LABEL …=true` |
| `ORCHICON_RUNTIME_IMAGES` wiring | `runtime.go:81` | `runtime.go:77` | the `Images:` field |
| test seam | `daemon.go:76` | `daemon.go:77` | the `dockerFn` field |
| `DockerBin` wiring | `runtime.go:85` | `runtime.go:75` | `DockerBin: "docker"` |

**One substantive correction, not a line number.** §10 originally cited
`install_host_test.go:160`/`:214`, which are *call sites inside* tests. The row
now names the tests (`:146` `TestPlaneEnvIsPerInstanceAndComplete`, `:206`
`TestPlaneBindAndShape`) and adds a **fifth** must-change test,
`:560` `TestInstallHostHasNoDockerInvocation`: it rejects the literal `"podman"`
as a callee and *pins* `exec.LookPath("docker")` as the engine probe, so a
podman-aware `containerEngineAvailable` (`install_host.go:106`) fails it until
the test is taught that podman may be probed and never executed. §12 item 13
carries the matching insertion points — including the `if bind != ""` emit gate
at `install_host.go:207`, without which the podman URL is computed and then
discarded.

**Scope check (AC 9):** `git diff --stat develop...HEAD` for this branch touches
this file only. No engine code was written, which is the point — the contract is
the deliverable, and the implementer works from §12.

---

## 14. Acceptance-criteria trace

| AC | Where it is answered |
|---|---|
| 1 — all six concerns × three platforms in an ADR | §2–§7; the platform matrix in §1 fixes the three columns |
| 2 — current Docker behaviour stated per concern | each §N opens with "Docker-defined behaviour being replaced" |
| 3 — single-valued per platform | §1 ("Every value below is single-valued") + the per-concern tables; ambiguity is resolved in the D-numbers, not deferred |
| 4 — Windows transport decided + security consequence | §2 D1.1 (named pipe + own-SID DACL; the TCP alternative's missing credential is named) |
| 5 — image gate + mount restriction preserved with purpose | §4 D3.1 (mount-source restriction, purpose stated), §7 D6.1 (image gate, purpose stated), §7 D6.2 (format-string pin) |
| 6 — pristine reset mechanism + blocking findings | §6 (mechanism + hard no-named-volumes rule + D5.2 retry wording), §9 BF-2/BF-3 |
| 7 — host-input fingerprint restated | §8 (role + mandatory `engine=`/`shape=` terms) |
| 8 — tests reviewed, changes enumerated before engine work | §10 (per-test verdicts, five must-change, three new) + §13 audit |
| 9 — no engine code | §0 non-goal; verified on the branch (`docs/` only) — §13 scope check |
