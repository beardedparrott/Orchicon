package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortSocketPath returns a unix socket path short enough to bind.
//
// A unix socket path is capped by the kernel at 108 bytes INCLUDING the
// terminating NUL (sun_path), and exceeding it fails with a bare
// "bind: invalid argument" (EINVAL) that names neither the length nor the
// limit. t.TempDir() is unbounded: TMPDIR + the test name + a random suffix
// (+ an ordinal for each further call in the same test), and `go test`
// inherits GOTMPDIR — which this repo's Makefile points at
// <repo>/.dev/tools/gotmp. On a deep checkout the tests below therefore
// overflow the cap and fail to bind on the operator's machine while passing
// in a container with a short path. /tmp keeps this at ~30 bytes, matching
// the choice already made in daemon_secrets_test.go.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ocs-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	if len(sock) >= 108 {
		t.Fatalf("socket path is %d bytes, over the 107-byte sun_path limit: %s", len(sock), sock)
	}
	return sock
}

// TestRunClientStdioForwardsFollowUpFrames pins the DUPLEX contract of the
// in-container `runtime-client` shim: a "stdio" request is long-lived, so
// every follow-up frame the plane writes on the same stdin (a user turn, a
// signal, "close") must reach the supervisor on the SAME connection. Without
// the forwarding goroutine only the opening frame arrives and the claude
// session silently degrades to a one-shot (container transport).
func TestRunClientStdioForwardsFollowUpFrames(t *testing.T) {
	sock := shortSocketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	type result struct {
		frames []AgentRequest
		err    error
	}
	got := make(chan result, 1)
	// Fake supervisor: read frames off the connection until it has seen the
	// opening frame plus the two follow-ups, then answer with the terminal
	// exit event so RunClient returns.
	go func() {
		conn, aerr := l.Accept()
		if aerr != nil {
			got <- result{err: aerr}
			return
		}
		defer conn.Close()
		dec := json.NewDecoder(conn)
		var frames []AgentRequest
		for len(frames) < 3 {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var f AgentRequest
			if derr := dec.Decode(&f); derr != nil {
				got <- result{frames: frames, err: derr}
				return
			}
			frames = append(frames, f)
		}
		_ = json.NewEncoder(conn).Encode(AgentEvent{Event: "exit", ExitCode: 0})
		got <- result{frames: frames}
	}()

	var in bytes.Buffer
	in.WriteString(`{"cmd":"stdio","exec_id":"exec-1","argv":["claude","-p"]}` + "\n")
	in.WriteString(`{"cmd":"stdin","exec_id":"exec-1","data":"{\"type\":\"user\"}"}` + "\n")
	in.WriteString(`{"cmd":"signal","exec_id":"exec-1","signal":"INT"}` + "\n")

	code, err := RunClient(sock, &in, io.Discard)
	if err != nil {
		t.Fatalf("RunClient: %v", err)
	}
	if code != 0 {
		t.Fatalf("RunClient exit code = %d, want 0", code)
	}
	res := <-got
	if res.err != nil {
		t.Fatalf("supervisor never saw the follow-up frames (got %d of 3): %v", len(res.frames), res.err)
	}
	if len(res.frames) != 3 {
		t.Fatalf("supervisor got %d frames, want 3 (the opening request + 2 follow-ups)", len(res.frames))
	}
	if res.frames[0].Cmd != "stdio" || len(res.frames[0].Argv) == 0 {
		t.Fatalf("frame 0 = %+v, want the stdio request", res.frames[0])
	}
	if res.frames[1].Cmd != "stdin" || res.frames[1].Data == "" {
		t.Fatalf("frame 1 = %+v, want the injected stdin turn", res.frames[1])
	}
	if res.frames[2].Cmd != "signal" || res.frames[2].Signal != "INT" {
		t.Fatalf("frame 2 = %+v, want the INT signal", res.frames[2])
	}
}

// TestRunClientStdioEOFClosesChild pins that a plane that drops its request
// body WITHOUT a "close" frame still terminates the child: the client
// translates the EOF into a "close" frame, so the supervisor's control loop
// drains instead of waiting forever (a leaked child + a wedged daemon relay).
func TestRunClientStdioEOFClosesChild(t *testing.T) {
	sock := shortSocketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	got := make(chan AgentRequest, 1)
	go func() {
		conn, aerr := l.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		dec := json.NewDecoder(conn)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var f AgentRequest
			if derr := dec.Decode(&f); derr != nil {
				return
			}
			if f.Cmd == "close" {
				select {
				case got <- f:
				default:
				}
				// Ack the drain with the terminal event, exactly as the
				// real supervisor does after closing the child's stdin.
				_ = json.NewEncoder(conn).Encode(AgentEvent{Event: "exit", ExitCode: 0})
				return
			}
		}
	}()

	var in bytes.Buffer
	in.WriteString(`{"cmd":"stdio","exec_id":"exec-2","argv":["claude","-p"]}` + "\n")

	code, err := RunClient(sock, &in, io.Discard)
	if err != nil {
		t.Fatalf("RunClient: %v", err)
	}
	if code != 0 {
		t.Fatalf("RunClient exit code = %d, want 0", code)
	}
	select {
	case f := <-got:
		if f.ExecID != "exec-2" {
			t.Fatalf("close frame exec id = %q, want exec-2", f.ExecID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdin EOF did not produce a close frame for the child")
	}
}
