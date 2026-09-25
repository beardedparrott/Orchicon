package guard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// The execution guard is the OS-level backstop for worker safety.
//
// The opencode permission rules (config.go permissionRules) only see the
// exact command the Bash tool runs. A destructive command issued from
// inside a subprocess — a python TUI calling subprocess.run(["rm","-rf",
// "/"]), os.system("rm -rf /"), etc. — is invisible to them, because
// opencode only ever sees "python tui.py". That is precisely how the
// 2026-07-30 /home wipe got through.
//
// The guard closes that hole for EVERY worker execution by shimming
// dangerous binaries ahead of the worker's PATH. Any process the worker
// spawns — opencode's Bash tool, python, a TUI, anything — resolves
// `rm`, `sudo`, `dd`, `mkfs`, etc. through PATH and hits the shim, which
// refuses the command. This works regardless of how deep in a subprocess
// tree the command is issued.
//
// Policy:
//   - always-block binaries (sudo, dd, mkfs*, fdisk, parted, shred,
//     wipefs, LVM tooling, mkswap): never legitimately needed inside a
//     project directory and catastrophic on a host — refused unconditionally.
//   - path-scoped binaries (rm, chmod, chown, mv, cp, ln): allowed only
//     when every path argument resolves inside the worker's project
//     directory; any target outside it (/, ~, $HOME, /home, ..) is refused.
//
// The guard is defense-in-depth, not a substitute for containers: a
// determined worker could still call /bin/rm by absolute path or write
// its own binary. That residual risk is what the containerized execution
// option (internal/opencode/README) closes. See also AGENTS.md.
//
// THE INTERACTIVE (Ask) PROFILE. The same shim serves the Ask path, switched
// into a GRANT-AWARE, FAIL-CLOSED profile by ORCHICON_GUARD_POLICY (see
// InteractiveEnviron). There the path-scoped check consults the SAME policy
// accessor the consent core reads (internal/permpolicy), plus the
// conversation's project dir, its session grants and the operator-approved
// once-targets — and it REFUSES the command when the policy cannot be read or
// parsed, because a guard that silently stops guarding is the failure this
// component was built after. The always-block class is unchanged: sudo/dd/
// mkfs-level capability is not up for consent.

// ScratchDir is the one writable scratch area outside the project directory
// (kept in sync with opencode.ScratchDir). Scoped binaries (rm/mv/cp/…)
// are allowed to operate inside it — it is Orchicon-owned ephemeral
// scratch (screenshots, logs, downloads), so guarding it like a foreign path
// burns worker tokens on "blocked" retries for no safety gain. Everything
// else outside the project stays blocked.
const ScratchDir = "/tmp/orchicon"

// The INTERACTIVE profile's environment contract. Only the Ask path
// (internal/askorchicon) sets these; the worker path sets none of them and is
// byte-for-byte unchanged. They ride the bash environment the Ask path already
// REPLACES per invocation (orchicon.HostTools.SetBashEnviron) — the same
// channel the shim's PATH entry uses, so no new constructor and no global.
const (
	// PolicyEnvVar non-empty switches the shim into the INTERACTIVE profile:
	// grant-aware, and FAIL-CLOSED (an unreadable or unparsable policy REFUSES
	// the path-scoped command rather than allowing it).
	PolicyEnvVar = "ORCHICON_GUARD_POLICY"
	// ProjectEnvVar is the conversation's project directory — the default
	// scope, where writes proceed without an ask.
	ProjectEnvVar = "ORCHICON_GUARD_PROJECT"
	// GrantsEnvVar lists the conversation's session-granted directories,
	// newline-separated (NOT ':'-separated: a path may contain a colon).
	GrantsEnvVar = "ORCHICON_GUARD_GRANTS"
	// OnceEnvVar lists the absolute targets the operator approved `once`,
	// newline-separated, so the approved command runs while a sibling path a
	// subprocess inside it targets is still refused.
	OnceEnvVar = "ORCHICON_GUARD_ONCE"
)

// InteractiveEnviron returns the ORCHICON_GUARD_* key/values that switch the
// shim into the interactive profile. An empty policyPath returns nil — that IS
// the worker profile, which is the frozen half of "no path-scoped check
// changes for a worker".
func InteractiveEnviron(policyPath, projectDir string, grants, once []string) []string {
	if strings.TrimSpace(policyPath) == "" {
		return nil
	}
	return []string{
		PolicyEnvVar + "=" + policyPath,
		ProjectEnvVar + "=" + projectDir,
		GrantsEnvVar + "=" + strings.Join(grants, "\n"),
		OnceEnvVar + "=" + strings.Join(once, "\n"),
	}
}

