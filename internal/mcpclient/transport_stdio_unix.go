//go:build !windows

package mcpclient

import (
	"os"
	"os/exec"

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
	// The process attributes are PER-OS: PDEATHSIG is Linux-only and Setpgid is portable, so the
	// two live in their own files and this one just asks for the platform's answer. Writing the
	// literal inline here is what broke the darwin release builds.
	cmd.SysProcAttr = stdioSysProcAttr()
	// Environment markers let the boot-time sweep identify MCP children
	// (see sweep.go) and give the child context about its owning server.
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID="+spec.ID,
	)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return &mcp.CommandTransport{Command: cmd, TerminateDuration: 0}, nil
}
