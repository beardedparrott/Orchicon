# Orchicon workflow-runtime base image.
#
# One container per active workflow run (Azure Pipelines self-hosted
# agent model): created when a workflow run leaves `pending`, all
# executions for that workflow are dispatched into it, and it is killed
# when the run reaches a terminal state. Everything inside is ephemeral.
#
# Security model — no root process at all:
#   - The container runs as the HOST user's uid (ORCHICON_HOST_UID,
#     default 1000), so any bind-mounted project directory is written as
#     the host user, never as root, and there is no privilege-escalation
#     path to the host.
#   - To give workers full control over the ephemeral filesystem, the
#     rootfs is chowned to that uid at build time. Workers can write
#     anywhere in the container (install tools with pip/npm/mise/uv/curl,
#     drop files into /etc or /usr) — all of it lands in the ephemeral
#     overlay and is wiped when the runtime container is killed.
#   - dpkg hard-refuses to run as non-root, so system packages are baked
#     here at build time; runtime installs use user-space package
#     managers (PIP_BREAK_SYSTEM_PACKAGES, npm with a user prefix,
#     mise/uv) which write into the chowned rootfs without a superuser.
#
# The image ships the opencode runtime (the adapter drives it with
# `opencode run --format json`). The orchicon binary (`runtime-supervisor`
# / `runtime-client`) is deliberately NOT baked in — the daemon bind-mounts
# its own executable read-only at /usr/local/bin/orchicon when it creates a
# runtime container, so a rebuilt binary is picked up by every new container
# without an image rebuild (same "mount, never bake" pattern as the adapters).
# Runtime config (auth, git identity) is mounted by the daemon at runtime,
# not baked in.
FROM debian:bookworm-slim

# Mark this image as the Orchicon runtime base. Docker inherits labels
# through `FROM`, so any image built on top of this one carries it, and
# the runtime daemon's container-create gate accepts derived images
# (AGENTS.md §Runtime Images).
LABEL org.orchicon.runtime-base=true

ENV DEBIAN_FRONTEND=noninteractive

# System toolchain — the "quick and efficient" baseline so workers don't
# reinstall common tools. Everything apt-provided is baked at build time
# (dpkg cannot run as the non-root runtime user).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git gnupg gh \
      build-essential pkg-config \
      python3 python3-pip python3-venv python3-dev \
      nodejs npm \
      ripgrep jq tar unzip xz-utils \
      zlib1g-dev libssl-dev libffi-dev libreadline-dev \
      libbz2-dev libsqlite3-dev liblzma-dev libncurses-dev libyaml-dev \
 && rm -rf /var/lib/apt/lists/*

# User-space package managers: mise (any-language version manager),
# uv (python), bun (JS runtime). Installed at build as root, then made
# available on PATH for the non-root runtime user.
RUN curl -fsSL https://mise.run | sh \
 && curl -LsSf https://astral.sh/uv/install.sh | sh \
 && curl -fsSL https://bun.sh/install | bash \
 && mkdir -p /usr/local/bin \
 && install /root/.local/bin/mise /usr/local/bin/mise \
 && install /root/.local/bin/uv /usr/local/bin/uv \
 && install /root/.bun/bin/bun /usr/local/bin/bun \
 && rm -rf /root/.cache /root/.bun/_bun* /root/.local/share/mise

# Runtime CLI adapters (opencode today; Claude Code / Codex later) are
# deliberately NOT baked into this image. The daemon bind-mounts the host's
# adapter installs in at runtime (see internal/runtime/daemon.go standard
# mounts: ~/.opencode). Orchicon never ships, downloads, or redistributes
# the adapter CLI — that keeps the product redistributable regardless of an
# adapter's license (Claude Code's terms prohibit bundling it).

# Runtime user: the same uid/gid as the host user running Orchicon, so
# bind-mounted project dirs are owned/written by that user. Everything
# below is chowned to this user so the worker has full control of the
# ephemeral filesystem without any root process. BuildKit bind-mounts
# /etc/resolv.conf + /etc/hosts read-only into the build container, so
# the recursive chown of /etc is best-effort (those two stay root-owned,
# which is fine — docker manages them).
RUN useradd -m -u 1000 -s /bin/bash orchicon \
 && (chown -R 1000:1000 /usr /etc /opt /var /home /root 2>/dev/null || true) \
 && rm -rf /home/orchicon/.profile /home/orchicon/.bashrc \
 && mkdir -p /home/orchicon && chown 1000:1000 /home/orchicon

# Host-home skeleton. Docker creates a missing bind-mount parent as ROOT, which the runtime uid could
# not then write, and `.local/share/opencode` / `.config/opencode` are overlaid by the daemon's mounts
# while the rest stay ephemeral — so these directories must exist, writable, for whatever home the
# container is given.
#
# NOTE: $HOME SUBDIRECTORIES ARE CREATED AT RUNTIME, NOT HERE.
#
# This image used to `mkdir -p /home/<the author's username>/...` and chown it. The container's HOME
# is set to the HOST user's home (`-e HOME=$HostHome`), because the daemon bind-mounts host paths at
# their own absolute paths — so those directories must exist for whatever home the CONTAINER is given.
# Baking one username in meant that on any other machine the directories were created at a path
# nobody uses, while the operator's ACTUAL home was left without them: the runtime uid could then
# fail to write `$HOME/.config`, `$HOME/.local/share` and the rest, which is the opposite of the
# intent. `orchicon runtime-supervisor` now creates them from $HOME at start (see ensureHomeDirs),
# which is correct for every user and needs nothing baked in.
#
# The image's own user (`orchicon`, uid 1000) keeps its home at /home/orchicon from useradd above;
# that is the standard account home and is unrelated to the runtime HOME the daemon sets.

# pip on Debian 12 enforces PEP 668 (externally managed env); the runtime
# user must be able to pip-install freely into the chowned rootfs.
ENV PIP_BREAK_SYSTEM_PACKAGES=1
ENV ORCHICON_CONTAINER_MODE=1

USER orchicon
WORKDIR /home/orchicon
# No ENTRYPOINT: the daemon controls what runs (runtime-supervisor).