// guardedBinary names one binary the guard shims on PATH.
type guardedBinary struct {
	name   string
	scoped bool // true: allow when targets stay in the project; false: always block
}

// buildGuardedBinaries is the shim set for one guard: the never-allow class
// (always-block, from the SHARED declaration in internal/neverallow so the guard
// and the opencode permission config cannot drift apart) followed by the
// path-scoped binaries, which are allowed when every target stays inside the
// project directory.
//
// It is computed per guard rather than cached in a package variable: the shared
// declaration is the single source of truth, so a guard must read it when it is
// built rather than inherit whatever it held at package init.
func buildGuardedBinaries() []guardedBinary {
	scoped := []guardedBinary{
		{name: "rm", scoped: true},
		{name: "chmod", scoped: true},
		{name: "chown", scoped: true},
		{name: "mv", scoped: true},
		{name: "cp", scoped: true},
		{name: "ln", scoped: true},
	}
	out := make([]guardedBinary, 0, len(neverallow.Shimmed())+len(scoped))
	for _, name := range neverallow.Shimmed() {
		out = append(out, guardedBinary{name: name})
	}
	return append(out, scoped...)
}

// Guard is a generated shim directory prepended to a worker's
// PATH. `dir` holds one `guard` script plus symlinks named after each
// guarded binary. `real` maps scoped binary names to their resolved
// absolute paths on the host (the shim execs the real binary when it
// decides the command is safe).
type Guard struct {
	dir  string
	real map[string]string
	// policyPath is the operator's persistent permission policy file, read
	// PER INVOCATION by the shim's policy_lookup() (no cache, no watcher —
	// an edit takes effect on the next command). Empty disables the check.
	// The interactive profile may point the shim at a different file per
	// invocation through PolicyEnvVar, which overrides this baked default.
	policyPath string
}

// MakeGuard creates a guard shim inside targetDir (which must already
// exist and be writable) for the given projectDir, and returns the
// absolute path of the generated shim subdir. It exists so the
// in-container runtime agent can run workers under the same safety shim
// inside workflow runtime containers, where the control plane's own /tmp
// is not reachable.
func MakeGuard(targetDir, projectDir string) (string, error) {
	dir, err := os.MkdirTemp(targetDir, "orchicon-guard-*")
	if err != nil {
		return "", err
	}
	g, err := buildGuardIn(dir, projectDir, permpolicy.DefaultPath())
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return g.dir, nil
}

// NewExecutionGuard creates a guard for one execution. projectDir is the
// worker's working directory (may be empty — absolute targets are still
// blocked). The returned guard must be closed (removes the shim dir).
func NewExecutionGuard(projectDir string) (*Guard, error) {
	return NewExecutionGuardWithPolicy(projectDir, permpolicy.DefaultPath())
}

// NewExecutionGuardWithPolicy is NewExecutionGuard with an explicit policy
// file (tests, and any caller that resolves the path itself). An empty
// policyPath renders a shim without the deny check.
func NewExecutionGuardWithPolicy(projectDir, policyPath string) (*Guard, error) {
	dir, err := os.MkdirTemp("", "orchicon-guard-*")
	if err != nil {
		return nil, err
	}
	g, err := buildGuardIn(dir, projectDir, policyPath)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return g, nil
}

