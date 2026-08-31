#!/usr/bin/env bash
# ============================================================================
# gen-release-notes.sh — release narrative + UPDATES.md lifecycle helper.
#
# RELEASE_HIGHLIGHTS.md is the curated, reader-facing story for the current
# release cycle ("what it is, why you'd use it" — whole features, not
# commits). It feeds BOTH public release surfaces:
#   - the GitHub Release body (narrative only — the itemized UPDATES.md
#     rows are an internal track record, deliberately NOT on the release
#     page), and
#   - README.md's "Last Release Changes" section (a compact digest — the
#     first sentence of each "### New:" section plus a link to the release
#     page).
#
# UPDATES.md remains the per-PR engineering log: monotonic row numbers that
# are NEVER renumbered; the "since last release" boundary is derived from
# the PREVIOUS RELEASE's UPDATES.md (read from git), so trimming released
# rows can't shift the boundary.
#
# Row format (one row per PR, appended newest-first at the top):
#   | # | Type | Phase | Summary |
#   Type ∈ Feature | Bug fix | Chore | Docs | Refactor | Test
#
# Modes:
#   scripts/gen-release-notes.sh [VERSION] [PREV_TAG]
#       Generate the GitHub Release body: the narrative section for the
#       release from RELEASE_HIGHLIGHTS.md (everything in the first
#       "## vX.Y.Z" section). Falls back to the old Type → Phase row block
#       when RELEASE_HIGHLIGHTS.md is absent. Emits markdown to stdout.
#       release.yml feeds this to `gh release create --notes-file`.
#
#   scripts/gen-release-notes.sh --trim [PREV_TAG]
#       Rewrite UPDATES.md in place, dropping rows already released (row
#       number <= the previous release's max). Keeps the current cycle only.
#       Run this on the next develop merge AFTER a release so the file stays
#       small. Safe to re-run; no-op when there is nothing to trim. Row
#       numbers are preserved (monotonic) — never renumber. Also syncs
#       README.md's "Last Release Changes" (compact digest, not rows).
#
#   scripts/gen-release-notes.sh --sync-readme [PREV_TAG]
#       Rewrite README.md's "## Last Release Changes" section in place to a
#       one-paragraph-level digest of RELEASE_HIGHLIGHTS.md — NOT the full
#       row list. README stays in sync with the highlights automatically —
#       no separate human step at release time. Public exposure is still
#       main-gated (CF Pages + release assets), so this is safe on develop.
#
# PREV_TAG resolution (trim mode only): the authoritative source is the
# GitHub Releases API (only the human's develop→main merge publishes a
# Release), falling back to a git-only heuristic when gh is unavailable.
# Pass "none" to include every row (first release / no trim).
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

MODE="body"
if [ "${1:-}" = "--trim" ]; then
  MODE="trim"
  shift
elif [ "${1:-}" = "--sync-readme" ]; then
  MODE="sync-readme"
  shift
fi

VERSION="${1:-}"
PREV_TAG="${2:-}"

# Resolve the version being released (body mode only).
if [ "$MODE" = "body" ] && [ -z "$VERSION" ]; then
  if [ -n "${GITHUB_REF_NAME:-}" ]; then
    VERSION="$GITHUB_REF_NAME"
  else
    VERSION="$(git describe --tags --abbrev=0 2>/dev/null || echo "latest")"
  fi
fi

# Resolve the previous release tag. The authoritative source is the GitHub
# Releases API: only the human's develop→main merge publishes a Release, so
# `gh release list` returns exactly the prior release (the current one is not
# created yet when the notes are generated). This deliberately ignores git
# tags that are NOT releases — the develop version-bump tags (v0.1.x on
# develop) are ancestors of main after the merge and would otherwise be
# mistaken for the prior release. Falls back to a git-only heuristic only
# when gh is unavailable.
if [ -z "$PREV_TAG" ]; then
  if command -v gh >/dev/null 2>&1 && [ -n "${GH_TOKEN:-}" ]; then
    PREV_TAG="$(gh release list --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null || true)"
  fi
fi
if [ -z "$PREV_TAG" ]; then
  PREV_TAG="$(git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | \
    while read -r t; do
      if ! git merge-base --is-ancestor "$t" HEAD 2>/dev/null; then
        echo "$t"
        break
      fi
    done || true)"
fi

# Load the previous release's UPDATES.md row numbers (the boundary).
prev_max=0
if [ -n "$PREV_TAG" ] && [ "$PREV_TAG" != "none" ]; then
  if prev_content="$(git show "${PREV_TAG}:UPDATES.md" 2>/dev/null)"; then
    prev_max="$(printf '%s\n' "$prev_content" | awk -F'|' '$2 ~ /^[ ]*[0-9]+[ ]*$/ {gsub(/ /,"",$2); print $2}' | sort -n | tail -1 || true)"
    prev_max="${prev_max:-0}"
  fi
