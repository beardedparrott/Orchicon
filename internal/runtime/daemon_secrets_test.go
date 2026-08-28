package runtime

import (
	"os"
	"strings"
	"testing"
)

func TestCreateContainerInjectsSecretsAsEnv(t *testing.T) {
	tmp, _ := os.CreateTemp("", "orchicon-exe-*")
	tmp.Close()
	defer os.Remove(tmp.Name())
	var captured []string
	d := &Daemon{
		SocketPath:   "/tmp/test.sock",
		DockerBin:    "docker",
		Image:        "orchicon-runtime:local",
		AllowedRoots: []string{"/tmp"},
		ExePath:      tmp.Name(),
		CPUs: "1", Memory: "1g", TmpfsSize: "100m",
		HostHome:     "",
	}
	d.dockerFn = func(args ...string) (string, error) {
		captured = args
		if len(args) >= 2 && args[0] == "inspect" {
			return "", &fakeErr{"not found"}
		}
		if len(args) > 0 && args[0] == "run" {
			// Simulate successful run creation; the subsequent ping will fail but we have captured args
			return "cid123", nil
		}
		return "ok", nil
	}
	req := CreateRequest{
		WorkflowID: "test-workflow-123",
		Secrets:    map[string]string{"TAVILY_API_KEY": "tvly-secret", "OTHER_KEY": "val2"},
	}
	// Call createContainer in a goroutine and give it a short time to capture the run args before ping timeout.
	done := make(chan struct{})
	go func() {
		_, _ = d.createContainer("test-secrets-container", req)
		close(done)
	}()
	// Wait a bit for dockerFn to be called
	for i := 0; i < 20; i++ {
		if len(captured) > 0 {
			break
		}
		// tiny sleep via busy loop
		for j := 0; j < 1000000; j++ {}
	}
	// Give the goroutine time to hit docker run
	// Check even if createContainer still pinging
	if len(captured) == 0 {
		t.Fatal("docker was not called")
	}
	joined := strings.Join(captured, " ")
	if !strings.Contains(joined, "TAVILY_API_KEY=tvly-secret") {
		t.Fatalf("expected TAVILY_API_KEY injected as -e, got %v", captured)
	}
	if !strings.Contains(joined, "OTHER_KEY=val2") {
		t.Fatalf("expected OTHER_KEY injected, got %v", captured)
	}
	if !strings.Contains(joined, "-e") {
		t.Fatalf("no -e flags found in %v", captured)
	}
}

func errNotFound() error { return &fakeErr{"not found"} }

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
