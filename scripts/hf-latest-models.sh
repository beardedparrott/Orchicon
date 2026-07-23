#!/usr/bin/env bash
# hf-latest-models.sh — Fetch the latest AI models released on Hugging Face
# today and this week, and output a clean summary to /tmp.
#
# Usage:
#   scripts/hf-latest-models.sh              # default: today's models
#   scripts/hf-latest-models.sh --week        # this week's models
#   scripts/hf-latest-models.sh --today       # today's models (default)
#   scripts/hf-latest-models.sh --force-cache # reuse a cached response (faster)
#
# Output: /tmp/hf-latest-models-{today|week}.txt

set -euo pipefail

SCRIPT_NAME="$(basename "${BASH_SOURCE[0]}")"

# ── Config ────────────────────────────────────────────────────────────────────
HF_API="https://huggingface.co/api/models"
PER_PAGE=100
MAX_PAGES_TODAY=5
MAX_PAGES_WEEK=30
CACHE_DIR="/tmp/hf-cache"
mkdir -p "$CACHE_DIR"

# ── Help ───────────────────────────────────────────────────────────────────────
usage() {
  cat <<EOF
Usage: $SCRIPT_NAME [--today | --week] [--force-cache]

Options:
  --today        Fetch models released today (default)
  --week         Fetch models released in the last 7 days
  --force-cache  Use cached API response if available (skips HTTP fetch)
  --help         Show this help

Output: /tmp/hf-latest-models-{today|week}.txt
EOF
  exit 0
}

# ── Parse arguments ────────────────────────────────────────────────────────────
MODE="today"
FORCE_CACHE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --today)       MODE="today" ;;
    --week)        MODE="week" ;;
    --force-cache) FORCE_CACHE=true ;;
    --help|-h)     usage ;;
    *)             echo "Unknown option: $1"; usage ;;
  esac
  shift
done

# ── Date calculations ─────────────────────────────────────────────────────────
NOW_UTC=$(date -u +%Y-%m-%dT%H:%M:%S)
TODAY=$(date -u +%Y-%m-%d)
WEEK_AGO=$(date -u -d "7 days ago" +%Y-%m-%d)

if [ "$MODE" = "week" ]; then
  LABEL="week ($WEEK_AGO — $TODAY)"
  OUTPUT_FILE="/tmp/hf-latest-models-week.txt"
  CACHE_FILE="$CACHE_DIR/week.json"
  MAX_PAGES="$MAX_PAGES_WEEK"
else
  LABEL="today ($TODAY)"
  OUTPUT_FILE="/tmp/hf-latest-models-today.txt"
  CACHE_FILE="$CACHE_DIR/today.json"
  MAX_PAGES="$MAX_PAGES_TODAY"
fi

