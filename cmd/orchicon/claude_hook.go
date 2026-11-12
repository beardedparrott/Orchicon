package main

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/claude"
)

// runClaudeHook is the `orchicon claude-hook` subcommand: the PreToolUse hook a claude worker
// session invokes for every command- or path-carrying tool call.
func runClaudeHook(in io.Reader, out io.Writer) int {
	return claude.RunHook(in, out, os.Getenv)
}
