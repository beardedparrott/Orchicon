package runtime

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTestRegistry spins up a real supervisor socket + child registry on a
// temp unix socket, so tests can drive the full exec/re-attach flow.
func startTestRegistry(t *testing.T) (socketPath string, h *childRegistry, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h = newChildRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go h.serve(conn)
		}
	}()
	cleanup = func() {
		l.Close()
		h.killAll(syscall.SIGKILL)
	}
	return socketPath, h, cleanup
}

// dialExec connects and sends an exec request, returning the connection and
// decoder so the caller can read events and disconnect mid-stream.
func dialExec(t *testing.T, socketPath, execID string, argv []string, graceSec int64) (net.Conn, *json.Decoder) {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(AgentRequest{
		Cmd:                   "exec",
		ExecID:                execID,
		Argv:                  argv,
		ReconnectGraceSeconds: graceSec,
	}); err != nil {
		t.Fatalf("send exec: %v", err)
	}
	return conn, json.NewDecoder(conn)
}

// readUntil decodes events until pred matches; returns the matching event.
func readUntil(t *testing.T, dec *json.Decoder, pred func(AgentEvent) bool) AgentEvent {
	t.Helper()
	for {
		var ev AgentEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if pred(ev) {
			return ev
		}
	}
}

// TestExecSessionReattach verifies the core resilience: a client disconnect
// does NOT kill the child, and a re-attach for the same exec_id resumes the
// stream (instead of spawning a second child).
func TestExecSessionReattach(t *testing.T) {
	socketPath, h, cleanup := startTestRegistry(t)
	defer cleanup()

	argv := []string{"sh", "-c", "echo hello; sleep 1; echo world"}
	conn, dec := dialExec(t, socketPath, "ex1", argv, 60)
	readUntil(t, dec, func(ev AgentEvent) bool {
		return ev.Stream == "stdout" && strings.Contains(ev.Data, "hello")
	})

	// Client 1 disconnects mid-run.
	conn.Close()

	// The child must survive the disconnect (reconnect grace pending).
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	_, ok := h.cmd["ex1"]
	h.mu.Unlock()
	if !ok {
		t.Fatal("session removed after a client disconnect — the child should survive the reconnect grace")
	}

	// Re-attach with the SAME exec_id: the stream must resume, not spawn a
	// second child.
	conn2, dec2 := dialExec(t, socketPath, "ex1", argv, 60)
	defer conn2.Close()
	ev := readUntil(t, dec2, func(ev AgentEvent) bool {
		return ev.Stream == "stdout" && strings.Contains(ev.Data, "world")
	})
	if ev.Event != "" {
		t.Fatalf("unexpected event before world: %+v", ev)
	}
	exit := readUntil(t, dec2, func(ev AgentEvent) bool { return ev.Event == "exit" })
	if exit.ExitCode != 0 {
		t.Fatalf("re-attach exit code = %d, want 0", exit.ExitCode)
	}
}

// TestExecSessionGraceKillsOrphan verifies that when nothing re-attaches, the
// orphaned child is killed once the reconnect grace expires.
func TestExecSessionGraceKillsOrphan(t *testing.T) {
	socketPath, h, cleanup := startTestRegistry(t)
	defer cleanup()

	argv := []string{"sh", "-c", "echo start; sleep 30"}
	conn, dec := dialExec(t, socketPath, "ex2", argv, 1) // 1s grace
	readUntil(t, dec, func(ev AgentEvent) bool {
		return ev.Stream == "stdout" && strings.Contains(ev.Data, "start")
	})

	// Disconnect and never re-attach.
	conn.Close()

	time.Sleep(1600 * time.Millisecond)
	h.mu.Lock()
	_, ok := h.cmd["ex2"]
	h.mu.Unlock()
	if ok {
		t.Fatal("orphaned child still registered after the reconnect grace — it should have been killed")
	}
}

// TestExecSessionReattachAfterExit verifies a re-attach to an already-finished
// exec reports its final exit state instead of starting a new child.
func TestExecSessionReattachAfterExit(t *testing.T) {
	socketPath, _, cleanup := startTestRegistry(t)
	defer cleanup()

	argv := []string{"sh", "-c", "exit 3"}
	conn, dec := dialExec(t, socketPath, "ex3", argv, 60)
	exit := readUntil(t, dec, func(ev AgentEvent) bool { return ev.Event == "exit" })
	conn.Close()
	if exit.ExitCode != 3 {
		t.Fatalf("first exit code = %d, want 3", exit.ExitCode)
	}

	// Re-attach after exit — must report exit 3, not start a new child.
	conn2, dec2 := dialExec(t, socketPath, "ex3", argv, 60)
	defer conn2.Close()
	exit2 := readUntil(t, dec2, func(ev AgentEvent) bool { return ev.Event == "exit" })
	if exit2.ExitCode != 3 {
		t.Fatalf("re-attach exit code = %d, want 3", exit2.ExitCode)
	}
}
