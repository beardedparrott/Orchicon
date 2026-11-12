package mcpclient

// stdio_spawn.go — WHAT HAPPENED TO THE CHILD, in words the operator can act on.
//
// THE REPORTED FAILURE this exists for: a conversation resolving two MCP servers got NEITHER, and every
// layer reported the same sentence — "connect timed out after 15s (stdio transport): context deadline
// exceeded". Between handing the command to the transport and the handshake completing, nothing recorded
// the child's pid, whether it was ever exec'd, whether it exited (and with what status), or what it wrote
// to stderr. So "the interpreter was not found", "the interpreter started and the import blew up", and "the
// server started and never answered the handshake" were indistinguishable — and a server that answers
// `initialize` in 0.29s when spawned by hand appeared simply to be a server that does not work.
//
// THREE FACTS, CHEAP TO RECORD, THAT TELL THEM APART:
//
//	pid        — set iff exec succeeded, so "no pid" IS the "never started" answer
//	state      — ProcessState is set iff the child was reaped, so an exit code distinguishes "died" from
//	             "still running"
//	stderr     — a child's last words are the diagnosis: an ImportError, a missing env var, a traceback
//
// stderr is the part that actually ends these investigations, and it was going to /dev/null: the go-sdk's
// CommandTransport only sets stdin/stdout, so it never touched cmd.Stderr.

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stderrTailLines is how many of the child's last stderr lines are kept. A server that dies on import
// prints a traceback; the TAIL carries the actual error, the head carries the boilerplate.
const stderrTailLines = 20

// procStderr is a bounded, concurrent-safe capture of a child's stderr.
type procStderr struct {
	mu    sync.Mutex
	lines []string
}

func newProcStderr() *procStderr { return &procStderr{} }

// Write implements io.Writer so it can be assigned to cmd.Stderr. It is deliberately line-oriented and
// never returns an error: a write failure here must not turn into a child-process failure.
func (p *procStderr) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Split on newlines, keeping partial-line fragments as their own entry — a child killed mid-line is
	// exactly the case where that fragment is the diagnosis.
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.lines = append(p.lines, line)
		if len(p.lines) > stderrTailLines {
			p.lines = p.lines[len(p.lines)-stderrTailLines:]
		}
	}
	return len(b), nil
}

// Tail returns the captured lines joined for a log/error line, or "" when the child wrote nothing.
func (p *procStderr) Tail() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lines) == 0 {
		return ""
	}
	return strings.Join(p.lines, " | ")
}

// stdioSpawn is one prepared stdio child: the transport the SDK will drive, the command behind it (so a
// failure can be described), and the stderr capture.
type stdioSpawn struct {
	cmd       *exec.Cmd
	stderr    *procStderr
	transport *mcp.CommandTransport
}

// stdioProcessState describes what happened to a spawned child, for a failure message.
func stdioProcessState(cmd *exec.Cmd) string {
	switch {
	case cmd.Process == nil:
		return "the child never started (exec failed before a process existed)"
	case cmd.ProcessState != nil:
		return fmt.Sprintf("the child EXITED with %s before the handshake completed", cmd.ProcessState.String())
	default:
		return fmt.Sprintf("the child is STILL RUNNING (pid %d) — it started but did not complete the handshake",
			cmd.Process.Pid)
	}
}

// killStopsTheChild terminates a spawned child that will never be used.
func killStopsTheChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil {
		return // already reaped; nothing to kill
	}
	killProcessTree(cmd)
}

// diagnosis renders the one line a failed start has to explain itself with.
func (s *stdioSpawn) diagnosis() string {
	if s == nil || s.cmd == nil {
		return ""
	}
	parts := []string{stdioProcessState(s.cmd)}
	if tail := s.stderr.Tail(); tail != "" {
		parts = append(parts, "stderr: "+tail)
	} else {
		parts = append(parts, "the child wrote nothing to stderr")
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// truncateForError keeps a very chatty child's stderr from producing a megabyte-long error string.
func truncateForError(s string) string {
	const max = 1200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ensure io is referenced for the io.Writer contract documented above.
var _ io.Writer = (*procStderr)(nil)