// buildGuardIn renders the guard script + symlinks into dir (created by
// the caller). projectDir is the worker's working directory.
func buildGuardIn(dir, projectDir, policyPath string) (*Guard, error) {
	if projectDir != "" {
		if abs, err := filepath.Abs(projectDir); err == nil {
			projectDir = abs
		}
	}

	g := &Guard{dir: dir, real: make(map[string]string), policyPath: policyPath}
	binaries := buildGuardedBinaries()

	// Resolve the real binary paths for scoped binaries. They are always
	// present on a working Linux/macOS host; a missing one means the
	// binary doesn't exist and nothing needs shimming.
	for _, b := range binaries {
		if b.scoped {
			g.real[b.name] = resolveRealBin(b.name)
		}
	}

	data := struct {
		ProjectDir     string
		Real           map[string]string
		ScratchDir     string
		NeverAllowCase string
		PolicyFile     string
	}{projectDir, g.real, ScratchDir, neverallow.CasePattern(), policyPath}

	tmpl, err := template.New("guard").Parse(guardScriptTemplate)
	if err != nil {
		g.Close()
		return nil, fmt.Errorf("guard: template: %w", err)
	}
	script := filepath.Join(dir, "guard")
	f, err := os.OpenFile(script, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		g.Close()
		return nil, fmt.Errorf("guard: write script: %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		g.Close()
		return nil, fmt.Errorf("guard: render script: %w", err)
	}
	f.Close()

	// Symlink each guarded name to the script. Names whose binary is
	// absent on the host are skipped (nothing to intercept).
	for _, b := range binaries {
		if b.scoped && g.real[b.name] == "" {
			continue
		}
		link := filepath.Join(dir, b.name)
		if err := os.Symlink("guard", link); err != nil {
			g.Close()
			return nil, fmt.Errorf("guard: symlink %s: %w", b.name, err)
		}
	}
	return g, nil
}

// apply returns environ with the guard directory first on PATH, so every
// process the worker spawns resolves guarded binaries through the shim.
func (g *Guard) Apply(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	found := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+g.dir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+g.dir)
	}
	return out
}

// close removes the shim directory. Called once per execution; children
// of a killed subprocess that outlive the guard simply fail to resolve
// the shimmed names, which is the desired end state anyway.
func (g *Guard) Close() {
	if g != nil && g.dir != "" {
		os.RemoveAll(g.dir)
	}
}

