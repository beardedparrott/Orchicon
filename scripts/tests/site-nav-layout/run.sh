#!/usr/bin/env bash
# ============================================================================
# site-nav-layout/run.sh — assert the landing page's primary nav cannot break
# a label, and cannot silently regress to the squeezed header it had.
#
# WHY THIS EXISTS: the header broke TWICE on the way to being fixed, and both
# times the defect was in the CSS of a page nothing in CI renders.
#
#   1. Adding the "Pricing" link made NINE primary links. The row is a flex row,
#      so its items shrank, and with no `white-space:nowrap` a label broke
#      mid-phrase ("Web"/"app") inside the 64px bar.
#   2. The first fix retuned the link-HIDING tiers, which cannot help, because
#      `.wrap{width:min(1200px, 100% - 48px)}` CAPS at 1200px: from ~1248px up
#      the nav's available width is constant, so nine links overflow at their
#      WIDEST and no `max-width` tier can reach that case.
#
# The lesson is an INVARIANT rather than a number: a nav label must never be
# splittable or shrinkable (`nowrap` + `flex:none`), the row must be able to wrap
# (`flex-wrap:wrap`) so a font-metric miss degrades to a second line instead of to
# the reported break, and nine links must never all be laid out inline. How the
# last one is achieved is NOT pinned: the nav hid links by nth-child, then grouped
# them behind collapsed menus, and the assertion follows the intent rather than the
# mechanism — see the grouping check at the end for why that distinction matters.
# This asserts the invariant, not a pixel width — the widths depend on platform font
# metrics, and this container has no browser to measure them with (Chromium cannot
# launch: 20 missing shared libraries, no root to install them).
#
# Deliberately dependency-free: it reads the committed CSS as text, the same
# "source assertion" idiom the frontend uses for things it cannot render
# (frontend/src/components/ui/mode-toggle-source.test.ts).
#
#   scripts/tests/site-nav-layout/run.sh [site/index.html]
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
PAGE="${REPO_ROOT}/site/index.html"
[ "$#" -gt 0 ] && PAGE="$1"

[ -f "$PAGE" ] || { echo "run.sh: no such page: ${PAGE}" >&2; exit 2; }

PASSED=0
FAILED=0
_record() { # <ok(0|1)> <label> <detail>
  if [ "$1" -eq 0 ]; then
    printf '  \033[32mPASS\033[0m  %-58s %s\n' "$2" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-58s %s\n' "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}
# eq <label> <want> <got> — exact string equality.
eq() { [ "$2" = "$3" ] && _record 0 "$1" "$3" || _record 1 "$1" "want [$2] got [$3]"; }
# has <label> <needle> <haystack> — substring presence.
has() {
  case "$3" in
    *"$2"*) _record 0 "$1" "present" ;;
    *)       _record 1 "$1" "MISSING ($2)" ;;
  esac
}
# absent <label> <needle> <haystack> — substring absence.
absent() {
  case "$3" in
    *"$2"*) _record 1 "$1" "PRESENT ($2)" ;;
    *)       _record 0 "$1" "absent" ;;
  esac
}
# num_le <label> <value> <max> — numeric bound.
num_le() {
  case "$2" in
    ''|*[!0-9]*) _record 1 "$1" "not a number: [$2]" ;;
    *) [ "$2" -le "$3" ] && _record 0 "$1" "${2} <= ${3}" || _record 1 "$1" "${2} > ${3}" ;;
  esac
}
# num_ge <label> <value> <min>
num_ge() {
  case "$2" in
    ''|*[!0-9]*) _record 1 "$1" "not a number: [$2]" ;;
    *) [ "$2" -ge "$3" ] && _record 0 "$1" "${2} >= ${3}" || _record 1 "$1" "${2} < ${3}" ;;
  esac
}

# --- the CSS, flattened to ONE line so a rule can be read as a unit ----------
# EVERY declaration for a selector must be collected, not the last one written:
# `tail -1` on `.nav nav a` finds the later padding-only rule and reports a
# missing `nowrap` that is present. That was a bug in this harness's first draft.
CSS="$(awk '/<style[^>]*>/{f=1;next} /<\/style>/{f=0} f' "$PAGE" | tr '\n' ' ' | tr -s ' ')"
[ -n "$CSS" ] || { echo "run.sh: no <style> block found in ${PAGE}" >&2; exit 2; }

# all_decls <selector-regex> — concatenate every matching rule's body verbatim.
all_decls() {
  printf '%s' "$CSS" | grep -oE "$1\\{[^}]*\\}" | tr -d '{}' | tr '\n' ' '
}

echo "page under test: ${PAGE}"
echo

# --- the nav must have exactly nine primary links (the count is the trigger) ---
COUNT="$(awk '/<nav aria-label="Primary"/{f=1} f; /<\/nav>/{if(f)exit}' "$PAGE" | grep -c '<a href')"
eq "primary nav link count" "9" "$COUNT"

