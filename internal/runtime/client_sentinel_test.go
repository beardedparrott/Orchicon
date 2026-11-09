//go:build !windows

package runtime

// client_sentinel_test.go — A REFUSAL THE RETRY LOOP MUST RESPECT HAS TO SURVIVE THE WIRE.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daemonAnswering starts a unix-socket HTTP server that answers every request with the given body, and returns a
// client pointed at it.
func daemonAnswering(t *testing.T, body string) *Client {
	t.Helper()
	sock := filepath.Join(shortSocketDir(t), "rt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, body)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(sock, "dev")
}

// shortSocketDir returns a directory short enough to hold a unix socket path.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		dir, err := os.MkdirTemp("/tmp", "orchts")
		if err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	dir := t.TempDir()
	if len(filepath.Join(dir, "rt.sock")) > 100 {
		t.Skipf("no path short enough for a unix socket here: %q", dir)
	}
	return dir
}

func TestTheContainerBinaryRefusalSurvivesTheWire(t *testing.T) {
	c := daemonAnswering(t, `{"error":"cannot create a runtime container: no linux/amd64 orchicon binary","code":"no_container_binary"}`)

	_, err := c.Create(context.Background(), CreateRequest{WorkflowID: "wf1"})
	if err == nil {
		t.Fatal("the daemon refused the create but the client reported success")
	}
	if !errors.Is(err, ErrNoContainerBinary) {
		t.Fatalf("the refusal lost its identity crossing the wire, so the retry loop cannot recognise it: %v", err)
	}
	// The operator still needs the sentence, not just the sentinel.
	if want := "no linux/amd64 orchicon binary"; !strings.Contains(err.Error(), want) {
		t.Errorf("the error dropped the daemon's explanation (want %q): %v", want, err)
	}
}

func TestAnOrdinaryFailureIsNotMistakenForAPermanentOne(t *testing.T) {
	c := daemonAnswering(t, `{"error":"engine is starting, try again"}`)

	_, err := c.Create(context.Background(), CreateRequest{WorkflowID: "wf1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrNoContainerBinary) {
		t.Fatalf("a retryable failure was classified as permanent — the serve gate would give up at once: %v", err)
	}
}
