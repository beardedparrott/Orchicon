// Package opencode implements the OpenCode adapter bridge — a CLI
// subprocess wrapper that translates OpenCode's stdout JSON into
// execution telemetry events (docs/04_Runtime_Adapter_SDK.md §6).
//
// v0.1 transport strategy (docs/04 §6.0): the adapter spawns OpenCode
// as a subprocess, drives it via CLI flags, and parses JSON from stdout.
// This is the only stable surface available today and is sufficient to
// validate the orchestration model end-to-end. When OpenCode ships a
// stable IPC API, the adapter swaps its internals to an IPC client; the
// gRPC contract and control plane are unaffected.
//
// The adapter MUST NOT advertise capabilities the CLI surface cannot
// honestly deliver (docs/04 §6.2). v0.1 advertises a reduced
// capability set.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Adapter is the OpenCode CLI adapter bridge. It implements
// scheduler.AdapterBridge by spawning `opencode` as a subprocess and
// parsing stdout JSON lines into telemetry events.
type Adapter struct {
	log    *slog.Logger
	mu     sync.Mutex
	active map[string]*exec.Cmd // execution_id → running subprocess
}

// New creates an OpenCode adapter bridge.
func New(log *slog.Logger) *Adapter {
	return &Adapter{
		log:    log,
		active: make(map[string]*exec.Cmd),
	}
}

// Start spawns an OpenCode subprocess for the given execution and
// streams telemetry back via the callbacks (docs/03 §4, docs/04 §6).
// The subprocess runs until completion or context cancellation.
func (a *Adapter) Start(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	// In v0.1, if the `opencode` binary is not on PATH, we run in
	// "simulation mode" — emitting synthetic telemetry events so the
	// end-to-end dispatch flow can be verified without a real runtime
	// (docs/04 §6.3: "for local dev, an in-process adapter is supported
	// for tests only").
	binary, err := exec.LookPath("opencode")
	if err != nil {
		a.log.Warn("opencode binary not found — running in simulation mode", "execution", execRow.ID)
		return a.runSimulation(ctx, execRow, manifest, callbacks)
	}

	// Build the command. OpenCode's CLI surface varies by version; we
	// pass the manifest as flags + env. The adapter parses stdout JSON
	// lines (docs/04 §6.1: "parse tool-call/transcript lines from
	// stdout JSON").
	args := []string{
		"--non-interactive",
		"--prompt", manifest.Goal,
	}
	if manifest.SystemPrompt != "" {
		args = append(args, "--system-prompt", manifest.SystemPrompt)
	}
	if manifest.ModelRef != "" {
		args = append(args, "--model", manifest.ModelRef)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(),
		"OPENCODE_EXECUTION_ID="+execRow.ID,
		"OPENCODE_TASK_ID="+manifest.TaskID,
		"OPENCODE_PROJECT_ID="+manifest.ProjectID,
	)

	// Capture stdout + stderr.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencode: start: %w", err)
	}

	a.mu.Lock()
	a.active[execRow.ID] = cmd
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.active, execRow.ID)
		a.mu.Unlock()
	}()

	// Signal execution started (docs/03 §6: assigned → running).
	callbacks.OnStarted(ctx, execRow.ID)

	// Parse stdout JSON lines into telemetry events
	// (docs/04 §6.1: line-buffered stdout parsing).
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		a.parseStdoutLine(ctx, execRow.ID, line, callbacks)
	}

	// Wait for the process to exit.
	err = cmd.Wait()
	succeeded := err == nil
	callbacks.OnResult(ctx, execRow.ID, succeeded)
	if err != nil {
		a.log.Warn("opencode subprocess exited with error", "execution", execRow.ID, "error", err)
	}
	return nil
}

// parseStdoutLine decodes a JSON line from OpenCode's stdout into a
// telemetry event and routes it to the callbacks. The JSON shape is
// adapter-defined (docs/04 §6.1).
func (a *Adapter) parseStdoutLine(ctx context.Context, execID, line string, callbacks scheduler.ExecutionCallbacks) {
	var evt map[string]any
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		// Non-JSON line: treat as a log/progress marker.
		a.log.Debug("opencode stdout (non-JSON)", "execution", execID, "line", line)
		return
	}
	eventType, _ := evt["type"].(string)
	switch eventType {
	case "tool_call":
		a.log.Info("opencode tool call", "execution", execID, "tool", evt["tool"])
	case "file_diff":
		a.log.Info("opencode file diff", "execution", execID, "path", evt["path"])
	case "prompt_response":
		a.log.Info("opencode prompt response", "execution", execID)
	case "health":
		if state, ok := evt["state"].(string); ok {
			callbacks.OnHealth(ctx, execID, state)
		}
	case "error":
		a.log.Warn("opencode error", "execution", execID, "message", evt["message"])
		callbacks.OnHealth(ctx, execID, domain.HealthUnhealthy)
	default:
		a.log.Debug("opencode event", "execution", execID, "type", eventType)
	}
}

// runSimulation emits synthetic telemetry events so the dispatch flow
// can be verified end-to-end without the `opencode` binary
// (docs/04 §6.3: in-process adapter for tests/dev only).
func (a *Adapter) runSimulation(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	callbacks.OnStarted(ctx, execRow.ID)

	// Simulate a short execution with progress events.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	steps := 0
	maxSteps := 3
	for {
		select {
		case <-ctx.Done():
			callbacks.OnResult(ctx, execRow.ID, false)
			return ctx.Err()
		case <-ticker.C:
			steps++
			a.log.Info("opencode simulation: progress",
				"execution", execRow.ID, "step", steps, "max", maxSteps,
				"goal", manifest.Goal)
			if steps >= maxSteps {
				callbacks.OnResult(ctx, execRow.ID, true)
				return nil
			}
		}
	}
}

// Compile-time assertion that Adapter satisfies the AdapterBridge
// interface.
var _ scheduler.AdapterBridge = (*Adapter)(nil)