func resolveRealBin(name string) string {
	// Search PATH for the real binary, skipping any orchicon-guard shim
	// directories that may be present on PATH when this process itself is
	// running inside a guarded environment (e.g. a worker worktree or
	// `go test` invoked through a guarded shell). exec.LookPath would
	// otherwise resolve to the shim itself, causing the new shim to exec
	// another shim (or itself) instead of the real binary.
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if strings.Contains(dir, "orchicon-guard") {
			continue
		}
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			// Ensure it is executable.
			if fi.Mode()&0o111 != 0 {
				return candidate
			}
			// Even if not executable by mode, return it — the shim will
			// fail with a clear exec error rather than looping through the
			// guard. This mirrors exec.LookPath's behaviour for non-exec
			// files.
			return candidate
		}
	}
	// Fallback: search without guard dirs on PATH via a cleaned env.
	// Build a PATH without guard entries and try LookPath with it.
	var cleaned []string
	for _, dir := range filepath.SplitList(pathEnv) {
		if strings.Contains(dir, "orchicon-guard") {
			continue
		}
		cleaned = append(cleaned, dir)
	}
	if len(cleaned) > 0 {
		origPath := os.Getenv("PATH")
		os.Setenv("PATH", strings.Join(cleaned, string(os.PathListSeparator)))
		p, err := exec.LookPath(name)
		os.Setenv("PATH", origPath)
		if err == nil {
			return p
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	// If the resolved path is itself a guard shim, try common absolute
	// locations as a last resort.
	if strings.Contains(p, "orchicon-guard") {
		for _, abs := range []string{"/bin/" + name, "/usr/bin/" + name, "/usr/local/bin/" + name} {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
		return ""
	}
	return p
}

// guardScriptTemplate is the bash shim generated per execution. It
// dispatches on argv[0]'s basename (the symlink name) to the policy for
// that binary, then either refuses the command or execs the real binary.
var guardScriptTemplate = `#!/bin/bash
# Orchicon execution safety guard — generated per worker execution.
# Blocks destructive commands and any file operation targeting a path
# outside the worker's project directory. Injected on PATH for every
# worker execution; see internal/opencode/guard.go.
PROJECT_DIR='{{.ProjectDir}}'
SCRATCH_DIR='{{.ScratchDir}}'
POLICY_FILE='{{.PolicyFile}}'

# --- interactive profile ---------------------------------------------------
# ORCHICON_GUARD_POLICY switches on the interactive (Ask) profile; only the Ask
# path (internal/askorchicon) sets it. In that profile the shim is GRANT-AWARE
# (it honours the conversation's project, its session grants, the
# operator-approved once-targets and the policy accept list) and FAIL-CLOSED:
# a policy it cannot read or parse REFUSES the path-scoped command. A guard that
# silently stops guarding is the exact failure this component was built after,
# so the safe default is refusal. The worker path sets none of these and keeps
# its historical behaviour byte-for-byte.
INTERACTIVE=""
if [ -n "${ORCHICON_GUARD_POLICY:-}" ]; then
  INTERACTIVE=1
  POLICY_FILE="$ORCHICON_GUARD_POLICY"
fi
GUARD_PROJECT="${ORCHICON_GUARD_PROJECT:-}"
GUARD_GRANTS="${ORCHICON_GUARD_GRANTS:-}"
GUARD_ONCE="${ORCHICON_GUARD_ONCE:-}"
# extglob: the shim spells "* but not across a slash" as '*([!/])' in
# policy_entry_match, so a policy glob cannot match a path permpolicy.Decide
# would not. It must be enabled BEFORE any function below is defined, because a
# function body is parsed when its definition executes.
shopt -s extglob
TAB=$(printf '\t')

# failed_closed refuses the path-scoped command because the policy could not be
# read. Only reachable in the interactive profile — the worker profile keeps the
# "absent policy = no policy" rule.
failed_closed() {
  echo "ORCHICON GUARD (fail-closed): the permission policy '$1' cannot be read ($2) — refusing the path-scoped command '${0##*/}' rather than running it unguarded." >&2
  exit 1
}

blocked() {
  echo "ORCHICON GUARD: command '${0##*/}' is PERMANENTLY BLOCKED — destructive or privileged tooling in the never-allow class (sudo / dd / mkfs* / fdisk / parted / shred / wipefs / LVM / mkswap). That class can never be approved: not by a session grant, not by the operator, not by a policy accept entry. There is no prompt to answer." >&2
  exit 1
}

# interactive_blocked refuses an absolute target the interactive profile does not
# cover, NAMING the path and the reason, so the model can course-correct from the
# tool's RESULT.
interactive_blocked() {
  echo "ORCHICON GUARD: command '${0##*/}' blocked — '$1' is outside the conversation's project (${GUARD_PROJECT:-none}), the granted directories (${GUARD_GRANTS:-none}) and the accept list; it is not an approved once-target either. Grant its directory in the chat, or add it to the permission policy ($POLICY_FILE)." >&2
  exit 1
}

# inside_dir returns 0 when $2 equals $1 or sits below it. An empty $1 is never a
# match (the empty-project rule in blocked_path relies on that).
inside_dir() {
  [ -n "$1" ] || return 1
  case "$2" in
    "$1"|"$1"/*) return 0 ;;
  esac
  return 1
}

# inside_list returns 0 when $2 is inside any newline-separated root in $1.
inside_list() {
  local root
  [ -n "$1" ] || return 1
  while IFS= read -r root; do
    [ -n "$root" ] || continue
    inside_dir "$root" "$2" && return 0
  done < <(printf '%s\n' "$1")
  return 1
}

# policy_blocked names the DENIED POLICY ENTRY, not just the rejection: a
# refusal the operator (or the model) cannot trace back to a rule is
# unfixable.
policy_blocked() {
  echo "ORCHICON GUARD: command '${0##*/}' blocked — '$DENIED_TARGET' is denied by entry '$DENIED_ENTRY' in the permission policy ($POLICY_FILE). A session grant cannot override it." >&2
  exit 1
}

# policy_entry_match ENTRY TARGET — permpolicy.matchPattern's semantics, in
# bash. It is the ONE place the shim's pattern matching is defined, and it is
# written out because bash's own '*' CROSSES '/', while doublestar's does not: a
# plain 'case "$target" in $entry' would let the shim deny — and, worse, ACCEPT
# — a path permpolicy.Decide never matches, and the shim disagreeing with the
# accessor it is supposed to read is exactly the drift this profile exists to
# prevent.
#
#   '~'    a leading tilde is expanded (the operator writes ~/.ssh/**, not the
#          absolute home)
#   '*'    one path segment — it does not cross '/'
#   '?'    one character, never '/'
#   '**'   anything, crossing '/' freely AND able to match ZERO segments
#          ('a/**/x' covers 'a/x'), which is why the pattern is also retried
#          with each '/**/' run collapsed
#   '/**'  at the end also covers the directory itself ('~/.ssh/**' covers
#          '~/.ssh')
policy_entry_match() {
  local entry="$1" target="$2" pat translated
  case "$entry" in
    '~') pat="$HOME" ;;
    '~/'*) pat="$HOME/${entry#\~/}" ;;
    *) pat="$entry" ;;
  esac
  case "$pat" in
    */'**') [ "${pat%/\*\*}" = "$target" ] && return 0 ;;
  esac
  while :; do
    translated="${pat//'**'/$(printf '\1')}"
    translated="${translated//'*'/'*([!/])'}"
    translated="${translated//'?'/'[!/]'}"
    translated="${translated//$(printf '\1')/'*'}"
    case "$target" in
      $translated) return 0 ;;
    esac
    case "$pat" in
      *'**/'*)
        # '**' matches zero path segments too: retry with one run collapsed.
        pat="${pat%%'**/'*}${pat#*'**/'}"
        ;;
      *) return 1 ;;
    esac
  done
}

# policy_lookup STRICTLY parses the policy file and records what it says about
# ONE target: POLICY_VERDICT is "deny", "accept" or "" (nothing matched), and
# POLICY_ENTRY is the entry's literal text — reported EXACTLY AS THE OPERATOR
# WROTE IT (a leading ~ stays a ~), because the operator's next move is to grep
# their policy file for the name the refusal gave.
#
# PRECEDENCE: the ACCESSOR's, not a first-match-in-file-order read. The whole
# file is parsed — both halves — and then the DENY list is consulted IN FULL
# before any accept entry, whatever order the two sections appear in. permpolicy
# .Decide evaluates its deny list before either half is read; a first-match read
# would let an accept section written above the deny section approve a path the
# operator denied, which is the shim silently disagreeing with the accessor.
#
# FAIL CLOSED (interactive profile only). A MISSING, unreadable or MALFORMED
# policy REFUSES via failed_closed: a policy file that silently parses to
# nothing is worse than no file at all, because the operator believes their
# exclusions are in force. The worker profile (no ORCHICON_GUARD_POLICY) keeps
# the historical lenient read, where an absent file means "no policy".
#
# The file is read PER INVOCATION on purpose: the policy is a few hundred bytes,
# a path-scoped command is rare, and a fresh read means a hand-edit or a UI
# change takes effect on the very next command — no watcher, no cache, no
# staleness window.
policy_lookup() {
  local target="$1" line section entry pat i
  POLICY_VERDICT=""
  POLICY_ENTRY=""
  [ -n "$POLICY_FILE" ] || return 0
  if [ ! -f "$POLICY_FILE" ] || [ ! -r "$POLICY_FILE" ]; then
    if [ -n "$INTERACTIVE" ]; then
      failed_closed "$POLICY_FILE" "missing, unreadable, or not a regular file"
    fi
    return 0
  fi
  local -a deny_entries=() accept_entries=()
  section=""
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      *"$TAB"*)
        # YAML forbids a tab in indentation; a tab means these are not the
        # bytes permpolicy writes, so the file cannot be vouched for.
        if [ -n "$INTERACTIVE" ]; then failed_closed "$POLICY_FILE" "malformed (tab in indentation)"; fi
        return 0
        ;;
    esac
    line="${line%"${line##*[![:space:]]}"}"
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in
      ""|'#'*) continue ;;
      "deny:") section="deny"; continue ;;
      "accept:") section="accept"; continue ;;
      "deny: []"|"accept: []") section=""; continue ;;
      "deny:"*|"accept:"*)
        if [ -n "$INTERACTIVE" ]; then failed_closed "$POLICY_FILE" "malformed (bad list header)"; fi
        return 0
        ;;
      "- "*)
        if [ -z "$section" ]; then
          if [ -n "$INTERACTIVE" ]; then failed_closed "$POLICY_FILE" "malformed (entry outside a list)"; fi
          continue
        fi
        pat="${line#- }"
        pat="${pat%%#*}"
        pat="${pat%"${pat##*[![:space:]]}"}"
        pat="${pat%\"}"
        pat="${pat#\"}"
        pat="${pat%\'}"
        pat="${pat#\'}"
        [ -n "$pat" ] || continue
        entry="$pat"
        if [ "$section" = "deny" ]; then
          deny_entries+=("$entry")
        else
          accept_entries+=("$entry")
        fi
        ;;
      *)
        if [ -n "$INTERACTIVE" ]; then failed_closed "$POLICY_FILE" "malformed (unrecognised line)"; fi
        continue
        ;;
    esac
  done < "$POLICY_FILE"

  # The accessor's precedence: the deny list in full, then the accept list.
  for ((i = 0; i < ${#deny_entries[@]}; i++)); do
    if policy_entry_match "${deny_entries[i]}" "$target"; then
      POLICY_VERDICT="deny"
      POLICY_ENTRY="${deny_entries[i]}"
      return 0
    fi
  done
  for ((i = 0; i < ${#accept_entries[@]}; i++)); do
    if policy_entry_match "${accept_entries[i]}" "$target"; then
      POLICY_VERDICT="accept"
      POLICY_ENTRY="${accept_entries[i]}"
      return 0
    fi
  done
  return 0
}

# denied_target returns 0 (deny) when any argument matches an entry in the
# operator's DURABLE permission policy deny list. The deny list sits ABOVE the
# session grant and above the project scope: it is the operator's exclusion, and
# a grant suppresses prompts but cannot open a path the operator excluded.
#
# This sits BELOW the never-allow binary class (that case block above is
# absolute and cannot be reached past).
denied_target() {
  local a
  for a in "$@"; do
    case "$a" in
      -*) continue ;;
      --) continue ;;
    esac
    policy_lookup "$a"
    if [ "$POLICY_VERDICT" = "deny" ]; then
      DENIED_TARGET="$a"
      DENIED_ENTRY="$POLICY_ENTRY"
      return 0
    fi
  done
  return 1
}

# blocked_path returns 0 (block) when a path argument escapes the sanctioned
# set. The precedence, per non-flag argument — the consent layer's order, minus
# the ask rung a shim cannot have:
#   1. a DENY entry  (above a session grant, on purpose)
#   2. inside PROJECT_DIR (baked at build time; worker AND interactive)
#   3. inside SCRATCH_DIR (Orchicon-owned ephemeral scratch)
#   4. interactive only: inside the conversation's project, a session-granted
#      directory, an operator-approved once-target, or an ACCEPT entry
#   5. anything else is refused (relative / ~ / $HOME / .. keep the historical
#      block rule)
blocked_path() {
  denied_target "$@" && policy_blocked
  local a
  for a in "$@"; do
    case "$a" in
      -*) continue ;;
      --) continue ;;
    esac
    case "$a" in
      /*)
        inside_dir "$PROJECT_DIR" "$a" && continue
        # Scratch carve-out: Orchicon-owned scratch is writable even though
        # it lives outside the project (the worker is told to use it).
        inside_dir "$SCRATCH_DIR" "$a" && continue
        if [ -n "$INTERACTIVE" ]; then
          inside_dir "$GUARD_PROJECT" "$a" && continue
          inside_list "$GUARD_GRANTS" "$a" && continue
          inside_list "$GUARD_ONCE" "$a" && continue
          policy_lookup "$a"
          [ "$POLICY_VERDICT" = "accept" ] && continue
          interactive_blocked "$a"
        fi
        # Empty PROJECT_DIR = the shared host-serve mode: no single project
        # root, so every absolute target is outside scope and blocked (closes
        # the rm / leak that an empty dir would otherwise allow through the
        # "$PROJECT_DIR"/* glob).
        return 0
        ;;
      '~'|'~'/*|'$HOME'|'$HOME'/*|'${HOME}'|'${HOME}'/*|*".."*)
        if [ -n "$INTERACTIVE" ]; then
          interactive_blocked "$a"
        fi
        return 0
        ;;
    esac
  done
  return 1
}

case "${0##*/}" in
  {{.NeverAllowCase}})
    blocked
    ;;
  # blocked_path applies the deny check FIRST, then the project/scratch/grant/
  # accept/once allow-set, then the refusal — see its doc comment.
  rm)    blocked_path "$@" && blocked; exec '{{index .Real "rm"}}' "$@" ;;
  chmod) blocked_path "$@" && blocked; exec '{{index .Real "chmod"}}' "$@" ;;
  chown) blocked_path "$@" && blocked; exec '{{index .Real "chown"}}' "$@" ;;
  mv)    blocked_path "$@" && blocked; exec '{{index .Real "mv"}}' "$@" ;;
  cp)    blocked_path "$@" && blocked; exec '{{index .Real "cp"}}' "$@" ;;
  ln)    blocked_path "$@" && blocked; exec '{{index .Real "ln"}}' "$@" ;;
  *)
    echo "ORCHICON GUARD: unexpected invocation '${0##*/}'." >&2
    exit 1
    ;;
esac
`
