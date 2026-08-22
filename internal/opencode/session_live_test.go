package opencode

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// liveCallbacks records the terminal OnResult so the test can assert on
// the session runner's outcome.
type liveCallbacks struct {
	mu     sync.Mutex
	ok     bool
	output string
	errMsg string
	got    bool
}

func (c *liveCallbacks) OnStarted(context.Context, string)                          {}
func (c *liveCallbacks) OnHealth(context.Context, string, string)                   {}
func (c *liveCallbacks) OnStall(context.Context, string, string, bool)              {}
func (c *liveCallbacks) OnRecovered(context.Context, string, string)                {}
func (c *liveCallbacks) OnText(context.Context, string, string)                     {}
func (c *liveCallbacks) OnToolCall(context.Context, string, string, []byte, []byte) {}
func (c *liveCallbacks) OnArtifact(context.Context, string, string, string, string) {}
func (c *liveCallbacks) OnResult(_ context.Context, _ string, ok bool, output, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ok = ok
	c.output = output
	c.errMsg = errMsg
	c.got = true
}

func (c *liveCallbacks) OnWrittenFiles(context.Context, string, []string) {}

func (c *liveCallbacks) result() (bool, string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok, c.output, c.errMsg, c.got
}

// TestSessionRunLive is the end-to-end sanity check of the session
// transport: a real host serve + a real (free) model, driven through the
// exact sessionRun the adapter uses. Set ORCHICON_TEST_OPENCODE=1 to run
// (it spawns opencode and makes one free-model call). Skipped in `make
// ci` by default.
func TestSessionRunLive(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_OPENCODE") != "1" {
		t.Skip("set ORCHICON_TEST_OPENCODE=1 to run the live serve test")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hs := NewHostServe(log, t.TempDir(), "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := hs.Start(ctx); err != nil {
		t.Fatalf("host serve start: %v", err)
	}
	defer hs.Stop()

	proj := t.TempDir()
	client := hs.Client()
	if client == nil {
		t.Fatal("no host serve client")
	}

	callbacks := &liveCallbacks{}
	execRow := db.ExecutionRow{ID: "exec-live-test", TenantID: "tnt_dev", ProjectID: "proj-test", TaskID: "task-test"}
	manifest := scheduler.ExecutionManifest{
		Goal:         "Reply with exactly one word: PONG",
		SystemPrompt: "You are a terse test bot.",
		ModelRef:     "opencode/deepseek-v4-flash-free",
		ProjectDir:   proj,
	}

	a := New(log)
	r := &sessionRun{
		a:         a,
		parentCtx: ctx,
		procCtx:   ctx,
		execRow:   execRow,
		manifest:  manifest,
		callbacks: callbacks,
		client:    client,
		modelRef:  manifest.ModelRef,
		system:    manifest.SystemPrompt,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
	}
	if err := r.run(); err != nil {
		t.Fatalf("session run: %v", err)
	}

	ok, output, errMsg, got := callbacks.result()
	if !got {
		t.Fatal("OnResult never fired")
	}
	if !ok {
		t.Fatalf("execution failed: %s (output %q)", errMsg, output)
	}
	if !strings.Contains(strings.ToUpper(output), "PONG") {
		t.Errorf("expected the model reply to contain PONG, got %q", output)
	}
	t.Logf("session run ok, output: %q", strings.TrimSpace(output))

	// The serve data dir must exist and be the isolated one (sessions
	// persist there for follow-ups / troubleshooting).
	if _, err := os.Stat(filepath.Join(hs.dataDir, "opencode", "auth.json")); err != nil {
		t.Errorf("isolated auth.json not seeded: %v", err)
	}
}
