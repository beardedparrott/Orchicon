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
	"os"
	"path/filepath"
	"strings"
)

// ClaudeBinEnv overrides the resolved claude binary. It is the escape hatch for
// an install in an unusual place, mirroring ORCHICON_OPENCODE_BIN.
const ClaudeBinEnv = "ORCHICON_CLAUDE_BIN"

// claudeDefaultRelativePath is where the CLI's own installer puts itself, and
// therefore where an operator's authenticated install lives.
var claudeDefaultRelativePath = []string{".local", "bin", "claude"}

// ClaudeBinaryPath resolves the claude executable to spawn.
//
// ORDER, and each step is load-bearing:
//
//  1. ORCHICON_CLAUDE_BIN — an explicit operator/deployment choice wins.
//  2. $HOME/.local/bin/claude — the operator's OWN install, which is the one
//     they signed into and the one whose version matches the catalog. $HOME is
//     whatever the plane set (the runtime daemon passes HOME=$HostHome into a
//     container, where the same host path is bind-mounted), so this resolves to
//     the same install on both transports.
//  3. A bare "claude" — the last resort, so a machine with the CLI only on PATH
//     still works exactly as before. It is a fallback and not the primary
//     because it is the case that silently picks the wrong installation.
//
// A candidate must EXIST and be a regular file; anything else falls through
// rather than spawning a path that cannot run.
func ClaudeBinaryPath() string {
	if v := strings.TrimSpace(os.Getenv(ClaudeBinEnv)); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		cand := filepath.Join(append([]string{home}, claudeDefaultRelativePath...)...)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return "claude"
}
