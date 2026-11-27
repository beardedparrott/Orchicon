package runtime

// "serve handshake: exit status 1" COST A PRODUCTION INVESTIGATION.
//
// The daemon runs `docker exec -i <container> orchicon runtime-client` (daemon.go, startServe) and used
// `cmd.Output()`, which DISCARDS stderr. When the container has no orchicon binary, the engine writes
//
//	OCI runtime exec failed: exec failed: unable to start container process:
//	exec: "orchicon": executable file not found in $PATH
//
// to stderr and exits 127 — and the operator saw only "serve handshake: exit status 1". That line points
// at nothing: it sent the investigation to the session store, the runtime image and the container engine,
// when the binary was simply absent from the container being exec'd.
//
// These are the assertions that would have shortened it to one look.

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// Reproduced from the real engine output (docker 29.10, orchicon-runtime:orchicon-dev, no binary mount).
const realEngineStderr = `OCI runtime exec failed: exec failed: unable to start container process: ` +
	`exec: "orchicon": executable file not found in $PATH: unknown`

func TestServeHandshakeHintNamesTheMissingBinary(t *testing.T) {
	hint := serveHandshakeHint(realEngineStderr)
	if hint == "" {
		t.Fatal("the hint is empty for the actual engine output. Without it the operator sees only " +
			"'exit status 1', which is what sent a production investigation to the wrong component.")
	}
	for _, want := range []string{"bind-mounted", "not created by the daemon"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint must state %q — the binary is deliberately NOT baked into the image, so "+
				"'missing from the image' is the wrong conclusion. hint: %s", want, hint)
		}
	}
}

// An unrelated engine failure must NOT be dressed up as a missing binary.
func TestServeHandshakeHintStaysQuietForOtherFailures(t *testing.T) {
	for _, other := range []string{
		"Error response from daemon: container is not running",
		"permission denied while trying to connect to the Docker daemon socket",
		"",
	} {
		if hint := serveHandshakeHint(other); hint != "" {
			t.Errorf("a hint was produced for an unrelated engine failure (%q): %s", other, hint)
		}
	}
}

// The message must carry the engine's own words, not just the exit status. This is the actual defect:
// the cause existed and was thrown away.
func TestServeHandshakeErrorWouldCarryTheEngineStderr(t *testing.T) {
	// The shape the fixed code produces, asserted as the property rather than by running a container:
	// cmd.Stderr is wired, and the formatted error includes it.
	src := mustReadFile(t, "daemon.go")
	body := funcBodyText(t, src, "func (d *Daemon) startServe(")
	if !strings.Contains(body, "cmd.Stderr") {
		t.Error("startServe still discards the container engine's stderr. That text IS the diagnosis: " +
			"without it the error is 'serve handshake: exit status 1', which names no cause at all.")
	}
	if !strings.Contains(body, "engine said:") {
		t.Error("the handshake error must include what the engine actually said")
	}
	if !strings.Contains(body, "serveHandshakeHint(") {
		t.Error("the handshake error must translate the engine's words into the remedy")
	}
	// And the exit status alone must not be the fallback when stderr is empty.
	if !strings.Contains(body, "produced no stderr") {
		t.Error("when the engine is silent the message must SAY it is silent, rather than implying the " +
			"exit status is the whole story")
	}
}

// --- helpers ---

func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	b, err := readFileForTest(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func funcBodyText(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("declaration not found: %s", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	return rest
}

var _ = errors.New
var _ = exec.Command

// runtime-client exits 1 after relaying the supervisor's {"event":"error"} line on STDOUT, so the engine's
// stderr is empty. The handshake error must carry that line.
func TestSupervisorErrorFromStdout(t *testing.T) {
	out := []byte(`{"event":"error","error":"chdir /x: no such file or directory"}` + "\n")
	if got := supervisorErrorFrom(out); got != "chdir /x: no such file or directory" {
		t.Errorf("got %q", got)
	}
	for _, none := range []string{"", `{"event":"serve","port":4096}`, "not json"} {
		if got := supervisorErrorFrom([]byte(none)); got != "" {
			t.Errorf("%q: unexpected %q", none, got)
		}
	}
}
