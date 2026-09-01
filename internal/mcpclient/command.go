package mcpclient

import (
	"fmt"
	"strings"
)

// errUnconfiguredCommand is returned for a stdio spec selected without a
// configured command (selected-but-unconfigured — actionable, never
// silent).
func errUnconfiguredCommand(spec ServerSpec) error {
	return fmt.Errorf("mcp server %q: selected but no command configured", spec.ID)
}

// splitCommandLine splits a command string into argv the way a shell
// would for the simple quoting we accept (whitespace-separated, with
// double-quote groups). It exists because permissions.mcp_servers entries
// carry the command as a single string (frontend MCPPicker shape).
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' || r == '\t':
			if !inQuote {
				flush()
				continue
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
