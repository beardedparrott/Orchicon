#!/usr/bin/env bash
# Functional check of the REWORKED step 4 probe (and step 8's absence needle)
# with a STUB engine — no container engine, no plane, no port. Proves the two
# rewritten assertions really discriminate: pristine => no FAIL, leaked state =>
# FAIL, and step 8's needle appears only when a container really was created.
set -uo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
STUB=/tmp/orchicon/stub
mkdir -p "$STUB"
cat >"$STUB/podman" <<'EOS'
#!/bin/sh
# stub engine: emulates `ps -a --format '{{.Names}}'` and `exec <c> sh -c ...`
STATE="${FAKE_PS_STATE:-/tmp/orchicon/stub/.ps-count}"
case "$1" in
  ps)
    n=0; [ -f "$STATE" ] && n="$(cat "$STATE")"
    n=$((n + 1)); printf '%s' "$n" >"$STATE"
    printf 'orchicon-pool-aaa\n'
    # FAKE_PS_MODE=grow: a NEW container appears from the second ps onwards
    if [ "${FAKE_PS_MODE:-same}" = "grow" ] && [ "$n" -ge 2 ]; then printf 'orchicon-rt-new123\n'; fi
    exit 0 ;;
  exec)
    # the state WRITE reports "written"; the state PROBE reports what the tree holds
    if [ "${FAKE_LEAK:-}" = "1" ]; then echo FILE-PRESENT; echo DIR-PRESENT
    elif [ "${FAKE_WRITE:-}" = "1" ]; then echo written; echo ABSENT; echo DIR-ABSENT
    else echo ABSENT; echo DIR-ABSENT; fi
    exit 0 ;;
esac
exit 0
EOS
chmod +x "$STUB/podman"

harness() {
  # shellcheck source=../scripts/runtime-e2e.sh
  . "$REPO/scripts/runtime-e2e.sh"
}

run_case4() { # <dir> <leak 0|1>
  local ev="$1" leak="$2"
  rm -rf "$ev"; mkdir -p "$ev"
  rm -f "$STUB/.ps-count"
  printf 'orchicon-pool-aaa\n' >"$ev/03-engine-ps-after.txt"
  (
    harness
    EVIDENCE="$ev"; MARKER="RUNTIME-E2E-TEST"; CONTAINER_NAME="orchicon-pool-aaa"
    PROJECT_ID="p"; WORKFLOW_ID="w"; CURRENT_STEP=4
    PASSES=0; FAILS=0; SKIPS=0
    : >"$ev/results.tsv"; : >"$ev/defects.tsv"
    PATH="$STUB:$PATH" ORCHICON_ENGINE=podman FAKE_PS_STATE="$STUB/.ps-count" \
      FAKE_PS_MODE=same FAKE_WRITE=1 FAKE_LEAK="$leak" step4_pristine_reset >/dev/null 2>&1
    printf 'passes=%s fails=%s skips=%s\n' "$PASSES" "$FAILS" "$SKIPS"
  )
  echo "  probe: $(tr '\n' '|' <"$ev/04-state-probe.txt")"
  echo "  probe names run A's own container: $(grep -c 'container=orchicon-pool-aaa' "$ev/04-state-probe.txt")"
  echo "  defects: $(cut -f2 "$ev/defects.tsv" 2>/dev/null | tr '\n' '|')"
}

run_case8() { # <dir> <ps-mode same|grow>
  local ev="$1"
  rm -rf "$ev"; mkdir -p "$ev"; rm -f "$STUB/.ps-count8"
  (
    harness
    EVIDENCE="$ev"; MARKER="RUNTIME-E2E-TEST"; PROJECT_ID="p"; WORKFLOW_ID="w"; INSTANCE="dev"
    CONTAINER_NAME="orchicon-pool-aaa"; CURRENT_STEP=8
    PASSES=0; FAILS=0; SKIPS=0
    : >"$ev/results.tsv"; : >"$ev/defects.tsv"
    PATH="$STUB:$PATH" ORCHICON_ENGINE=podman FAKE_PS_STATE="$STUB/.ps-count8" \
      FAKE_PS_MODE="$2" step8_all_native >/dev/null 2>&1
    printf 'passes=%s fails=%s skips=%s\n' "$PASSES" "$FAILS" "$SKIPS"
  )
  echo "  needle present in the capture: $(grep -c 'new container appeared:' "$ev/08-all-native.txt" || true)"
  echo "  defects: $(cut -f2 "$ev/defects.tsv" 2>/dev/null | tr '\n' '|')"
}

echo "=== step 4 case 1 — pristine reset (expect 0 fails) ==="
run_case4 /tmp/orchicon/s4-clean 0
echo "=== step 4 case 2 — state leaked into run B (expect >=1 fail) ==="
run_case4 /tmp/orchicon/s4-leak 1
echo "=== step 8 case 1 — no container created (expect 0 fails, no needle) ==="
run_case8 /tmp/orchicon/s8-clean same
echo "=== step 8 case 2 — a container WAS created (expect >=1 fail + the needle) ==="
run_case8 /tmp/orchicon/s8-new grow
