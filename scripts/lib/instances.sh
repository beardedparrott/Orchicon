# scripts/lib/instances.sh — THE shell instance table (sourced; no shebang).
#
# dev / test / prod are three INSTANCES of the same product on one host:
# separate container, separate ports, separate data volume, separate host state.
# This file is the ONE place the shell side enumerates them and resolves their
# per-instance launch shape.
#
# THE GO HALF IS cmd/orchicon/install_ports.go's instanceOrder, and the two
# halves cannot share a constant — so the agreement is ASSERTED:
# TestInstallPortDefaultsMatchShellInstanceTable reads THIS file and requires
# the set below to equal instanceOrder, in both directions. Adding an instance
# here without a Go column (or the reverse) fails that gate.
#
# Sourced, never executed: a sourced file must not change its caller's shell
# options, so there is no `set -e` and no shebang here.

# engine_cmd (docker | podman) comes from the one resolver, so this file and the
# teardown can never disagree about which engine they are driving.
command -v engine_cmd >/dev/null 2>&1 || . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/engine.sh"

# ORCHICON_PG_VOLUME overrides the Postgres data volume. The default reuses the
# existing compose-stack volumes (orchicon_postgres-data /
# orchicon-prod_postgres-data) so dev/prod data is preserved when switching to
# the single-container workflow. Set to "fresh" to start with an empty database
# in the instance volume instead.

instance_info() {
  local inst="$1"
  case "$inst" in
    dev)
      VOLUME="orchicon-cnt-dev-data"
      PG_VOLUME="${ORCHICON_PG_VOLUME:-orchicon_postgres-data}"
      COMPOSE_PG="orchicon-postgres"
      COMPOSE_STACK_SCRIPT="dev.sh"
      PLANE_HTTP_PORT=8080
      # NO PLANE_PUBLIC_URL literal here: the host plane's URL is DERIVED per
      # instance from the resolved docker bridge + PLANE_HTTP_PORT
      # (bridge_bind_env — the one place either the bind or the URL is
      # computed). A literal is how one instance's workers end up dialing the
      # other's plane, and a hardcoded 172.17.0.1 breaks whenever the bridge
      # subnet changes (retired by the host-gateway mapping in #3).
      PG_PORT=5432
      NATS_PORT=4222
      NATS_MON_PORT=8222
      OTLP_GRPC_PORT=4317
      OTLP_HTTP_PORT=4318
      TEMPO_PORT=3200
      LOKI_PORT=3100
      VM_PORT=8428
      HOST_DATA_DIR="$HOME/.local/share/orchicon-dev"
      ;;
    prod)
      VOLUME="orchicon-cnt-prod-data"
      PG_VOLUME="${ORCHICON_PG_VOLUME:-orchicon-prod_postgres-data}"
      COMPOSE_PG="orchicon-prod-postgres"
      COMPOSE_STACK_SCRIPT="dev-prod.sh"
      PLANE_HTTP_PORT=8091
      # See dev: the URL is derived per instance (bridge_bind_env), never a
      # literal — and prod's port (8091) is what keeps it apart from dev's.
      PG_PORT=5433
      NATS_PORT=4223
      NATS_MON_PORT=8223
      OTLP_GRPC_PORT=4319
      OTLP_HTTP_PORT=4320
      TEMPO_PORT=3201
      LOKI_PORT=3101
      VM_PORT=8429
      HOST_DATA_DIR="$HOME/.local/share/orchicon-prod"
      ;;
    test)
      VOLUME="orchicon-cnt-test-data"
      # NOT dev/prod's legacy compose volume. They reuse orchicon_postgres-data /
      # orchicon-prod_postgres-data, which may hold real tenant data; "fresh"
      # keeps this instance's database inside its OWN data volume, so a test
      # instance can never read or write a real deployment's rows.
      PG_VOLUME="${ORCHICON_PG_VOLUME:-fresh}"
      COMPOSE_PG="orchicon-test-postgres"
      COMPOSE_STACK_SCRIPT="dev-test.sh"
      PLANE_HTTP_PORT=8092
      PG_PORT=5434
      NATS_PORT=4224
      NATS_MON_PORT=8224
      OTLP_GRPC_PORT=4321
      OTLP_HTTP_PORT=4322
      TEMPO_PORT=3202
      LOKI_PORT=3102
      VM_PORT=8430
      HOST_DATA_DIR="$HOME/.local/share/orchicon-test"
      ;;
    *)
      echo "Unknown instance: $inst (use dev|prod|test)" >&2
      return 1
      ;;
  esac

  # OPERATOR PORT OVERRIDES — deliberately THE SAME VARIABLES `orchicon install`
  # resolves (cmd/orchicon/install_ports.go). A user must be able to move an
  # instance off a port another product already holds, and the two launchers must
  # not disagree about which port it answers on: `orchicon install` and
  # `scripts/container.sh up` bind the SAME host port for one instance, so an
  # override has to be honoured in BOTH or in neither. The knob names are what
  # tie them together, so they cannot be renamed on one side alone.
  #
  # Applied at this ONE choke point, before anything is composed from these
  # numbers. Every consumer reads these variables — the container publishes
  # (PORTS / SERVICE_PORTS below), the host plane's environment (plane_env), its
  # bridge bind and advertised URL (bridge_bind_env), and every /healthz probe —
  # so no consumer can retain a stale literal.
  PLANE_HTTP_PORT="${ORCHICON_CONTROL_PORT:-$PLANE_HTTP_PORT}"
  PG_PORT="${ORCHICON_POSTGRES_PORT:-$PG_PORT}"
  NATS_PORT="${ORCHICON_NATS_PORT:-$NATS_PORT}"
  NATS_MON_PORT="${ORCHICON_NATS_MONITOR_PORT:-$NATS_MON_PORT}"
  OTLP_GRPC_PORT="${ORCHICON_OTLP_GRPC_PORT:-$OTLP_GRPC_PORT}"
  OTLP_HTTP_PORT="${ORCHICON_OTLP_HTTP_PORT:-$OTLP_HTTP_PORT}"
  TEMPO_PORT="${ORCHICON_TEMPO_PORT:-$TEMPO_PORT}"
  LOKI_PORT="${ORCHICON_LOKI_PORT:-$LOKI_PORT}"
  VM_PORT="${ORCHICON_VM_PORT:-$VM_PORT}"

  # COMPOSED from the (possibly overridden) ports above, not written as literals
  # beside them: a literal is how an override silently leaves the publish on the
  # old number. 3002/3003/3004 are RETIRED (they were Grafana's published port)
  # and are RESERVED — no row claims them.
  PORTS="-p $PLANE_HTTP_PORT:8080"

  # Host-residency publishes (services-only mode). EVERY port is bound to
  # 127.0.0.1 EXPLICITLY: postgres and nats are a database and an internal
  # event bus, and must never be reachable from the LAN. This loopback publish
  # is the security boundary for the pg_hba trust rules the supervisor adds.
  SERVICE_PORTS="-p 127.0.0.1:$PG_PORT:5432"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$NATS_PORT:4222 -p 127.0.0.1:$NATS_MON_PORT:8222"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$OTLP_GRPC_PORT:4317 -p 127.0.0.1:$OTLP_HTTP_PORT:4318"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$TEMPO_PORT:3200 -p 127.0.0.1:$LOKI_PORT:3100"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$VM_PORT:8428"
}