fi

# Parse UPDATES.md rows (monotonic id, type, phase, summary). awk splits on
# '|'; Type/Phase never contain a pipe, Summary may (join fields 5..NF-1).
# Emit "id<TAB>type<TAB>phase<TAB>summary" for every row.
rows() {
  awk -F'|' '
    NR == 1 { next }
    $2 ~ /^[ ]*[0-9]+[ ]*$/ {
      id=$2; gsub(/^[ ]+|[ ]+$/, "", id)
      typ=$3; gsub(/^[ ]+|[ ]+$/, "", typ)
      phase=$4; gsub(/^[ ]+|[ ]+$/, "", phase)
      summary=""
      for (i=5; i<NF; i++) {
        if (i>5) summary=summary "|"
        summary=summary $i
      }
      gsub(/^[ ]+|[ ]+$/, "", summary)
      print id "\t" typ "\t" phase "\t" summary
    }
  ' UPDATES.md
}

# render_block turns "id<TAB>type<TAB>phase<TAB>summary" lines into markdown:
# rows grouped by "Type — Phase", newest first. (Legacy fallback — used only
# when RELEASE_HIGHLIGHTS.md does not exist.)
render_block() {
  awk -F'\t' '
    {
      key=$2 "\t" $3
      if (!(key in seen)) { order[n++] = key; seen[key] = 1 }
      bullets[key] = bullets[key] sprintf("- **#%s** — %s\n", $1, $4)
    }
    END {
      for (i=0; i<n; i++) {
        split(order[i], k, "\t")
        printf "### %s — %s\n\n%s", k[1], k[2], bullets[order[i]]
        if (i < n-1) printf "\n"
      }
    }
  '
}

# render_highlights prints the highlights narrative for the given version
# (the "## <version>" section of RELEASE_HIGHLIGHTS.md, up to but not
# including the next "## " heading). With no version argument (or when the
# version has no exact section), it renders the FIRST "## " section — the
# current release cycle by file contract. Empty when the file has no
# "## " section. Warns on stderr when an explicitly requested version is
# missing so a stale heading is visible at release time, then falls back.
render_highlights() {
  local version="$1"
  local highlights_file="${REPO_ROOT}/RELEASE_HIGHLIGHTS.md"
  [ -f "$highlights_file" ] || return 0
  local first_section
  first_section="$(awk '/^## / { print substr($0, 4); exit }' "$highlights_file")"
  if [ -n "$version" ] && [ "$version" != "$first_section" ]; then
    echo "::warning::RELEASE_HIGHLIGHTS.md has no '## ${version}' section (current cycle heading is '${first_section}') — using the first section." >&2
  fi
  awk '
    /^## / { if (started++) exit; else { next } }
    started { print }
  ' "$highlights_file"
}

# render_readme_digest turns the highlights narrative into the compact
# README digest: the feature name + first sentence of each "### New:"
# section, plus the "Also in this release" bullets and the release-page
# pointer. NOT the row list — README carries the story in one screen.
render_readme_digest() {
  render_highlights "" | awk '
    BEGIN { in_also = 0; cur = 0 }
    /^### / {
      sub(/^### /, "")
      in_also = 0
      if ($0 ~ /^New: */) {
        sub(/^New: */, "")
        gsub(/ — .*$/, "")
        bullets[++n] = "**" $0 "**: "
        pending[n] = 1
        cur = n
      } else if ($0 ~ /^Also in this release/) {
        in_also = 1
      }
      next
    }
    /^## / { exit }
    in_also && /^- / {
      line = $0
      sub(/^- /, "", line)
      bullets[++n] = line
      pending[n] = 0
      next
    }
    /^[[:space:]]*$/ { next }
    {
      # First paragraph line of the current "### New:" section completes
      # its bullet; later paragraphs are dropped.
      if (cur > 0 && pending[cur]) {
        text = $0
        gsub(/^[[:space:]]+/, "", text)
        idx = index(text, ". ")
        if (idx > 0) text = substr(text, 1, idx)
        bullets[cur] = bullets[cur] text
        pending[cur] = 0
      }
    }
    END {
      for (i = 1; i <= n; i++) if (!pending[i]) print "- " bullets[i]
      print ""
      print "Full details: [release notes on GitHub](https://github.com/beardedparrott/Orchicon/releases)."
    }
  '
}

