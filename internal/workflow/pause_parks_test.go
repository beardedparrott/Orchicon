package workflow

import (
	"os"
	"strings"
	"testing"
)

// TestPauseParksAsPausedNotPending — using `pending` was what let the sequence engine re-arm the
// child in the same second as the pause.
func TestPauseParksAsPausedNotPending(t *testing.T) {
	b, err := os.ReadFile("run_control.go")
	if err != nil {
		t.Fatalf("read run_control.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")

	if !strings.Contains(src, "status := domain.WorkItemPaused") {
		t.Fatal("pause does not park the item as `paused`. `pending` means \"on deck\" to the sequence " +
			"reconciler, which arms it and re-fires the workflow the operator just paused.")
	}
	// The old pending park must be gone from the pause path.
	i := strings.Index(src, "func pauseRunInner")
	body := src[i:]
	if !strings.Contains(body, "WorkItemPaused") {
		t.Error("pauseRunInner does not set WorkItemPaused")
	}
	if strings.Contains(body, "status := domain.WorkItemPending") {
		t.Error("pauseRunInner still parks as pending")
	}
}
