package runtime

// TestCreateContainerInjectsSecretsAsEnv pins the secret-injection contract of
// the runtime container create: every request secret rides the `docker run` argv
// as its own `-e K=V` flag.
//
// It used to be FLAKY, and the flake is worth recording because the symptom was
// misleading. Two defects, either of which alone could fail it:
//
//  1. NO SYNCHRONIZATION. The fake recorded calls into a plain `captured`
//     variable from the createContainer goroutine while the test goroutine read
//     it, separated by a best-effort busy-wait (`for j := 0; j < 1000000; j++`)
//     rather than a barrier. That is a data race — `go test -race` would flag it
//     — and the busy-wait is not a memory barrier, so the read could observe a
//     stale or torn value.
//  2. LAST-CALL SEMANTICS. `captured = args` kept only the MOST RECENT docker
//     call, but the property under test is about the `run` call specifically.
//     createContainer issues an `inspect` FIRST (containerExists) and only then
//     `run`, so the assertion was really "whichever call happened to land last".
//
// Together: the spin broke the instant `captured` became non-empty — which the
// `inspect` call alone satisfies — so the test passed only when the goroutine
// won the race all the way to `run` in one scheduling quantum. Under the load of
// a full `go test ./...` (packages compile and run in parallel) it read the
// inspect args and failed with:
//
//	expected TAVILY_API_KEY injected as -e, got
//	[inspect --format {{.Id}} test-secrets-container]
//
// Reproduced deterministically by starving the goroutine (GOMAXPROCS=1): 15/15
// failures, all reporting the inspect call as if it were the run call.
//
// The fix synchronizes on the `run` call ITSELF via a channel, so there is no
// wait-and-hope and no dependence on which call was last.

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// runCallTimeout bounds the wait for the create goroutine to reach `docker run`.
// Generous — this is a scheduling deadline, not a performance assertion — so a
// slow or heavily loaded machine reports a real failure ("run was never called")
// rather than a timing flake.
const runCallTimeout = 10 * time.Second

func TestCreateContainerInjectsSecretsAsEnv(t *testing.T) {
	tmp, _ := os.CreateTemp("", "orchicon-exe-*")
	tmp.Close()
	defer os.Remove(tmp.Name())

	// calls records EVERY docker invocation (under a mutex) purely so a failure
	// can show what actually ran; runArgs carries the `run` argv to the test.
	var mu sync.Mutex
	var calls [][]string
	runArgs := make(chan []string, 1)

	d := &Daemon{
		SocketPath:   "/tmp/test.sock",
		DockerBin:    "docker",
		Image:        "orchicon-runtime:local",
		AllowedRoots: []string{"/tmp"},
		ExePath:      tmp.Name(),
		CPUs:         "1", Memory: "1g", TmpfsSize: "100m",
		HostHome: "",
	}
	d.dockerFn = func(args ...string) (string, error) {
		cp := append([]string(nil), args...)
		mu.Lock()
		calls = append(calls, cp)
		mu.Unlock()

		if len(args) >= 2 && args[0] == "inspect" {
			// No container yet: createContainer proceeds to `run`.
			return "", errNotFound()
		}
		if len(args) > 0 && args[0] == "run" {
			// Signal the run argv to the test. Non-blocking + buffered: the
			// create goroutine must never be blocked by an unread channel.
			select {
			case runArgs <- cp:
			default:
			}
			// Simulate a successful create; the readiness ping that follows
			// will keep failing (no real supervisor socket), which is fine —
			// the create goroutine simply gives up at its own deadline.
			return "cid123", nil
		}
		return "ok", nil
	}

	req := CreateRequest{
		WorkflowID: "test-workflow-123",
		Secrets:    map[string]string{"TAVILY_API_KEY": "tvly-secret", "OTHER_KEY": "val2"},
	}

	go func() {
		_, _ = d.createContainer("test-secrets-container", req)
	}()

	// Synchronize on the RUN call. Not on "any docker call" (the inspect lands
	// first) and not on a spin count — on the call whose argv is under test.
	var run []string
	select {
	case run = <-runArgs:
	case <-time.After(runCallTimeout):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("docker run was never invoked within %s; captured calls: %v", runCallTimeout, calls)
	}

	joined := strings.Join(run, " ")

	// The create targets the requested container.
	if !strings.Contains(joined, "test-secrets-container") {
		t.Errorf("run argv does not name the container: %v", run)
	}

	// Each secret rides as its OWN `-e K=V` flag. Asserting the flag and the
	// pair TOGETHER (not "K=V somewhere" plus "-e somewhere else") is the whole
	// point: a value that leaked into some other argument would not be injected
	// as an environment variable.
	for _, want := range []string{"-e TAVILY_API_KEY=tvly-secret", "-e OTHER_KEY=val2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run argv missing %q\ngot: %v", want, run)
		}
	}

	// The secrets are ENVIRONMENT flags, not image/command arguments: they must
	// precede the image + entrypoint tail, or docker would treat them as the
	// command to exec.
	imageIdx := -1
	for i, a := range run {
		if a == d.Image {
			imageIdx = i
			break
		}
	}
	if imageIdx < 0 {
		t.Fatalf("run argv is missing the image %q: %v", d.Image, run)
	}
	for i, a := range run {
		if strings.HasPrefix(a, "TAVILY_API_KEY=") || strings.HasPrefix(a, "OTHER_KEY=") {
			if i > imageIdx {
				t.Errorf("secret %q appears AFTER the image (it would be a command arg, not env): %v", a, run)
			}
		}
	}
}

func errNotFound() error { return &fakeErr{"not found"} }

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