# residency_for prints the plane residency for THIS invocation: host or
# container.
#
# DEFAULT IS HOST. The plane runs on the HOST as its own `orchicon serve`
# process, with the services (Postgres/NATS/telemetry) in the one
# container and the plane reaching them over loopback. That is the supported
# shape — a user is a host user — so `container.sh up dev` gives a host plane
# with no extra ceremony. `container` is kept as the self-contained shape (the
# whole stack, plane included, inside one container) for anyone who wants it.
#
# Precedence: an explicit argument, then ORCHICON_PLANE_RESIDENCY, then host.
# Pass the argument as the third positional to switch back:
#   scripts/container.sh up dev container
residency_for() {
  local value="${1:-${ORCHICON_PLANE_RESIDENCY:-host}}"
  case "$value" in
    host|container)
      echo "$value"
      ;;
    *)
      log_err "ORCHICON_PLANE_RESIDENCY must be 'host' or 'container' (got '$value')"
      return 1
      ;;
  esac
}

# plane_env prints the HOST plane's environment profile for an instance, one
# KEY=VALUE per line. EVERY value is sourced from instance_info, so no value
# can be global: a shared HTTP port or plane URL would silently point one
# instance's workers at the other. Values the operator already set in the
# environment (identity/keys) are inherited rather than invented.
plane_env() {
  local inst="${1:-dev}"
  instance_info "$inst"
  echo "ORCHICON_INSTANCE=$inst"
  # THE PRIMARY BIND IS A WILDCARD, AND THAT IS THE POINT.
  #
  # A wildcard listener answers on EVERY IPv4 interface at once, so ONE listener
  # serves all three client populations: host clients on loopback (orch, the TUI,
  # the desktop app), runtime containers dialling the host gateway, and any other
  # machine on the LAN. It is a strict SUPERSET of the loopback-plus-bridge pair
  # it replaces, so nothing needs a second bind.
  #
  # The pair existed to work around a COLLISION, not a capability gap: `:<port>`
  # already owns <bridge ip>:<port> on every interface, so a second listener
  # there could never bind and retried a doomed net.Listen every 30s forever
  # ("bridge listener bind failed — runtime-container workers cannot reach the
  # plane"). The fix was to DELETE the redundant listener, not to narrow the
  # primary — narrowing it silently took the plane off the LAN.
  echo "ORCHICON_HTTP_ADDR=:$PLANE_HTTP_PORT"
  # THE ADVERTISED URL ONLY — there is no extra bind any more.
  #
  # A runtime container still has to be told where the plane is, and that value
  # is a REAL requirement: bridge_bind_env computes it per instance so two
  # instances can never be pointed at each other's plane. Only the listener half
  # is redundant, because the wildcard primary above already serves that address.
  #
  # An unresolvable bridge is now a WARNING, never fatal: the wildcard bind does
  # not depend on it, and every runtime container is created with
  # --add-host host.docker.internal:host-gateway (engine.go, ADR-0013 D2.4), so
  # the stable NAME reaches the plane on both engines even when the numeric
  # bridge address cannot be resolved. That is what keeps a rootless podman host
  # (no docker0, no 172.17.0.1) working instead of refusing to start.
  local bind_env=""
  if bind_env=$(bridge_bind_env "$inst"); then
    printf '%s\n' "$bind_env"
  else
    # Unreachable while bridge_bind_env owns the fallback above; kept so a future
    # change that reintroduces a failure mode still cannot leave the plane without
    # an advertised URL (every plane-channel MCP call in every dispatched run
    # would time out). The value is the same stable name, per instance.
    log_warn "$inst: no plane URL could be computed — advertising the stable host name"
    log_warn "  (host.docker.internal, which every runtime container resolves via --add-host)."
    echo "ORCHICON_PLANE_PUBLIC_URL=http://host.docker.internal:$PLANE_HTTP_PORT"
  fi
  echo "ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:$PG_PORT/orchicon?sslmode=disable"
  echo "ORCHICON_NATS_URL=nats://localhost:$NATS_PORT"
  echo "ORCHICON_OTEL_ENDPOINT=localhost:$OTLP_GRPC_PORT"
  echo "ORCHICON_TEMPO_URL=http://localhost:$TEMPO_PORT"
  echo "ORCHICON_LOKI_URL=http://localhost:$LOKI_PORT"
  echo "ORCHICON_VM_URL=http://localhost:$VM_PORT"
  # State dir: the KEK and ask-history live here. The host VALUE moves; the
  # expectation (`<DataDir>/secrets/kek`) does not — migrate_host_data_dir
  # copies the container volume so existing tenant secrets still decrypt.
  echo "ORCHICON_DATA_DIR=$HOST_DATA_DIR"
  echo "ORCHICON_BLOB_DIR=$HOST_DATA_DIR/blobs"
  echo "ORCHICON_RUNTIME_SOCKET=${RUNTIME_SOCKET:-${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}/runtime.sock}"
  # Per-instance PID/log file for `serve --detach`/`--stop`: two host planes
  # must never share one.
  echo "ORCHICON_SERVE_STATE_DIR=$HOST_DATA_DIR/serve"
  local inherit
  for inherit in ORCHICON_DEPLOYMENT_TENANT_ID ORCHICON_AUTH_SIGNING_KEY ORCHICON_SECRETS_KEK; do
    if [ -n "${!inherit:-}" ]; then
      echo "$inherit=${!inherit}"
    fi
  done
}

