package opencode

import (
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed semgrep_orchicon.yml
var lintRuleset string

//go:embed semgrepignore
var lintIgnore string

// writeSafetyLint drops the Semgrep ruleset + ignore file into the
// project so review and QA workers can run the safety lint from their
// project-scoped bash/PowerShell tool. The ruleset lives in .orchicon/
// (alongside run artifacts); the .semgrepignore goes in the project root
// where semgrep expects it. Both files are plain text — fully portable
// across Linux, macOS, and Windows.
//
// Workers invoke semgrep directly (no shell script — semgrep is a
// cross-platform Python CLI):
//
//	semgrep scan --config .orchicon/semgrep_orchicon.yml --error .
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
	if err := os.WriteFile(filepath.Join(orchDir, "semgrep_orchicon.yml"), []byte(lintRuleset), 0o755); err != nil {
		slog.Default().Warn("opencode: write safety lint ruleset", "dir", orchDir, "error", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".semgrepignore"), []byte(lintIgnore), 0o644); err != nil {
		slog.Default().Warn("opencode: write safety lint ignore", "dir", projectDir, "error", err)
	}
}
