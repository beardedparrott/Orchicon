//go:build windows

package mcpclient

import (
	"os"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// newStdioTransport is the windows fallback: PDEATHSIG is a Linux/BSD
// mechanism (unavailable on windows), so orphan prevention relies on the
// boot-time /proc sweep which does not apply on windows either; stdio MCP
// on windows is therefore best-effort (the session-end Close still kills
// the child deterministically).
func newStdioTransport(spec ServerSpec) (*stdioSpawn, error) {
	argv := spec.Command
	if len(argv) == 0 {
		return nil, errUnconfiguredCommand(spec)
	}
	cmd := procwin.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID="+spec.ID,
	)
	cmd.Env = append(cmd.Env, planeIdentityEnv()...)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// CAPTURED FOR THE SAME REASON AS THE UNIX PATH (stdio_spawn.go): the go-sdk sets only stdin/stdout,
	// so a child that dies on import used to write its traceback to nowhere.
	stderr := newProcStderr()
	cmd.Stderr = stderr
	return &stdioSpawn{
		cmd:       cmd,
		stderr:    stderr,
		transport: &mcp.CommandTransport{Command: cmd, TerminateDuration: 0},
	}, nil
}
