package runtime

// client_unreachable_test.go — A MISSING DAEMON MUST SAY SO, NOT PRINT A DIAL STRING.
//
// The operator, on a native Windows plane:
//
//	ConnectError: [internal] runtime image fetch failed for ghcr.io/beardedparrott/orchicon-runtime:latest:
//	Post "http://runtime/v1/images/pull?ref=...": dial unix
//	C:\Users\<user>\AppData\Local\Temp\orchicon-runtime\runtime.sock:
//	connect: No connection could be made because the target machine actively refused it.
//
// Every part of that is true and none of it is the answer: the host says `http://runtime` (a placeholder the
// dialer ignores), the path is a Unix socket on a platform that cannot serve one, and nothing mentions that
// fetching a runtime image requires a daemon this host does not have.
//
// These tests drive the REAL transport against a REAL refused socket — no stub dialer — because the whole
// question is what a genuine ECONNREFUSED becomes.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// closedSocketPath returns a path inside a fresh temp dir that NOBODY is listening on.
//
// The daemon is "missing" in the way production sees it: the path may not exist at all, which is also the
// Windows case (nothing ever bound it). Dialling it fails at connect.
func closedSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "orchicon-nodaemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "runtime.sock")
}

// TestAMissingDaemonIsDiagnosedNotJustDropped is the reported case, at the client boundary the plane uses.
func TestAMissingDaemonIsDiagnosedNotJustDropped(t *testing.T) {
	c := NewClient(closedSocketPath(t), "dev")

	_, _, err := c.PullImage(context.Background(), "ghcr.io/beardedparrott/orchicon-runtime:latest", nil)
	if err == nil {
		t.Fatal("a client pointed at a socket nobody serves must fail")
	}

	var unreachable *DaemonUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("error is %T, not a DaemonUnreachableError — a caller cannot tell 'no daemon' from 'the daemon "+
			"said no', which is the distinction the operator needed: %v", err, err)
	}
	msg := err.Error()
	// It must say what is actually wrong…
	if !strings.Contains(msg, "runtime daemon") {
		t.Errorf("the message must name the missing daemon: %s", msg)
	}
	// …name the socket, so a WRONG socket is diagnosable…
	if !strings.Contains(msg, unreachable.SocketPath) {
		t.Errorf("the message must name the socket it tried: %s", msg)
	}
	// …and tell the operator that this is a platform fact, not a corrupt install.
	for _, want := range []string{"Unix socket", "Linux"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message must explain the platform constraint (%q missing): %s", want, msg)
		}
	}
	// THE RAW FAILURE IS NOT LOST: someone with a permissions problem needs it, and it is a different fix.
	if !strings.Contains(msg, "refused") && !strings.Contains(msg, "no such file") {
		t.Logf("note: underlying failure phrasing was %q (platform-dependent, not asserted)", unreachable.Err)
	}
	if unreachable.Unwrap() == nil {
		t.Error("Unwrap must expose the dial failure so errors.Is(err, syscall.ECONNREFUSED) still works")
	}
}

// TestEveryMethodGetsTheDiagnosis pins the reason this is a TRANSPORT: the client has ten methods and all of them
// share one http.Client, so a guard at the transport covers all of them — including ones added later.
func TestEveryMethodGetsTheDiagnosis(t *testing.T) {
	c := NewClient(closedSocketPath(t), "dev")
	ctx := context.Background()

	calls := map[string]func() error{
		"Create": func() error { _, err := c.Create(ctx, CreateRequest{WorkflowID: "wf"}); return err },
		"List":   func() error { _, err := c.List(ctx); return err },
		"Images": func() error { _, err := c.Images(ctx); return err },
		"Pull":   func() error { _, _, err := c.PullImage(ctx, "x:1", nil); return err },
		"Build":  func() error { _, err := c.BuildImage(ctx, BuildRequest{Tag: "x:1"}, nil); return err },
		"Remove": func() error { return c.RemoveImage(ctx, "x:1") },
		"Inspect": func() error {
			_, err := c.InspectImage(ctx, "x:1")
			return err
		},
		"Exec": func() error { _, err := c.Exec(ctx, "wf", ExecRequest{}); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s: no error against a socket nobody serves", name)
			continue
		}
		var unreachable *DaemonUnreachableError
		if !errors.As(err, &unreachable) {
			t.Errorf("%s: error is %T (%v), not a DaemonUnreachableError — the transport wrapper is being "+
				"bypassed somewhere", name, err, err)
		}
	}
}

// TestALiveDaemonAnswerIsNotRewritten is the other half: once the connection is ESTABLISHED, a failure belongs to
// the daemon and must pass through in its own words.
func TestALiveDaemonAnswerIsNotRewritten(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "orchicon-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "runtime.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("this platform cannot listen on a unix socket (%v) — the assertion below is a Linux/macOS check", err)
	}
	defer ln.Close()

	// A daemon that answers — badly, but answers. Its status/body must reach the caller unmodified.
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				// The body length must be the REAL byte count: a short Content-Length truncated the JSON
				// mid-string and sent the reader down its raw-body fallback, which tested the fixture rather
				// than the wrapper.
				const body = `{"error":"pull exploded"}`
				_, _ = c.Write([]byte("HTTP/1.1 500 Internal Server Error\r\nContent-Length: " +
					itoa(len(body)) + "\r\n\r\n" + body))
			}(conn)
		}
	}()

	c := NewClient(sock, "dev")
	_, _, err = c.PullImage(context.Background(), "x:1", nil)
	if err == nil {
		t.Fatal("a 500 from the daemon must be an error")
	}
	var unreachable *DaemonUnreachableError
	if errors.As(err, &unreachable) {
		t.Errorf("a LIVE daemon's failure was rewritten as 'unreachable' — the wrapper must only replace DIAL "+
			"failures, or real daemon errors stop being diagnosable: %v", err)
	}
	if !strings.Contains(err.Error(), "pull exploded") {
		t.Errorf("the daemon's own message was lost: %v", err)
	}
}

// TestNoDaemonSentenceIsHonestAboutWhatStillWorks guards the wording, because this text is what an operator reads
// instead of a stack trace. It must not claim the plane is broken.
func TestNoDaemonSentenceIsHonestAboutWhatStillWorks(t *testing.T) {
	for _, want := range []string{"runtime image", "Linux"} {
		if !strings.Contains(NoDaemonOnThisHost, want) {
			t.Errorf("NoDaemonOnThisHost must mention %q: %s", want, NoDaemonOnThisHost)
		}
	}
	if strings.Contains(NoDaemonOnThisHost, "error") && !strings.Contains(NoDaemonOnThisHost, "requires") {
		t.Error("the sentence should describe the constraint, not just assert an error")
	}
}

// itoa keeps the fixture's Content-Length honest without importing fmt into the assertion path.
func itoa(n int) string { return strconv.Itoa(n) }
