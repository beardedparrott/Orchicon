package claude

// binary.go — resolving the claude CLI to spawn.
//
// WHY THIS EXISTS, and it is not tidiness. The adapter used to spawn the BARE
// name "claude" and let PATH decide, on the assumption that there is one claude
// on the machine. On this operator's host there are THREE:
//
//	/home/<user>/.local/bin/claude   -> ~/.local/share/claude/versions/2.1.284
//	/usr/bin/claude                  -> a root-owned shell wrapper
//	/opt/claude-code/bin/claude      -> 2.1.241 (root-owned, an older release)
//
// The wrapper execs the /opt install, so a PATH that reaches /usr/bin before the
// operator's own ~/.local/bin runs a CLI a year older than the one they
// authenticated. That is not a downgrade in degrees: 2.1.241 REJECTS
// `--permission-prompts` — the flag the Ask profile depends on — with
// "unknown option", exits 1, and writes NOTHING to stdout. The turn then has no
// events at all, so the operator sees a permanent "thinking…" with no error and
// nothing in the log.
//
// The operator's own install is also the only one that carries their login, so
// preferring it fixes the credential question at the same time.

import (
	"github.com/beardedparrott/orchicon/internal/adapter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ClaudeBinEnv overrides the resolved claude binary. It is the escape hatch for
// an install in an unusual place, mirroring ORCHICON_OPENCODE_BIN.
const ClaudeBinEnv = "ORCHICON_CLAUDE_BIN"

// ClaudeBinaryName is the claude CLI's name ON A CONTAINER'S PATH, and it is the value a container
// dispatch must put in argv[0].
const ClaudeBinaryName = "claude"

// ClaudeBinaryFor returns the claude executable for a transport.
func ClaudeBinaryFor(isContainer bool) string {
	if !isContainer {
		return ClaudeBinaryPath()
	}
	return claudeContainerArgv0(runtime.GOOS, claudeHostInstall())
}

// claudeContainerArgv0 is the container argv[0] decision, with the platform and host install injected so the
// non-Linux arms are reachable from a Linux gate.
func claudeContainerArgv0(hostGOOS, hostBin string) string {
	if ok, _ := adapter.PreferHostAdapterInContainer(hostGOOS, hostBin); ok {
		return hostBin
	}
	return ClaudeBinaryName
}

// claudeHostInstall is the host install a CONTAINER child can exec, or "" when there is none.
func claudeHostInstall() string {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		home = strings.TrimSpace(os.Getenv("USERPROFILE"))
	}
	if home == "" {
		return ""
	}
	return adapter.ResolveBinary(runtime.GOOS, home, adapter.KindClaude, os.Getenv)
}

// claudeDefaultRelativePath is where the CLI's own installer puts itself, and
// therefore where an operator's authenticated install lives.
var claudeDefaultRelativePath = []string{".local", "bin", "claude"}

// ClaudeBinaryPath resolves the claude executable to spawn.
func ClaudeBinaryPath() string { return claudeBinaryPathFor(runtime.GOOS, os.Getenv) }

// claudeBinaryPathFor is ClaudeBinaryPath with the platform and environment injected, matching
// opencode's resolver and for the same reason: this is now platform-SPECIFIC behaviour.
func claudeBinaryPathFor(goos string, getenv func(string) string) string {
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	if v := env(ClaudeBinEnv); v != "" {
		return v
	}
	home := env("HOME")
	if home == "" && getenv != nil {
		// Windows has no HOME; USERPROFILE is the equivalent, and os.UserHomeDir uses it internally.
		home = env("USERPROFILE")
	}
	var candidates []string
	if home != "" {
		candidates = append(candidates, filepath.Join(append([]string{home}, claudeDefaultRelativePath...)...))
	}
	candidates = append(candidates, claudeWindowsCandidates(goos, getenv)...)
	// THE AGNOSTIC ANSWER, AFTER the known shapes and BEFORE the bare-name fallback: ask the OS's PATH
	// — read from its SOURCE, not this process's snapshot.
	candidates = append(candidates, adapter.DiscoverKind(goos, home, adapter.KindClaude, getenv)...)
	for _, cand := range candidates {
		// A candidate must EXIST and be a regular file, and must never be a shell SHIM: Go cannot execute
		// a .cmd/.bat/.ps1, so resolving one moves the failure to the first dispatch.
		if strings.EqualFold(filepath.Ext(cand), ".cmd") ||
			strings.EqualFold(filepath.Ext(cand), ".bat") ||
			strings.EqualFold(filepath.Ext(cand), ".ps1") {
			continue
		}
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	// The bare name is the last resort: a machine with the CLI only on PATH still works exactly as before.
	// The hint, when a shim exists, is what makes "not found" honest — the caller reports it.
	return "claude"
}

// ClaudeMissingHint explains a "claude was not found" outcome when a SHIM is present, so the operator is
// not told to install something they already installed. Empty when there is nothing to add.
func ClaudeMissingHint() string { return claudeShimHint(runtime.GOOS, os.Getenv) }
