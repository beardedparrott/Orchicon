//go:build !windows

package mcpclient

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStdioTransport builds the go-sdk CommandTransport for a stdio server
// with Linux PDEATHSIG (parent-death signal): when the control-plane
// process dies, the kernel kills the child so MCP server processes can
// never outlive a dead control plane (ADR-0008). Setpgid places the child
// in its own process group so the boot-time sweep can signal the whole
// group, and so an in-flight child cannot receive terminal signals
// intended for the plane.
func newStdioTransport(spec ServerSpec) (*mcp.CommandTransport, error) {
	argv := spec.Command
	if len(argv) == 0 {
		return nil, errUnconfiguredCommand(spec)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
	// Environment markers let the boot-time sweep identify MCP children
	// (see sweep.go) and give the child context about its owning server.
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID="+spec.ID,
	)
	return &mcp.CommandTransport{Command: cmd, TerminateDuration: 0}, nil
}