# --- LINK INVARIANT: a label is never split and never shrunk below its text ---
LINK_DECLS="$(all_decls '\.nav nav a')"
has "nav link is nowrap (a label is never split)" "white-space:nowrap" "$LINK_DECLS"
has "nav link is flex:none (never shrunk)"         "flex:none"         "$LINK_DECLS"

# --- ROW BACKSTOP: the row wraps rather than breaking a label or spilling ---
ROW_DECLS="$(all_decls '\.nav nav')"
has "nav row can wrap (backstop)" "flex-wrap:wrap"           "$ROW_DECLS"
has "nav row wraps right-aligned" "justify-content:flex-end" "$ROW_DECLS"

# --- THE HEADER BOX MAY GROW, so a wrapped second line is not clipped ---------
# Checked with the `min-height` declaration REMOVED first: `min-height:64px`
# contains `height:64px`, so a naive substring test reports a fixed height on
# correct CSS. Also a bug in this harness's first draft.
BOX_DECLS="$(all_decls '\.nav \.wrap')"
has "header box is min-height (can grow)" "min-height:64px" "$BOX_DECLS"
absent "header box is not a fixed height" "height:64px" "$(printf '%s' "$BOX_DECLS" | sed 's/min-height:[^;]*//g')"

# --- THE GAP BUDGET: the row's own gaps are the lever that actually fixed this.
#     `row-gap` also matches 'gap:', so the row-gap is excluded explicitly; the
#     LARGEST remaining value is the base (narrower tiers only ever reduce it).
GAPS="$(printf '%s' "$ROW_DECLS" | grep -o 'gap:[0-9]*px' | grep -v 'row-gap' | tr -dc '0-9\n' | sort -n | tail -1)"
num_le "base row gap fits nine in the capped container" "$GAPS" "14"

# --- THE TIERS must stay inside the range where the container is still
#     SHRINKING. Above ~1248px the container is capped, so a max-width tier
#     there could never fire — dead code pretending to be a fix. ---
DEAD="$(printf '%s' "$CSS" | grep -o 'max-width:[0-9]*px' | tr -dc '0-9\n' | awk '$1 > 1248' | head -1)"
eq "no tier above the container cap (dead tier)" "" "$DEAD"

# --- the narrow-screen nav-hide must survive (spaces stripped from both sides) ---
HIDE="$(printf '%s' "$CSS" | tr -d ' ')"
has "narrow-screen nav-hide intact" "@media(max-width:860px){.navnav{display:none}}" "$HIDE"

# --- every tier that hides a link must name a real nth-child within 1..9 ---
BAD=""
for n in $(printf '%s' "$CSS" | grep -o 'nth-child([0-9]*)' | tr -dc '0-9\n'); do
  [ "$n" -ge 1 ] && [ "$n" -le 9 ] || BAD="$BAD $n"
done
eq "every nth-child tier is within 1..9" "" "$BAD"

# --- and the anti-regression that matters most: nine links must never all be
#     asked to fit at once.
#
#     THE MECHANISM CHANGED, SO THIS ASSERTION HAD TO. The nav used to HIDE
#     links by nth-child as the viewport narrowed; it now GROUPS them behind
#     collapsed menus (the markup says so). Asserting the old mechanism pinned a
#     design that was deliberately replaced, so it reported a failure against
#     correct markup — a stale assertion, not a regression. It went unnoticed
#     because nothing runs this script.
#
#     The INTENT is unchanged and is what is asserted here: fewer items are laid
#     out inline than the nav contains, because the rest are collapsed.
MENU_DECLS="$(all_decls '\.navmenu')"
has "nav menu panels are collapsed by default" "display:none" "$MENU_DECLS"
has "a collapsed menu opens when its group is active" "display:block" \
    "$(all_decls '\.navgroup\[data-open="true"\] \.navmenu')"
TOP="$(awk '/<nav aria-label="Primary"/{f=1} f; /<\/nav>/{if(f)exit}' "$PAGE" | awk '
  inmenu { depth += gsub(/<div/,"&",$0) - gsub(/<\/div>/,"&",$0); if (depth <= 0) inmenu = 0; next }
  /<div class="navmenu"/ { inmenu = 1; depth = gsub(/<div/,"&",$0) - gsub(/<\/div>/,"&",$0); next }
  /<(a href|button)/ { top++ }
  END { print top+0 }')"
num_le "inline items fewer than links (not all nine inline)" "$TOP" "$((COUNT - 1))"

# --- the footer version must be STAMPED at build time, never hardcoded. A
#     literal here goes stale in silence: it read v0.4.0 while releases had
#     reached v0.4.5, and nothing would ever have corrected it. Assert the
#     marker build-site.sh rewrites is present. ---
has "footer version is a build-time stamp, not a literal" 'id="site-version"' "$(cat "$PAGE")"

echo
if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: all ${PASSED} assertions passed"
else
  echo "RESULT: ${FAILED} of $((PASSED + FAILED)) FAILED" >&2
fi
exit "$([ "$FAILED" -eq 0 ] && echo 0 || echo 1)"
