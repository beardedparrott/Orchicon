# scripts/lib/engine.sh — the ONE docker/podman resolver (sourced; no shebang).
#
# Three separate places drive the container engine out of this repo: `make
# clean-docker`, the instance teardown (the helper container that clears
# root-owned files, `volume rm`, `rm -f`), and the launch path's bridge lookup.
# Three independent `command -v docker` checks would drift, so they all ask this
# function instead.
#
# Podman is docker-CLI-compatible for every verb used here (`image prune --filter
# dangling=true`, `container prune --filter label=`, `run --rm -v`, `rm -f`,
# `volume inspect`/`volume rm`), so no per-engine branching is needed — only a
# per-engine NAME.
#
# ORCHICON_ENGINE pins the choice. That is what the harnesses and CI use: they
# stub the engine by name on PATH and must not be at the mercy of which engine
# happens to be installed.
engine_cmd() {
  if [ -n "${ORCHICON_ENGINE:-}" ]; then
    printf '%s\n' "$ORCHICON_ENGINE"
    return 0
  fi
  if command -v docker >/dev/null 2>&1; then
    printf 'docker\n'
    return 0
  fi
  if command -v podman >/dev/null 2>&1; then
    printf 'podman\n'
    return 0
  fi
  return 1
}

# engine_available is the yes/no form, for callers that only need to know
# whether there is an engine at all (and must not error when there is not).
engine_available() { engine_cmd >/dev/null 2>&1; }