# bridge_ip prints the host's container-bridge address — the address a runtime
# container on the default bridge uses to reach the HOST. A HOST-RESIDENT
# plane binds it so runtime containers can dial the plane directly on the
# bridge; a published host port cannot be used instead (docker hairpin NAT
# drops gateway→published-port traffic from bridge containers, which is
# exactly the plane-channel MCP timeout this bind removes).
#
# Resolution order: ORCHICON_DOCKER_BRIDGE_IP (tests / hand-pinning) →
# docker's own IPAM config → the docker0 interface address.
#
# An unresolvable bridge is a HARD error (non-zero, no output). There is
# deliberately no 0.0.0.0 fallback — that is the LAN exposure the operator
# rejected — and no guessed address, because a wrong bind shows up much
# later as "workers cannot reach the plane".
bridge_ip() {
  if [ -n "${ORCHICON_DOCKER_BRIDGE_IP:-}" ]; then
    printf '%s\n' "$ORCHICON_DOCKER_BRIDGE_IP"
    return 0
  fi
  local ip=""
  if engine_cmd >/dev/null 2>&1; then
    ip=$("$(engine_cmd)" network inspect bridge \
      --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)
    # The Go template prints "<no value>" for a nil gateway.
    case "$ip" in "<no value>"|"null") ip="" ;; esac
  fi
  if [ -z "$ip" ]; then
    ip=$(ip -4 -o addr show docker0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 || true)
  fi
  if [ -z "$ip" ]; then
    log_err "cannot resolve the docker bridge address ($(engine_cmd 2>/dev/null || echo docker) network inspect bridge / docker0)"
    log_err "pin it explicitly with ORCHICON_DOCKER_BRIDGE_IP=<bridge gateway ip>; refusing to guess or bind 0.0.0.0"
    return 1
  fi
  printf '%s\n' "$ip"
}