# ── Fetch from Hugging Face API (paginated) ──────────────────────────────────
fetch_and_cache() {
  if [ "$FORCE_CACHE" = true ] && [ -f "$CACHE_FILE" ]; then
    echo "Using cached response from $CACHE_FILE"
    return
  fi

  echo "Fetching models released $LABEL from Hugging Face API..."

  pages=()
  for (( page=0; page<MAX_PAGES; page++ )); do
    offset=$(( page * PER_PAGE ))
    url="${HF_API}?sort=createdAt&direction=-1&limit=${PER_PAGE}&offset=${offset}"
    printf "  Page %d (offset=%d) ... " "$(( page + 1 ))" "$offset"
    response=$(curl -sS --max-time 20 \
      -H "User-Agent: orchicon-script/1.0" \
      "$url")

    count=$(echo "$response" | jq 'length' 2>/dev/null || echo "0")
    echo "got $count models"

    if [ "$count" -eq 0 ]; then
      echo "  No more models — stopping."
      break
    fi

    pages+=("$response")
  done

  if [ ${#pages[@]} -eq 0 ]; then
    echo "[]" > "$CACHE_FILE"
  elif [ ${#pages[@]} -eq 1 ]; then
    echo "${pages[0]}" > "$CACHE_FILE"
  else
    printf '%s\n' "${pages[@]}" | jq -s 'add' > "$CACHE_FILE"
  fi

  total=$(jq 'length' "$CACHE_FILE")
  echo "Cached $total models to $CACHE_FILE"
}

fetch_and_cache

# ── Check for valid JSON ──────────────────────────────────────────────────────
if ! jq empty "$CACHE_FILE" 2>/dev/null; then
  echo "ERROR: Invalid JSON in API response. Cache file: $CACHE_FILE"
  echo "Raw response (first 500 chars):"
  head -c 500 "$CACHE_FILE"
  exit 1
fi

# ── Filter cached models by date (local filtering, since the HF API
#     does not support server-side date filtering) ────────────────────────────
if [ "$MODE" = "week" ]; then
  FILTERED_FILE=$(mktemp)
  jq --arg from "$WEEK_AGO" '[.[] | select(.createdAt >= ($from + "T00:00:00.000Z"))]' "$CACHE_FILE" > "$FILTERED_FILE"
else
  FILTERED_FILE=$(mktemp)
  jq --arg today "$TODAY" '[.[] | select(.createdAt // "" | startswith($today))]' "$CACHE_FILE" > "$FILTERED_FILE"
fi

# ── Parse and format output ───────────────────────────────────────────────────
{
  total=$(jq 'length' "$FILTERED_FILE")
  downloaded=$(jq '[.[] | select(.downloads > 0)] | length' "$FILTERED_FILE")
  total_downloads=$(jq '[.[] | .downloads // 0] | add' "$FILTERED_FILE")
  avg_downloads=$(( total > 0 ? total_downloads / total : 0 ))

  echo "╔══════════════════════════════════════════════════════════════════════════╗"
  echo "║            Latest Hugging Face Models — $LABEL"
  echo "╚══════════════════════════════════════════════════════════════════════════╝"
  echo ""
  echo "  Generated:    $NOW_UTC UTC"
  echo "  Models found: $total"
  echo "  With downloads: $downloaded"
  echo "  Total downloads: $total_downloads"
  echo "  Avg downloads/model: $avg_downloads"
  echo ""

  echo "  ── Capability (pipeline_tag) distribution ──"
  jq -r 'group_by(.pipeline_tag // "unlabeled") | map({tag: .[0].pipeline_tag // "unlabeled", count: length}) | sort_by(-.count) | .[] | "    \(.tag): \(.count)"' "$FILTERED_FILE"
  echo ""

  jq -c '.[]' "$FILTERED_FILE" 2>/dev/null | while IFS= read -r model; do
    id=$(echo "$model" | jq -r '.id // "unknown"')
    pipeline_tag=$(echo "$model" | jq -r '.pipeline_tag // "unlabeled"')
    downloads=$(echo "$model" | jq -r '.downloads // 0')
    likes=$(echo "$model" | jq -r '.likes // 0')
    created_at=$(echo "$model" | jq -r '.createdAt // "unknown"')
    created_readable=$(echo "$created_at" | sed 's/T/ /' | sed 's/\.[0-9]*Z//' | sed 's/Z//')

    raw_libs=$(echo "$model" | jq -r '.library_name? // ""')
    if [ -z "$raw_libs" ] || [ "$raw_libs" = "null" ]; then
      libs="—"
    else
      libs="$raw_libs"
    fi

    printf "  Model:      %s\n" "$id"
    printf "  Capability: %s\n" "$pipeline_tag"
    printf "  Libraries:  %s\n" "$libs"
    printf "  Released:   %s\n" "$created_readable"
    printf "  Stats:      ⭐ %s  ⬇️ %s\n" "$likes" "$downloads"
    printf "  %s\n" "─────────────────────────────────────────────────────────"
    echo ""
  done

  echo ""
} > "$OUTPUT_FILE"

rm -f "$FILTERED_FILE"

echo ""
echo "Done. Output written to: $OUTPUT_FILE"
echo ""
head -8 "$OUTPUT_FILE"
