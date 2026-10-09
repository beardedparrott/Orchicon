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

# --- the nav must have exactly TEN primary links (the count is the trigger) ---
#
# IT WAS NINE. Contact is the tenth: the operator asked for a contact link in the bar,
# and it earns its place there because it is the one link a visitor looks for by reflex.
# The count is asserted AT ALL because it is the trigger for the layout invariant below
# — a link added without checking the row's budget is exactly how this header broke
# twice. Note the added link is FLAT, so it takes `.nav nav a` in full: unsplittable and
# unshrinkable, and it lands in the gap budget rather than beside it.
COUNT="$(awk '/<nav aria-label="Primary"/{f=1} f; /<\/nav>/{if(f)exit}' "$PAGE" | grep -c '<a href')"
eq "primary nav link count" "10" "$COUNT"

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
num_le "base row gap fits the capped container" "$GAPS" "14"

# --- THE TIERS must stay inside the range where the container is still
#     SHRINKING. Above ~1248px the container is capped, so a max-width tier
#     there could never fire — dead code pretending to be a fix. ---
DEAD="$(printf '%s' "$CSS" | grep -o 'max-width:[0-9]*px' | tr -dc '0-9\n' | awk '$1 > 1248' | head -1)"
eq "no tier above the container cap (dead tier)" "" "$DEAD"

# --- the narrow-screen nav-hide must survive (spaces stripped from both sides) ---
HIDE="$(printf '%s' "$CSS" | tr -d ' ')"
has "narrow-screen nav-hide intact" "@media(max-width:860px){.navnav{display:none}}" "$HIDE"

# --- every tier that hides a link must name a real nth-child within 1..10 ---
#
# THE BOUND IS THE LINK COUNT, so it moved with it (nine -> ten): a tier naming an index
# no link can occupy is dead code, and the bound is what detects it. There are no
# nth-child tiers on the nav today — it GROUPS links rather than hiding them by index —
# so this guards a mechanism that a future edit might reintroduce.
BAD=""
for n in $(printf '%s' "$CSS" | grep -o 'nth-child([0-9]*)' | tr -dc '0-9\n'); do
  [ "$n" -ge 1 ] && [ "$n" -le 10 ] || BAD="$BAD $n"
done
eq "every nth-child tier is within 1..10" "" "$BAD"

# --- and the anti-regression that matters most: the nav's links must never all
#     be asked to fit at once.
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
num_le "inline items fewer than links (not all links inline)" "$TOP" "$((COUNT - 1))"

# --- the footer version must be STAMPED at build time, never hardcoded. A
#     literal here goes stale in silence: it read v0.4.0 while releases had
#     reached v0.4.5, and nothing would ever have corrected it. Assert the
#     marker build-site.sh rewrites is present. ---
has "footer version is a build-time stamp, not a literal" 'id="site-version"' "$(cat "$PAGE")"

# --- THE CONTACT SECTION must keep every channel it advertises, and keep each one
#     labelled. This is the page's only outward-facing contact surface, and nothing
#     else in CI can detect a channel going missing: the page is a CDN-served
#     document that no gate renders, so the assertion is made against its source
#     text — the same idiom as the footer stamp above.
#
#     THE ACCESSIBILITY HALF IS NOT DECORATION. Each tile's glyph is aria-hidden
#     (a brand mark conveys nothing a screen reader should read out), so the link's
#     accessible name comes ENTIRELY from its visible text. Drop the text and the
#     link silently becomes an unlabelled target — invisible to a keyboard-only or
#     screen-reader visitor, and invisible to every other check here.
CONTACT="$(awk '/<section class="block contact"/{f=1} f; /<\/section>/{if(f)exit}' "$PAGE")"
has "contact section is present" 'id="contact"' "$(cat "$PAGE")"
eq  "contact section links every channel" "6" "$(printf '%s' "$CONTACT" | grep -c '<a ')"
for u in \
  "https://discord.gg/PYUSGe5REq" \
  "https://www.youtube.com/@Orchicon" \
  "https://www.linkedin.com/company/orchicon" \
  "https://www.reddit.com/r/Orchicon" \
  "https://x.com/orchicon" \
  "https://github.com/beardedparrott/Orchicon" ; do
  has "contact link: ${u}" "$u" "$CONTACT"
done
# --- THE NAV's Contact LINK is asserted separately from the SECTION, because the two
#     can drift: the section is the content and this is the entry point. A nav link that
#     points at an anchor nothing defines scrolls nowhere and reads as a broken link, so
#     the target is checked against the section's own id rather than merely present.
NAV="$(awk '/<nav aria-label="Primary"/{f=1} f; /<\/nav>/{if(f)exit}' "$PAGE")"
has "primary nav links to the contact section" 'href="#contact"' "$NAV"
has "the contact target exists to be linked to"  'id="contact"'  "$(cat "$PAGE")"