# sync_readme rewrites README.md's "## Last Release Changes" section in place
# to the given markdown block, preserving the rest of the file. Public
# exposure is main-gated (CF Pages + release assets), so this is safe on
# develop — README simply tracks the current UPDATES.md cycle.
sync_readme() {
  local block="$1"
  local readme="README.md"
  [ -f "$readme" ] || { echo "README.md not found — nothing to sync"; return; }
  local tmp
  tmp="$(mktemp)"
  awk -v heading="## Last Release Changes" -v block="$block" '
    BEGIN { in_section = 0; written = 0 }
    {
      if (!in_section && $0 == heading) {
        print
        print ""
        if (block != "") { print block } else { print "_No changes since the previous release._" }
        in_section = 1
        written = 1
        next
      }
      if (in_section && /^## /) {
        in_section = 0
        print ""
      }
      if (!in_section) print
    }
    END {
      if (!written) {
        printf "\n%s\n\n%s\n", heading, (block != "" ? block : "_No changes since the previous release._")
      }
    }
  ' "$readme" > "$tmp" && mv "$tmp" "$readme"
}

if [ "$MODE" = "trim" ]; then
  # Trim is a FAIL-SAFE maintenance step run as part of a develop PR after a
  # release: it drops released rows (id <= prev_max) and keeps the current
  # cycle. If the previous release cannot be determined (no gh token AND no
  # explicit PREV_TAG), it does NOTHING — silently trimming everything would
  # be worse than leaving the file as-is.
  if [ -z "$PREV_TAG" ] || [ "$PREV_TAG" = "none" ]; then
    echo "Cannot determine the previous release (no gh token / no PREV_TAG) — refusing to trim."
    exit 0
  fi
  all="$(rows)"
  [ -z "$all" ] && { echo "UPDATES.md has no rows to trim"; exit 0; }
  kept="$(printf '%s\n' "$all" | awk -F'\t' -v prev="$prev_max" '$1 > prev')"
  removed="$(printf '%s\n' "$all" | awk -F'\t' -v prev="$prev_max" '$1 <= prev' | wc -l | tr -d ' ')"
  if [ "$removed" = "0" ]; then
    echo "Nothing to trim (previous release already covers all rows)."
    exit 0
  fi
  kept_range="$(printf '%s\n' "$kept" | awk -F'\t' '{ if ($1<min||min=="") min=$1; if ($1>max) max=$1 } END { print min ".." max }')"

  # Rebuild the file: preserve everything up to the separator row
  # ("|---|---|---|---|"), then emit the kept rows as a table. This keeps the
  # title + description block and the header regardless of wording.
  tmp="$(mktemp)"
  sed -n '1,/^|-[| -]*$/p' UPDATES.md > "$tmp"
  printf '%s\n' "$kept" | awk -F'\t' '{ printf "| %s | %s | %s | %s |\n", $1, $2, $3, $4 }' >> "$tmp"
  mv "$tmp" UPDATES.md
  echo "Trimmed ${removed} released row(s) from UPDATES.md; kept the current cycle (rows ${kept_range})."

  # Keep README.md's "Last Release Changes" in sync with the highlights
  # (compact digest — see render_readme_digest; NOT the row list). The
  # no-version call digests the current-cycle section by contract.
  sync_readme "$(render_readme_digest "")"
  exit 0
fi

# body / sync-readme mode: prefer the curated highlights narrative; fall
# back to the itemized row block only when RELEASE_HIGHLIGHTS.md is absent.
# body / sync-readme mode: README gets the compact highlights digest (never
# the row list); the release body gets the full narrative — falling back to
# the itemized row block only when RELEASE_HIGHLIGHTS.md is absent.
if [ "$MODE" = "sync-readme" ]; then
  sync_readme "$(render_readme_digest "")"
  echo "Synced README.md 'Last Release Changes' to the release highlights digest."
  exit 0
fi

highlight_block="$(render_highlights "${VERSION:-}")"
if [ -n "$highlight_block" ]; then
  printf '## %s\n\n' "${VERSION:-}"
  # Trim leading blank lines so the heading sits flush against the story.
  printf '%s\n\n' "$(printf '%s' "$highlight_block" | sed '/./,$!d')"
  exit 0
fi

new_rows="$(printf '%s\n' "$(rows)" | awk -F'\t' -v prev="$prev_max" '$1 > prev')"
if [ -n "$new_rows" ]; then
  block="$(printf '%s\n' "$new_rows" | render_block)"

  if [ "$MODE" = "sync-readme" ]; then
    sync_readme "$block"
    echo "Synced README.md 'Last Release Changes' to the current UPDATES.md cycle."
    exit 0
  fi

  printf '## %s\n\n' "$VERSION"
  printf '%s\n' "$block"
fi
