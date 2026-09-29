package main

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/claude"
)

// runClaudeHook is the `orchicon claude-hook` subcommand: the PreToolUse hook a
// claude worker session invokes for every command- or path-carrying tool call.
//
// It is a thin wrapper (same style as runMCP): the whole rule set lives in
// internal/claude so it can be table-tested with no live session, and this file
// only binds it to the process's stdin/stdout. The subcommand takes NO
// arguments — the project boundary arrives through the child's environment
// (ORCHICON_CLAUDE_PROJECT_DIR), which the session sets, so a hostile tool input
// cannot re-point it.
func runClaudeHook(in io.Reader, out io.Writer) int {
	return claude.RunHook(in, out, os.Getenv)
}
