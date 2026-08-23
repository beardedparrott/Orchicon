package guard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
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

// ScratchDir is the one writable scratch area outside the project directory
// (kept in sync with opencode.ScratchDir). Scoped binaries (rm/mv/cp/…)
// are allowed to operate inside it — it is Orchicon-owned ephemeral
// scratch (screenshots, logs, downloads), so guarding it like a foreign path
// burns worker tokens on "blocked" retries for no safety gain. Everything
// else outside the project stays blocked.
const ScratchDir = "/tmp/orchicon"

// guardedBinary names one binary the guard shims on PATH.
type guardedBinary struct {
	name   string
	scoped bool // true: allow when targets stay in the project; false: always block
}

var guardedBinaries = []guardedBinary{
	// Always-block: destructive / system-modifying, never needed in-project.
	{name: "sudo"},
	{name: "dd"},
	{name: "mkfs"},
	{name: "mkfs.ext2"},
	{name: "mkfs.ext3"},
	{name: "mkfs.ext4"},
	{name: "mkfs.xfs"},
	{name: "mkfs.btrfs"},
	{name: "mkfs.fat"},
	{name: "mkfs.vfat"},
	{name: "mkswap"},
	{name: "fdisk"},
	{name: "parted"},
	{name: "shred"},
	{name: "wipefs"},
	{name: "pvcreate"},
	{name: "pvremove"},
	{name: "vgcreate"},
	{name: "vgremove"},
	{name: "lvcreate"},
	{name: "lvremove"},
	// Path-scoped: fine when all targets stay inside the project dir.
	{name: "rm", scoped: true},
	{name: "chmod", scoped: true},
	{name: "chown", scoped: true},
	{name: "mv", scoped: true},
	{name: "cp", scoped: true},
	{name: "ln", scoped: true},
}

// Guard is a generated shim directory prepended to a worker's
// PATH. `dir` holds one `guard` script plus symlinks named after each
// guarded binary. `real` maps scoped binary names to their resolved
// absolute paths on the host (the shim execs the real binary when it
// decides the command is safe).
type Guard struct {
	dir  string
	real map[string]string
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
	g, err := buildGuardIn(dir, projectDir)
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
	dir, err := os.MkdirTemp("", "orchicon-guard-*")
	if err != nil {
		return nil, err
	}
	g, err := buildGuardIn(dir, projectDir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return g, nil
}

// buildGuardIn renders the guard script + symlinks into dir (created by
// the caller). projectDir is the worker's working directory.
func buildGuardIn(dir, projectDir string) (*Guard, error) {
	if projectDir != "" {
		if abs, err := filepath.Abs(projectDir); err == nil {
			projectDir = abs
		}
	}

	g := &Guard{dir: dir, real: make(map[string]string)}

	// Resolve the real binary paths for scoped binaries. They are always
	// present on a working Linux/macOS host; a missing one means the
	// binary doesn't exist and nothing needs shimming.
	for _, b := range guardedBinaries {
		if b.scoped {
			g.real[b.name] = resolveRealBin(b.name)
		}
	}

	data := struct {
		ProjectDir string
		Real       map[string]string
		ScratchDir string
	}{projectDir, g.real, ScratchDir}

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
	for _, b := range guardedBinaries {
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
	p, err := exec.LookPath(name)
	if err != nil {
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

blocked() {
  echo "ORCHICON GUARD: command '${0##*/}' blocked (destructive, or targets a path outside the project directory)." >&2
  exit 1
}

# blocked_path returns 0 (block) if any path argument escapes PROJECT_DIR.
# A target under SCRATCH_DIR (Orchicon-owned ephemeral scratch) is allowed.
blocked_path() {
  local a
  for a in "$@"; do
    case "$a" in
      -*) continue ;;
      --) continue ;;
    esac
    case "$a" in
      /*)
        if [ -n "$PROJECT_DIR" ]; then
          case "$a" in
            "$PROJECT_DIR"|"$PROJECT_DIR"/*) continue ;;
          esac
        else
          # Empty PROJECT_DIR = the shared host-serve mode: no single
          # project root, so every absolute target is outside scope and
          # blocked (closes the rm / leak that an empty dir would otherwise
          # allow through the "$PROJECT_DIR"/* glob).
          return 0
        fi
        # Scratch carve-out: Orchicon-owned scratch is writable even though
        # it lives outside the project (the worker is told to use it).
        if [ -n "$SCRATCH_DIR" ]; then
          case "$a" in
            "$SCRATCH_DIR"|"$SCRATCH_DIR"/*) continue ;;
          esac
        fi
        return 0
        ;;
      '~'|'~'/*|'$HOME'|'$HOME'/*|'${HOME}'|'${HOME}'/*|*".."*)
        return 0
        ;;
    esac
  done
  return 1
}

case "${0##*/}" in
  sudo|dd|mkfs|mkfs.*|mkswap|fdisk|parted|shred|wipefs|pvcreate|pvremove|vgcreate|vgremove|lvcreate|lvremove)
    blocked
    ;;
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
