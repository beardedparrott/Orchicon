#!/usr/bin/env bash
# ============================================================================
# A PATH that has NO container engine and NO WSL on it — installed as real
# executable shims, so "the install path never asks for Docker" is PROVEN rather
# than assumed.
#
# WHY SHIMS AND NOT A GREP. An installer that quietly runs `docker info`, or
# `wsl.exe -d Ubuntu`, still prints a clean-looking transcript when neither is
# installed: it takes a shell `command -v` branch and carries on, or fails later
# with a message about something else. The only way to prove the engine was never
# consulted is to make the NAME resolvable and BOOBY-TRAPPED: the shim exits
# non-zero, writes the full argv to a log, and that log must stay EMPTY for the
# whole run.
#
# This mirrors a clean machine of each OS, where `docker`, `podman`, `wsl` and
# `docker.exe` genuinely do not exist:
#   docker docker-compose docker.exe compose        (Docker / Docker Desktop)
#   podman podman-remote podman.exe nerdctl         (Podman Desktop / alternatives)
#   wsl wsl.exe                                     (WSL — Windows local mode must not need it)
#   colima limactl                                  (macOS engine VMs)
#
# USAGE
#   scripts/e2e/no-engine-path.sh <shim-dir>     create the shims in <shim-dir>
#   scripts/e2e/no-engine-path.sh --check <shim-dir>
#                                                create them, then assert every
#                                                shim FAILS (exit 127) and logs
#
# SOURCED (by scripts/published-archive-e2e.sh) it defines the functions and
# creates nothing.
# ============================================================================
set -uo pipefail

# The names that must not exist on a clean machine, and must never be invoked.
engine_shim_names() {
	printf '%s\n' docker docker-compose docker.exe compose podman podman-remote podman.exe \
		nerdctl wsl wsl.exe colima limactl
}

shim_log_path() { printf '%s/invocations.log' "$1"; }

make_shims() { # make_shims <shim-dir>
	local dir="$1" name log
	[ -n "$dir" ] || { printf 'make_shims: no shim dir given\n' >&2; return 2; }
	mkdir -p "$dir" || return 1
	log="$(shim_log_path "$dir")"
	: >"$log"
	for name in $(engine_shim_names); do
		# The shim's own path is spelled out at creation time: a shim that has to work out which
		# name it was called by would need $0 parsing, and on Windows/MSYS $0 is unreliable.
		cat >"${dir}/${name}" <<SHIM
#!/usr/bin/env bash
# planted by scripts/e2e/no-engine-path.sh — a clean machine has no $name
printf '%s\t%s\t%s\n' "\$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$name" "\$*" >>"${log}" 2>/dev/null || true
printf 'no-engine-path: ${name} was INVOKED (%s) on a machine with no container engine and no WSL\n' "\$*" >&2
exit 127
SHIM
		chmod +x "${dir}/${name}" || return 1
	done
	return 0
}

shim_invocations() { # shim_invocations <shim-dir> — the log, empty when nothing asked
	local log; log="$(shim_log_path "$1")"
	[ -f "$log" ] && cat "$log"
	return 0
}

# engine_runs_here <shim-dir> — 0 when a REAL engine answered, i.e. this host is not clean.
# The probe goes through the shims: a shim that exits 127 proves nothing is installed, while a
# host whose engine answers would have to bypass the shim dir, which the harness never does.
engine_runs_here() {
	local dir="$1"
	PATH="${dir}:$PATH" command -v docker >/dev/null 2>&1 || return 0     # nothing named docker at all: absent
	PATH="${dir}:$PATH" docker info >/dev/null 2>&1 && return 0           # a real engine answered: NOT clean
	return 1
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	dir=""
	check=0
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--check) check=1; shift ;;
			-h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
			*) dir="$1"; shift ;;
		esac
	done
	[ -n "$dir" ] || { printf 'usage: no-engine-path.sh [--check] <shim-dir>\n' >&2; exit 2; }
	make_shims "$dir" || exit 1
	printf 'planted %s engine/WSL shims in %s\n' "$(engine_shim_names | wc -l | tr -d ' ')" "$dir"
	if [ "$check" = "1" ]; then
		rc=0
		for name in $(engine_shim_names); do
			if PATH="${dir}:$PATH" "$name" >/dev/null 2>&1; then
				printf '  FAIL  %s did NOT fail — the shim is not booby-trapped\n' "$name"
				rc=1
			else
				printf '  PASS  %s exits non-zero\n' "$name"
			fi
		done
		printf 'invocations logged: %s\n' "$(shim_invocations "$dir" | wc -l | tr -d ' ')"
		exit "$rc"
	fi
	exit 0
fi
