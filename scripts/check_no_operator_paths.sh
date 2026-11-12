#!/usr/bin/env bash
# No-operator-paths CI gate.
#
# Fails if a tracked file carries a REAL PERSON's home directory — `/home/<name>` or `/Users/<name>` — in any
# file type, comments and fixtures included.
#
# WHY THIS IS A GATE AND NOT A ONE-OFF CLEANUP. The operator's rule, verbatim: "None of my personal info or
# directories should be in Orchicon and its codebase." Two distinct harms, and the second is the one a reviewer
# cannot see:
#
#   - it publishes where a specific person works;
#   - A FIXTURE PINNED TO ONE MACHINE'S LAYOUT IS A TEST THAT CAN ONLY PASS THERE. Found in the sweep that
#     motivated this gate: a ledger fixture whose only "realistic path" was the maintainer's checkout, a test
#     comment quoting a log line from someone's home, and a committed evidence snapshot recording who ran it
#     (and, in one case, a DIFFERENT person's name entirely — `justi` — in a Windows-path escaping test).
#
# WHAT IS DELIBERATELY NOT FLAGGED, and each is a judgement rather than an oversight:
#
#   - DOCUMENTATION PLACEHOLDERS: /home/me, /home/user, /home/ops, /home/operator, /home/somebody,
#     /home/runner, /home/example, /home/test, /home/<name>. These are the correct way to write an example,
#     they belong to nobody, and a test built on them is portable — which is exactly what this gate wants.
#   - THE SERVICE ACCOUNT: /home/orchicon is created by `useradd` INSIDE our own runtime images
#     (deploy/runtime/Dockerfile, and the Dockerfile every runtime image is generated from). It is a fact
#     about the product, not about a person.
#   - SYSTEM / PACKAGE-MANAGER LOCATIONS: /home/linuxbrew, /Users/Shared, /home/lib, /home/local, and the
#     other reserved names in ALLOWED below.
#   - RUNTIME EXPANSIONS: $HOME, ${HOME}, ~ — the correct pattern in code, and none of them is a literal path.
#   - THE MODULE PATH (github.com/beardedparrott/orchicon) and the repo slug: that is the project's IDENTITY,
#     not a filesystem location, and the whole import graph depends on it.
#
# THE ALLOWLIST IS BY BARE NAME, so it cannot be defeated by a different prefix, and a name is either a known
# placeholder or it is reported. Adding to it is a deliberate act with a reason, which is the point.
#
# Usage: scripts/check_no_operator_paths.sh
#   exit 0 = every home-shaped path in the tree is a placeholder, a service account, or a system location
#   exit 1 = one was found (path:line printed)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Placeholder and service-account names. Matched against the WHOLE user-name component.
# ops2 / ops-other / operations are NOT separate users: they are the prefix-SIBLINGS of the `ops`
# placeholder home, used by protectedpath tests to prove a boundary check does not treat one directory as
# being inside another that merely shares its prefix. Allowing them is the correct reading: they belong
# to nobody, and they exist to test the very logic that keeps one directory out of another.
#
# (This header cannot spell such a path literally — the gate scans its own source too, which is the
# behaviour you want: a file that explains the rule is not exempt from it.)
#
# Single/double-letter names (u, x) are the codebase's existing convention for a throwaway home in an
# escaping or env test; op-user and O'Brien exist for the two quoting shapes that actually bite (a
# hyphen, an apostrophe) and belong to nobody.
ALLOWED="me|u|x|user|users|op-user|O'Brien|ops|ops2|ops-other|operations|op|operator|admin|somebody|someone|runner|example|test|tester|you|yourname|name|username|orchicon|orchicon-host|linuxbrew|Shared|Default|Public|system|lib|local|bin|etc|opt|var|tmp|srv|git|root|nobody"

declare -a hits=()
while IFS= read -r -d '' f; do
  while IFS= read -r match; do
    [ -n "$match" ] && hits+=("$match")
  done < <(grep -HnIE "/(home|Users)/([A-Za-z][A-Za-z0-9._-]*)($|[/\"'\` ])" "$f" 2>/dev/null \
             | grep -vE "/(home|Users)/(${ALLOWED})($|[/\"'\` ])" || true)
done < <(git ls-files -z)

if [ "${#hits[@]}" -gt 0 ]; then
  cat <<'MSG'
ERROR: a real person's home directory is committed.

Orchicon must not carry anyone's personal paths — not in code, not in comments, not in docs, and not in
fixtures. A fixture pinned to one machine's layout is ALSO a test that can only pass there, so this is a
portability bug as much as a privacy one.

Fix, by case:
  - code            -> os.UserHomeDir(), $HOME, ~ at runtime
  - comments/docs   -> a placeholder: /home/me, /home/ops, <repo>, <home>, <data-dir>
  - fixtures        -> a placeholder PATH is correct and portable; it does not need to be a real one
  - evidence/logs   -> normalise before committing: <repo>, <home>, <data-dir>

If the name IS legitimate (a service account we create, a system location), add it — with a reason — to
ALLOWED in this script, deliberately.

MSG
  printf '%s\n' "${hits[@]}"
  exit 1
fi

echo "ok: no real person's home directory in any tracked file"
