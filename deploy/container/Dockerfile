# Orchicon single-container image.
#
# Build:   make build        # produce bin/orchicon (frontend embedded)
#          cp bin/orchicon deploy/container/
#          docker build -f deploy/container/Dockerfile -t orchicon:local deploy/container
# Run:     docker run --rm -p 8080:8080 -p 3002:3000 \
#            -v orchicon-data:/var/lib/orchicon orchicon:local
#
# Multi-stage: copy runtime binaries from the official images onto a
# Postgres Debian base (glibc) so every process coexists in one network
# namespace. The orchicon binary is the PID-1 supervisor
# (`orchicon container`, cmd/orchicon/container.go): it writes the
# embedded Tempo/Loki/collector/Grafana configs into /var/lib/orchicon,
# spawns postgres → nats → telemetry → control plane with readiness
# gates, and restarts crashed children.
#
# NOTE: Tempo's OTLP receivers use ports 14317/14318 (offset from the
# collector's own 4317/4318) so the two can share one namespace.

FROM nats:2.10-alpine AS nats
FROM grafana/tempo:2.7.1 AS tempo
FROM grafana/loki:3.4.2 AS loki
FROM victoriametrics/victoria-metrics:v1.111.0 AS vm
FROM otel/opentelemetry-collector-contrib:0.119.0 AS collector
FROM grafana/grafana:11.5.2 AS grafana

FROM postgres:16 AS runtime

# util-linux provides setpriv (used by the supervisor to run postgres as
# the postgres user). The postgres user is set to uid 70 — the uid the
# compose-era alpine postgres volumes are owned by — so initdb and the
# postgres child both run as an existing user whose uid matches preserved
# volumes (initdb refuses to run without a passwd entry for the effective
# uid). curl installs the opencode runtime. git + gh are required for
# coding workers (the DevOps Engineer creates PRs and merges); without
# them the agent cannot run git/GitHub commands and may fabricate success.
RUN groupmod -g 70 postgres \
 && usermod -u 70 -g 70 postgres \
 && chown -R postgres:postgres /var/lib/postgresql \
 && chown postgres:postgres /usr/lib/postgresql \
 && apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates util-linux git gpg \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

COPY --from=nats /usr/local/bin/nats-server /usr/local/bin/nats-server
COPY --from=tempo /tempo /usr/local/bin/tempo
COPY --from=loki /usr/bin/loki /usr/local/bin/loki
COPY --from=vm /victoria-metrics-prod /usr/local/bin/victoria-metrics-prod
COPY --from=collector /otelcol-contrib /usr/local/bin/otelcol-contrib
COPY --from=grafana /usr/share/grafana /usr/share/grafana
COPY --from=grafana /etc/grafana /etc/grafana

# opencode runtime — the adapter bridge shells out to it for worker
# executions. Installed at build time; auth/config are mounted at runtime
# (~/.config/opencode, ~/.local/share/opencode) by the operator.
RUN curl -fsSL https://opencode.ai/install | bash \
 && cp /root/.opencode/bin/opencode /usr/local/bin/opencode

# The orchicon binary with the embedded frontend (see build steps above).
COPY orchicon /usr/local/bin/orchicon

ENV ORCHICON_CONTAINER_MODE=1
# Container-wide default so ANY process (supervisor, control plane, or a
# `docker exec orchicon db backup`) writes state/backups to the persistent
# volume, not the ephemeral root home. Overridable via docker run -e.
ENV ORCHICON_DATA_DIR=/var/lib/orchicon
VOLUME ["/var/lib/orchicon"]
EXPOSE 8080 3000
# Aggregate readiness: /healthz is served by the control plane, which only
# becomes ready after postgres + nats (and is the container's real signal).
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --retries=20 \
  CMD curl -fs http://localhost:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/orchicon", "container"]