# bridge_bind_env <inst> prints the HOST-RESIDENT plane env for THIS
# instance, one KEY=VALUE per line and nothing else:
#
#   ORCHICON_HTTP_EXTRA_BIND=<bridge ip>:<this instance's plane port>
#   ORCHICON_PLANE_PUBLIC_URL=http://<bridge ip>:<this instance's plane port>
#
# This is the ONE place either value is computed, so the bind the plane
# listens on and the URL it advertises to run containers can never disagree.
# The port is ALWAYS this instance's own PLANE_HTTP_PORT (dev 8080 / prod
# 8091): dev and prod are separate instances that migrate independently, and
# a shared 8080 would make prod's workers dial dev's plane (or the reverse) —
# a rejected dial, since the minted credential is instance-bound, but a
# broken run either way.
#
# These lines belong in THAT instance's plane environment only. Never export
# ORCHICON_PLANE_PUBLIC_URL from a shared shell profile: a globally-set value
# points one instance's workers at the other's plane.
bridge_bind_env() {
  local inst="${1:-dev}"
  instance_info "$inst" || return 1
  # NO ORCHICON_HTTP_EXTRA_BIND LINE. The wildcard primary bind already serves
  # this address; emitting it again would ask the plane for a second listener on
  # an address the first one owns, which cannot bind and is the exact collision
  # this change removes. Only the advertised URL is still needed.
  #
  # AN UNRESOLVABLE BRIDGE IS NOT AN ERROR, AND THIS IS THE ONE PLACE THAT
  # DECIDES IT. It used to `return 1`, which made plane_start refuse to start on
  # a host whose numeric bridge address could not be resolved — and a ROOTLESS
  # PODMAN host has no docker0 and no 172.17.0.1 at all (ADR-0014), so a
  # supported engine could not run the plane over a value it does not need.
  #
  # The fallback is the stable host NAME, not a guess: every runtime container is
  # created with --add-host host.docker.internal:host-gateway (engine.go
  # CreateBaseArgs, ADR-0013 D2.4), so the name resolves to the host on BOTH
  # engines and every platform, and only the PORT distinguishes two instances.
  # planePublicURL already documents exactly this as its last-resort branch.
  local ip
  if ! ip=$(bridge_ip); then
    printf 'ORCHICON_PLANE_PUBLIC_URL=http://host.docker.internal:%s\n' "$PLANE_HTTP_PORT"
    return 0
  fi
  printf 'ORCHICON_PLANE_PUBLIC_URL=http://%s:%s\n' "$ip" "$PLANE_HTTP_PORT"
}

# The instance set, in one place. Kept as a scalar (not a case statement) so the
# Go gate can compare it to instanceOrder with a plain field split.
ORCHICON_INSTANCES="dev prod test"

# instance_exists reports whether a name is one of the instances above. Used to
# REFUSE an unknown instance on a destructive verb: a teardown must never guess.
instance_exists() {
  case " $ORCHICON_INSTANCES " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# Logging + colour helpers. Defined ONLY when the caller has none: container.sh
# defines its own, scripts/orchicon.sh defines none, and both source this file —
# so a guarded definition gives the libs one vocabulary without clobbering the
# caller's.
if ! command -v log_err >/dev/null 2>&1; then
  C_RESET='\033[0m'; C_BOLD='\033[1m'; C_GREEN='\033[32m'; C_DIM='\033[2m'; C_YELLOW='\033[33m'; C_RED='\033[31m'
  log_ok()   { echo -e "${C_GREEN}✓${C_RESET} $*"; }
  log_dim()  { echo -e "${C_DIM}$*${C_RESET}"; }
  log_warn() { echo -e "${C_YELLOW}!${C_RESET} $*"; }
  log_err()  { echo -e "${C_RED}✗${C_RESET} $*" >&2; }
fi
