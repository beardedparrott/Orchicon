#!/usr/bin/env bash
# QA's OWN mutation probe (independent of the repo's gate): does the real
# harness gate the vacuity class, or only the repo's test of it?
set -uo pipefail
WT=/home/beardedparrott/projects/Orchicon/.orchicon-worktrees/01M544QJBKJ7BWX5HCMVRJHD4C
. "$WT/scripts/no-engine-e2e.sh" >/dev/null 2>&1
EVIDENCE=/tmp/orchicon/probe/ev; mkdir -p "$EVIDENCE"; : >"$EVIDENCE/results.tsv"
CURRENT_STEP=4
: > "$EVIDENCE/empty.txt"
printf 'no marker here\n' > "$EVIDENCE/nomarker.txt"
printf 'has XMUTPROBE inside\n' > "$EVIDENCE/withmarker.txt"
echo "--- M1 empty capture, absence assertion (old behaviour: PASS = vacuous)"
gated_absent "M1 served bundle lacks marker" "$EVIDENCE/empty.txt" "XMUTPROBE" "why-empty"
echo "--- M2 empty capture, presence assertion"
gated_contains "M2 served bundle carries marker" "$EVIDENCE/empty.txt" "XMUTPROBE" "why-empty"
echo "--- M3 non-empty, marker genuinely absent (must PASS)"
gated_absent "M3 stale control" "$EVIDENCE/nomarker.txt" "XMUTPROBE" "why-empty"
echo "--- M4 non-empty, marker present, absence assertion (must FAIL)"
gated_absent "M4 stale control caught" "$EVIDENCE/withmarker.txt" "XMUTPROBE" "why-empty" "owner:file:1"
echo "--- M5 non-empty, marker present, presence assertion (must PASS)"
gated_contains "M5 change is live" "$EVIDENCE/withmarker.txt" "XMUTPROBE" "why-empty" 
echo "=== recorded verbatim"
cat "$EVIDENCE/results.tsv"
echo "=== verdict"
awk -F'\t' '$3 ~ /^M1/ && $2=="SKIP"{a=1} $3 ~ /^M2/ && $2=="SKIP"{b=1} $3 ~ /^M3/ && $2=="PASS"{c=1} $3 ~ /^M4/ && $2=="FAIL"{d=1} $3 ~ /^M5/ && $2=="PASS"{e=1} END{ if(a&&b&&c&&d&&e) print "PROBE OK: empty captures SKIP, real content is judged"; else print "PROBE BROKEN" }' "$EVIDENCE/results.tsv"
