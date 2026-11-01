//go:build !windows

package mcpclient

import (
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// newStdioTransport builds the go-sdk CommandTransport for a stdio server
// with Linux PDEATHSIG (parent-death signal): when the control-plane
// process dies, the kernel kills the child so MCP server processes can
// never outlive a dead control plane (ADR-0008). Setpgid places the child
// in its own process group so the boot-time sweep can signal the whole
// group, and so an in-flight child cannot receive terminal signals
// intended for the plane.
//
// IT ALSO CAPTURES STDERR and stamps the OWNING PLANE's identity, both of which exist because of the same
// reported incident (see stdio_spawn.go and sweep_unix.go).
func newStdioTransport(spec ServerSpec) (*stdioSpawn, error) {
	argv := spec.Command
	if len(argv) == 0 {
		return nil, errUnconfiguredCommand(spec)
	}
	cmd := procwin.Command(argv[0], argv[1:]...)
	// The process attributes are PER-OS: PDEATHSIG is Linux-only and Setpgid is portable, so the
	// two live in their own files and this one just asks for the platform's answer. Writing the
	// literal inline here is what broke the darwin release builds.
	cmd.SysProcAttr = stdioSysProcAttr()
	// Environment markers let the boot-time sweep identify MCP children
	// (see sweep.go) and give the child context about its owning server. The PLANE identity markers
	// are what let the sweep reap a child whose plane is gone even after the kernel has reparented it
	// to some unrelated live process — see sweep_unix.go for why ppid liveness cannot answer that.
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID="+spec.ID,
	)
	cmd.Env = append(cmd.Env, planeIdentityEnv()...)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// THE CHILD'S OWN DIAGNOSIS. The go-sdk sets only stdin/stdout on this command, so without this a
	// Python server's ImportError went to /dev/null and every failure looked identical.
	stderr := newProcStderr()
	cmd.Stderr = stderr
	return &stdioSpawn{
		cmd:       cmd,
		stderr:    stderr,
		transport: &mcp.CommandTransport{Command: cmd, TerminateDuration: 0},
	}, nil
}
