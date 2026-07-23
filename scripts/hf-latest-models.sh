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
}

# ── Parse arguments ────────────────────────────────────────────────────────────
MODE="today"
FORCE_CACHE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --today)       MODE="today" ;;
    --week)        MODE="week" ;;
    --force-cache) FORCE_CACHE=true ;;
    --help|-h)     usage; exit 0 ;;
    *)             echo "Error: Unknown option: $1" >&2; usage; exit 1 ;;
  esac
  shift
done

# ── Check required tools ──────────────────────────────────────────────────────
check_deps() {
  local missing=()
  command -v curl >/dev/null 2>&1 || missing+=("curl")
  command -v jq   >/dev/null 2>&1 || missing+=("jq")

  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "Error: Missing required tools: ${missing[*]}" >&2
    echo "Install them and try again." >&2
    exit 1
  fi
}
check_deps

# ── Date calculations ─────────────────────────────────────────────────────────
NOW_UTC=$(date -u +%Y-%m-%dT%H:%M:%S)
TODAY=$(date -u +%Y-%m-%d)
if date -d "7 days ago" >/dev/null 2>&1; then
  WEEK_AGO=$(date -u -d "7 days ago" +%Y-%m-%d)
else
  # Fallback for systems without GNU date (e.g., macOS)
  WEEK_AGO=$(date -u -v-7d +%Y-%m-%d 2>/dev/null || echo "unknown")
fi

if [ "$MODE" = "week" ]; then
  LABEL="week (${WEEK_AGO} — ${TODAY})"
  OUTPUT_FILE="/tmp/hf-latest-models-week.txt"
  CACHE_FILE="$CACHE_DIR/week.json"
  MAX_PAGES="$MAX_PAGES_WEEK"
else
  LABEL="today (${TODAY})"
  OUTPUT_FILE="/tmp/hf-latest-models-today.txt"
  CACHE_FILE="$CACHE_DIR/today.json"
  MAX_PAGES="$MAX_PAGES_TODAY"
fi

# ── Fetch from Hugging Face API (cursor-based pagination via Link header) ────
fetch_and_cache() {
  if [ "$FORCE_CACHE" = true ] && [ -f "$CACHE_FILE" ]; then
    echo "Using cached response from $CACHE_FILE"
    return
  fi

  echo "Fetching models released $LABEL from Hugging Face API..."

  pages=()
  next_url=""
  headers_tmp="/tmp/hf-latest-headers-$$.txt"

  for (( page=0; page<MAX_PAGES; page++ )); do
    # First page: build initial URL; subsequent pages: use full URL from Link header
    if [ -z "$next_url" ]; then
      url="${HF_API}?sort=createdAt&direction=-1&limit=${PER_PAGE}"
    else
      url="$next_url"
    fi

    printf "  Page %d ... " "$(( page + 1 ))"

    # Fetch with headers dumped so we can extract the Link header for pagination
    set +e
    response=$(curl -sS --max-time 30 \
      -H "User-Agent: orchicon-script/1.0" \
      -w "%{http_code}" \
      -D "$headers_tmp" \
      "$url" 2>&1)
    curl_exit=$?
    set -e

    # The last three characters from curl -w are the HTTP status code
    http_code="${response: -3}"
    body="${response:0:${#response}-3}"

    if [ "$curl_exit" -ne 0 ]; then
      echo "NETWORK ERROR (curl exit $curl_exit)"
      echo "Failed to reach Hugging Face API. Check your internet connection." >&2
      rm -f "$headers_tmp"
      exit 1
    fi

    if [ "$http_code" = "429" ]; then
      echo "RATE LIMITED (HTTP 429)"
      echo "Hugging Face API rate limit reached. Try again later or use --force-cache." >&2
      rm -f "$headers_tmp"
      exit 1
    fi

    if [ "$http_code" -ge 500 ]; then
      echo "SERVER ERROR (HTTP $http_code)"
      echo "Hugging Face API returned HTTP $http_code. Try again later." >&2
      rm -f "$headers_tmp"
      exit 1
    fi

    if [ "$http_code" != "200" ]; then
      echo "HTTP $http_code"
      echo "Unexpected response from Hugging Face API (HTTP $http_code)." >&2
      rm -f "$headers_tmp"
      exit 1
    fi

    count=$(echo "$body" | jq 'length' 2>/dev/null || echo "0")
    echo "got $count models"

    if [ "$count" -eq 0 ]; then
      echo "  No more models — stopping."
      rm -f "$headers_tmp"
      break
    fi

    pages+=("$body")

    # Extract the full next URL from the Link header.
    # The header looks like:
    #   Link: <https://huggingface.co/api/models?sort=createdAt&...&cursor=XYZ>; rel="next"
    # We extract the URL inside angle brackets and use it directly for the next request.
    link=$(grep -i '^link:' "$headers_tmp" 2>/dev/null | sed 's/^[Ll]ink: //' | tr -d '\r\n' || true)
    rm -f "$headers_tmp"

    if echo "$link" | grep -q 'rel="next"'; then
      # Extract the full URL between '<' and '>'
      next_url=$(echo "$link" | sed 's/.*<\([^>]*\)>.*/\1/')
    else
      echo "  No more pages — stopping."
      break
    fi
  done

  # Merge all pages into a single JSON array
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
  echo "ERROR: Invalid JSON in API response. Cache file: $CACHE_FILE" >&2
  echo "Raw response (first 500 chars):" >&2
  head -c 500 "$CACHE_FILE" >&2
  exit 1
fi

# ── Filter cached models by date (local filtering, since the HF API
#     does not support server-side date filtering) ────────────────────────────
if [ "$MODE" = "week" ]; then
  FILTERED_FILE=$(mktemp)
  jq --arg from "${WEEK_AGO}T00:00:00.000Z" \
    '[.[] | select(.createdAt >= $from)]' \
    "$CACHE_FILE" > "$FILTERED_FILE"
else
  FILTERED_FILE=$(mktemp)
  # For "today", match models whose createdAt starts with today's date string
  jq --arg today "$TODAY" \
    '[.[] | select(.createdAt // "" | startswith($today))]' \
    "$CACHE_FILE" > "$FILTERED_FILE"
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
  jq -r 'group_by(.pipeline_tag // "unlabeled")
    | map({tag: (.[0].pipeline_tag // "unlabeled"), count: length})
    | sort_by(-.count)
    | .[]
    | "    \(.tag): \(.count)"' "$FILTERED_FILE"
  echo ""

  jq -r '
    def stars: if .likes > 0 then "★\(.likes)" else "—" end;
    def downloads_fmt: if .downloads > 0 then "⬇\(.downloads)" else "—" end;

    .[] | [
      "  Model:      \(.id // "unknown")",
      "  Capability: \(.pipeline_tag // "unlabeled")",
      "  Libraries:  \(if .library_name? and .library_name != null and .library_name != "" then .library_name else "—" end)",
      "  Released:   \(.createdAt | sub("T"; " ") | sub("\\.[0-9]*Z"; "") | sub("Z"; ""))",
      "  Stats:      \(stars)  \(downloads_fmt)",
      "  ─────────────────────────────────────────────────────────",
      ""
    ] | join("\n")
  ' "$FILTERED_FILE"

  echo ""
} > "$OUTPUT_FILE"

rm -f "$FILTERED_FILE"

echo ""
echo "Done. Output written to: $OUTPUT_FILE"
echo ""
head -8 "$OUTPUT_FILE"
