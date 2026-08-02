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
# The image ships the orchicon binary (for `runtime-supervisor` /
# `runtime-client`) and the opencode runtime (the adapter drives it with
# `opencode run --format json`). Runtime config (auth, git identity) is
# mounted by the daemon at runtime, not baked in.
FROM debian:bookworm-slim

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

# opencode runtime — the adapter bridge drives it as a subprocess inside
# the runtime container (`opencode run --format json`).
RUN curl -fsSL https://opencode.ai/install | bash \
 && install /root/.opencode/bin/opencode /usr/local/bin/opencode

# The orchicon binary provides `runtime-supervisor` (PID 1) and
# `runtime-client`. Copied by the build step alongside bin/orchicon.
COPY orchicon /usr/local/bin/orchicon

# Runtime user: the same uid/gid as the host user running Orchicon, so
# bind-mounted project dirs are owned/written by that user. Everything
# below is chowned to this user so the worker has full control of the
# ephemeral filesystem without any root process.
RUN useradd -m -u 1000 -s /bin/bash orchicon \
 && chown -R 1000:1000 /usr /etc /opt /var /home /root \
 && rm -rf /home/orchicon/.profile /home/orchicon/.bashrc \
 && mkdir -p /home/orchicon && chown 1000:1000 /home/orchicon

# Host-home skeleton (same paths the supervisor container uses, so the
# daemon's bind mounts overlay existing dirs). Docker creates missing
# bind-mount parents as ROOT — pre-creating and chowning these to the
# runtime uid means workers can still write into $HOME even though no
# root process exists. `.local/share/opencode` and `.config/opencode`
# are overlaid by the daemon's mounts; the rest stay ephemeral.
RUN mkdir -p \
      /home/beardedparrott/.local/share \
      /home/beardedparrott/.local/state \
      /home/beardedparrott/.config \
      /home/beardedparrott/.cache \
      /home/beardedparrott/.npm \
      /home/beardedparrott/projects \
 && chown -R 1000:1000 /home/beardedparrott

# pip on Debian 12 enforces PEP 668 (externally managed env); the runtime
# user must be able to pip-install freely into the chowned rootfs.
ENV PIP_BREAK_SYSTEM_PACKAGES=1
ENV ORCHICON_CONTAINER_MODE=1

USER orchicon
WORKDIR /home/orchicon
# No ENTRYPOINT: the daemon controls what runs (runtime-supervisor).
