#!/usr/bin/env bash
# fe-deps.sh — make `frontend/node_modules` usable, or say precisely why it is not.
#
# THE BUG THIS EXISTS FOR. `frontend/node_modules` was a SYMLINK POINTING AT ITSELF:
#
#     node_modules -> /home/beardedparrott/projects/Orchicon/frontend/node_modules
#
# Every path under it therefore failed with ELOOP, `tsc -b` died, npm exited 216, and `make rebuild-dev`
# reported only:
#
#     make[2]: *** [Makefile:707: fe-build] Error 216
#
# A number with no cause. The 216 IS the symptom of a broken symlink, but nothing in the output said so, and
# the fix (delete the link, reinstall) is not something a reader can infer from an exit code.
#
# A SYMLINK HERE IS LEGITIMATE IN ONE CASE: a worktree links the main checkout's node_modules so it does not
# pay for a second install. So the rule is not "no symlinks" — it is "the target must RESOLVE to a real
# directory". That covers a self-link, a dangling link, and a loop, while leaving the worktree case alone.
#
# usage: fe-deps.sh [--frontend <dir>] [--npm <path>] [--check-only]
set -euo pipefail

frontend="frontend"
npm_bin=""
check_only=0
while [ $# -gt 0 ]; do
  case "$1" in
    --frontend)   frontend="$2"; shift 2 ;;
    --npm)        npm_bin="$2"; shift 2 ;;
    --check-only) check_only=1; shift ;;
    -h|--help)    sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)            echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

nm="$frontend/node_modules"

# resolve_ok reports whether $nm names a real directory (following symlinks).
#
# `-d` alone is not enough: it FOLLOWS the link, so on a loop it returns false and on a dangling link too —
# but it cannot DISTINGUISH them, and the message has to say which, because "delete it" is only obviously
# right for one of them. `readlink -f` resolves the whole chain and fails on a loop, which is the useful
# signal.
resolve_ok() {
  [ -d "$nm" ] && [ -n "$(readlink -f "$nm" 2>/dev/null)" ]
}

if resolve_ok; then
  if [ -L "$nm" ]; then
    echo "==> $nm -> $(readlink -f "$nm") (linked; usable)"
  fi
  exit 0
fi

# --- broken: diagnose before doing anything -----------------------------------
if [ -e "$nm" ] || [ -L "$nm" ]; then
  target="$(readlink "$nm" 2>/dev/null || echo '(not a symlink)')"
  echo "==> $nm is BROKEN"
  echo "    target: $target"
  case "$target" in
    "$(readlink -f "$frontend" 2>/dev/null)/node_modules"|"$(cd "$frontend" && pwd -P)/node_modules")
      echo "    it points at ITSELF, so every path under it fails with \"Too many levels of symbolic links\"."
      echo "    This is what turns 'make rebuild-dev' into a bare \"Error 216\"." ;;
  esac
  if [ "$check_only" = "1" ]; then
    echo "    fix: rm -f $nm && (cd $frontend && npm ci)" >&2
    exit 1
  fi
  echo "==> removing the broken link and reinstalling"
  rm -f "$nm"
fi

if [ "$check_only" = "1" ]; then
  echo "    fix: (cd $frontend && npm ci)" >&2
  exit 1
fi

# --- install ------------------------------------------------------------------
if [ -z "$npm_bin" ]; then
  if command -v npm >/dev/null 2>&1; then
    npm_bin="$(command -v npm)"
  elif [ -x "$(dirname "$0")/../..//.dev/tools/node/bin/npm" ]; then
    npm_bin="$(dirname "$0")/../..//.dev/tools/node/bin/npm"
  else
    echo "ERROR: npm is not on PATH and not in .dev/tools/node/bin — install Node, or set --npm <path>" >&2
    exit 1
  fi
fi
echo "==> installing frontend dependencies with $npm_bin"
cd "$frontend"
if [ -f package-lock.json ]; then
  "$npm_bin" ci
else
  "$npm_bin" install
fi
echo "==> $nm is ready"
