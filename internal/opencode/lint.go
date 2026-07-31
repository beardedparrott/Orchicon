package opencode

import (
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed lint_safety.sh
var lintScript string

//go:embed semgrep_orchicon.yml
var lintRuleset string

// writeSafetyLint drops the safety lint wrapper + ruleset into the
// project's .orchicon/ directory so review and QA workers can run them
// from their project-scoped bash tool (their tool scope is confined to
// the project directory by the external_directory deny rule, so the
// files must live there).
//
// Best-effort by design: a failure only means the lint is unavailable to
// that execution — it never blocks dispatch, and the permission deny
// rules + execution guard still protect the host.
func writeSafetyLint(projectDir string) {
	if projectDir == "" {
		return
	}
	if st, err := os.Stat(projectDir); err != nil || !st.IsDir() {
		return
	}
	orchDir := filepath.Join(projectDir, ".orchicon")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		slog.Default().Warn("opencode: write safety lint: mkdir", "dir", orchDir, "error", err)
		return
	}
	if err := os.WriteFile(filepath.Join(orchDir, "lint-safety.sh"), []byte(lintScript), 0o755); err != nil {
		slog.Default().Warn("opencode: write safety lint", "path", orchDir, "error", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "semgrep_orchicon.yml"), []byte(lintRuleset), 0o755); err != nil {
		slog.Default().Warn("opencode: write safety lint ruleset", "path", orchDir, "error", err)
	}
}
