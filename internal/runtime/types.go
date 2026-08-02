// Package runtime implements the workflow runtime-container layer.
//
// Model (Azure Pipelines self-hosted agent): one short-lived container
// per active workflow run. It is created when a workflow run leaves
// `pending`, all executions for that workflow are dispatched into it,
// and it is killed when the run reaches a terminal state
// (completed/failed/aborted). Everything it contains — installed tools,
// caches, sessions — is ephemeral and wiped on teardown.
//
// Security model: the runtime container runs as the HOST user's uid
// (ORCHICON_HOST_UID, default 1000) with no root process at all. The
// image is built with the rootfs chowned to that uid, so workers have
// full write control over the ephemeral filesystem (they can install
// tools with pip/npm/mise/uv/curl) while any bind-mounted project
// directory is written as the host user — never as root. There is no
// privilege escalation path to the host, and a worker cannot chown,
// chmod, or bypass ownership on the mounted project volumes.
//
// Components:
//
//   - runtime-daemon  (host process): owns the Docker socket. Serves a
//     narrow HTTP API over a unix socket (mounted into the supervisor
//     container) — create/kill runtime containers, exec into them,
//     list adoptable containers. Validates every request (image, mount
//     roots, argv allowlist) so the control plane can never ask for an
//     arbitrary container.
//
//   - runtime-supervisor (PID 1 inside each runtime container): listens
//     on a unix socket inside the container, runs `opencode run` as a
//     child, streams stdout/stderr back, and signals children by
//     exec_id. Builds the execution guard shim in-container so the
//     worker runs under the same safety guard as the in-process path.
//
//   - runtime-client (in-container): forwards a dispatch request from
//     the daemon (via `docker exec`) to the supervisor socket and
//     relays streamed events back. Kept as a separate invocation so the
//     daemon never needs shell-level access to the container.
package runtime
