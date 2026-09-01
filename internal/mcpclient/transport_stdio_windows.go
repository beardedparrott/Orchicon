//go:build windows

package mcpclient

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStdioTransport is the windows fallback: PDEATHSIG is a Linux/BSD
// mechanism (unavailable on windows), so orphan prevention relies on the
// boot-time /proc sweep which does not apply on windows either; stdio MCP
// on windows is therefore best-effort (the session-end Close still kills
// the child deterministically).
func newStdioTransport(spec ServerSpec) (*mcp.CommandTransport, error) {
	argv := spec.Command
	if len(argv) == 0 {
		return nil, errUnconfiguredCommand(spec)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID="+spec.ID,
	)
	return &mcp.CommandTransport{Command: cmd, TerminateDuration: 0}, nil
}