# ============================================================================
# THE INSTALL SECTION: three methods, and the handler that switches them must
# see ONLY them.
#
# WHY THIS EXISTS. The Windows and Docker buttons did nothing — clicking either
# left the macOS/Linux command on screen. The page is a static document, so the
# cause was in the script, and it was one unscoped selector:
#
#   document.querySelectorAll('[role="tab"]')   // EVERY tab on the page
#
# There are THIRTEEN role="tab" elements here — five web-app tabs, four terminal
# tabs and these three — and only the last three carry `aria-controls` (the other
# two groups switch by data-web / data-tui). So `getAttribute('aria-controls')`
# returned null for the first tab in the document, getElementById(null) returned
# null, and `null.hidden = ...` threw. Because those tabs PRECEDE the install ones,
# the throw landed before the install trio was reached: aria-selected was never
# updated and the panel never un-hid.
#
# NOTHING IN CI RENDERS THIS PAGE (no browser in the container — see the header),
# so the assertions are made against the source text, the same idiom used above.
# ============================================================================
INSTALL="$(awk '/<section class="block" id="install"/{f=1} f; /<\/section>/{if(f)exit}' "$PAGE")"

# THE REGRESSION GUARD, stated POSITIVELY: the tabs are resolved from within their own
# tablist, so the query cannot reach another group's tabs. Stated positively on purpose —
# a negative assertion ("the old selector is absent") would be defeated by the selector
# being quoted in a comment, which is exactly where it is now explained.
has "install tabs are selected from their own tablist" '#install-tabs [role="tab"]' "$(cat "$PAGE")"

ITABS="$(printf '%s' "$INSTALL" | grep -c 'role="tab"')"
IPANELS="$(printf '%s' "$INSTALL" | grep -c 'role="tabpanel"')"
IHIDDEN="$(printf '%s' "$INSTALL" | grep -c 'role="tabpanel"[^>]*hidden')"
eq "the install section offers three methods" "3" "$ITABS"
eq "install tabs match install panels" "$ITABS" "$IPANELS"

# ONE VISIBLE AT REST, and this one matters because the SCRIPT does not establish it: unlike
# the web/terminal groups, select() is never called on load here — the initial state is the
# markup's own `hidden` attributes. A second un-hidden panel would render two commands on
# top of each other.
eq "exactly one install panel is shown at rest" "1" "$((IPANELS - IHIDDEN))"

# EVERY TAB CONTROLS A PANEL THAT EXISTS. A renamed panel id leaves a tab pointing at
# nothing: clicking it would un-hide null and throw all over again.
MISSING=""
for id in $(printf '%s' "$INSTALL" | grep -o 'aria-controls="[^"]*"' | sed 's/.*="//; s/"$//'); do
  grep -q "id=\"$id\"" "$PAGE" || MISSING="$MISSING $id"
done
eq "every install tab controls a panel that exists" "" "$MISSING"

# EVERY TAB HAS A NOTE. The note line is written from a lookup keyed by tab id, so a tab
# added without an entry renders the literal string "undefined" under the command.
NOTES="$(awk '/var notes = \{/{f=1} f; f&&/\};/{exit}' "$PAGE")"
UNNOTED=""
for id in $(printf '%s' "$INSTALL" | grep -o 'id="t-[^"]*"' | sed 's/^id="//; s/"$//'); do
  printf '%s' "$NOTES" | grep -q "\"$id\":" || UNNOTED="$UNNOTED $id"
done
eq "every install tab has a note (no 'undefined' note line)" "" "$UNNOTED"

# EACH METHOD SHOWS ITS OWN COMMAND — the reporter's actual ask was "show the right copy
# and paste line for Windows and Docker", and the failure mode that makes this worth
# asserting is a DUPLICATED panel: copy the macOS/Linux block to save typing and a Windows
# visitor is handed a bash one-liner. So each command is checked for its own shape, and the
# three are checked to be DISTINCT — a duplicate cannot pass both.
# The `id="c-x">` marker is STRIPPED before comparing: left on, the three strings differ
# by their own id and a duplicated command would still look "distinct" — the assertion
# would pass while asserting nothing. (Found by mutation-testing this very check.)
cmd_of() { printf '%s' "$INSTALL" | grep -o "id=\"$1\">[^<]*" | sed "s/^id=\"$1\">//"; }
CUNIX="$(cmd_of c-unix)"
CWIN="$(cmd_of c-win)"
CDOCK="$(cmd_of c-docker)"
has    "macOS/Linux shows the shell installer"      "install | bash" "$CUNIX"
has    "Windows shows the PowerShell installer"     "install.ps1"    "$CWIN"
has    "Docker shows a docker run"                  "docker run"     "$CDOCK"
absent "the Windows panel does not show the bash one-liner" "bash"    "$CWIN"
eq "each method shows a distinct command" "3" "$(printf '%s\n%s\n%s\n' "$CUNIX" "$CWIN" "$CDOCK" | sort -u | wc -l | tr -d ' ')"

# --- and the hero must NOT carry a second copy of the command ---
# The command lives in the install section, where each method gets its own; the hero's copy
# was a duplicate of the macOS/Linux line only, so it could silently disagree with it, and it
# invited a Windows visitor to paste a bash command. The hero keeps the two buttons.
absent "hero carries no competing install command" 'id="cmd-hero"' "$(cat "$PAGE")"

eq "every contact icon is hidden from assistive tech" "6" "$(printf '%s' "$CONTACT" | grep -c 'aria-hidden="true"')"
eq "every contact link carries a visible name"        "6" "$(printf '%s' "$CONTACT" | grep -c '<b>')"

echo
if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: all ${PASSED} assertions passed"
else
  echo "RESULT: ${FAILED} of $((PASSED + FAILED)) FAILED" >&2
fi
exit "$([ "$FAILED" -eq 0 ] && echo 0 || echo 1)"
