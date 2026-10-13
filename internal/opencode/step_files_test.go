package opencode

// step_files_test.go — PER-STEP FILE ATTRIBUTION on a Code Mode serve.
//
// The board's diff viewer showed NOTHING for every opencode execution ever run
// (measured: 0 ledger rows across 2,398 executions). Two independent captures
// were both blind on 2.x:
//
//   - the LIVE hook in parseEvent keys on a TOOL NAME (write / edit /
//     batch_write), and on 2.x no MCP tool is ever a tool name: it is called
//     through `execute`, so the part says `execute` and every case misses;
//   - the END-OF-RUN git reconcile is defeated separately by the worker
//     COMMITTING its work — its brief orders a clean `git status`, which is
//     exactly the state in which `git status` reports nothing (covered in
//     internal/fileedit/reconcile_runbranch_test.go).
//
// So this file covers the remaining signal, and the only one that is a property
// of the STEP rather than of the call: `session.step.ended.files`, which
// opencode emits for the paths a step changed. It survives however the tool was
// invoked, because it is the agent surface reporting its own edit.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

const v2StepEndedWithFiles = `{"type":"session.step.ended","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","finish":"stop","cost":0.01,"tokens":{"input":10,"output":5},"files":["frontend/src/App.tsx","internal/tui/app.go"]}}`

// TestV2StepEndedCarriesTheChangedFiles asserts the mapper does not DISCARD the
// payload — the original omission was silent, and a dropped field is invisible
// from the transcript it should have reached.
func TestV2StepEndedCarriesTheChangedFiles(t *testing.T) {
	legacy, ok := LegacyEventFromBus(v2Bus(t, v2StepEndedWithFiles))
	if !ok {
		t.Fatal("session.step.ended was not classified")
	}
	part, _ := legacy["part"].(map[string]any)
	files, _ := part["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("the step_finish part carries %v, want the step's 2 changed files — dropping them loses the "+
			"only per-step attribution a Code Mode serve provides", part["files"])
	}
	if files[0] != "frontend/src/App.tsx" {
		t.Errorf("files[0] = %v, want the first path verbatim", files[0])
	}
}

// stepFinishEvt builds the legacy step_finish event parseEvent consumes.
func stepFinishEvt(files ...string) map[string]any {
	list := make([]any, 0, len(files))
	for _, f := range files {
		list = append(list, f)
	}
	return map[string]any{"type": "step_finish", "part": map[string]any{"files": list}}
}

// TestStepFilesReachTheFileEditHook is the wiring: the paths a step reports must
// arrive at the ledger hook, through the SAME "file_diff" arm the diff-event path
// uses (so the observer's before/after reading and its no-op suppression are
// shared rather than reimplemented).
func TestStepFilesReachTheFileEditHook(t *testing.T) {
	type call struct{ tool, path string }
	var got []call
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.fileEdits = func(_ context.Context, _, _, _, toolName string, input map[string]any, _ string) {
		p, _ := input["path"].(string)
		got = append(got, call{toolName, p})
	}

	ctx := context.Background()
	out := &strings.Builder{}
	var lastErr string
	seq := 0
	stats := &execStreamState{}
	a.parseEvent(ctx, db.ExecutionRow{ID: "exec-1"}, scheduler.ExecutionManifest{ProjectDir: "/proj"},
		stepFinishEvt("a.go", "b.go"), noopCallbacks{}, nil, out, &lastErr, &seq, stats, nil)

	if len(got) != 2 {
		t.Fatalf("the hook received %v, want one file_diff per reported path — a step's files never reach the "+
			"ledger, which is the operator's empty diff viewer", got)
	}
	if got[0].tool != "file_diff" || got[0].path != "a.go" {
		t.Errorf("first call = %+v, want {file_diff a.go}", got[0])
	}
	if got[1].path != "b.go" {
		t.Errorf("second call = %+v, want path b.go", got[1])
	}
}

// TestStepFilesSkipEngineCoveredPaths pins the de-duplication: a path the engine
// already recorded an EXACT diff for must not also get a snapshot row, or one
// edit renders twice in the viewer.
func TestStepFilesSkipEngineCoveredPaths(t *testing.T) {
	var got []string
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.fileEdits = func(_ context.Context, _, _, _, _ string, input map[string]any, _ string) {
		p, _ := input["path"].(string)
		got = append(got, p)
	}

	ctx := context.Background()
	out := &strings.Builder{}
	var lastErr string
	seq := 0
	stats := &execStreamState{engineEditedPaths: map[string]bool{"covered.go": true}}
	a.parseEvent(ctx, db.ExecutionRow{ID: "exec-1"}, scheduler.ExecutionManifest{ProjectDir: "/proj"},
		stepFinishEvt("covered.go", "fresh.go"), noopCallbacks{}, nil, out, &lastErr, &seq, stats, nil)

	if len(got) != 1 || got[0] != "fresh.go" {
		t.Fatalf("hook paths = %v, want only the engine-uncovered path — a double-recorded edit renders twice", got)
	}
}

// TestStepWithoutFilesTouchesNoLedger is the CONTROL: a step that reports no
// files must not reach the hook at all. Otherwise every step would manufacture a
// row for an empty path, and the ledger would fill with noise.
func TestStepWithoutFilesTouchesNoLedger(t *testing.T) {
	calls := 0
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.fileEdits = func(context.Context, string, string, string, string, map[string]any, string) { calls++ }

	ctx := context.Background()
	out := &strings.Builder{}
	var lastErr string
	seq := 0
	a.parseEvent(ctx, db.ExecutionRow{ID: "exec-1"}, scheduler.ExecutionManifest{ProjectDir: "/proj"},
		map[string]any{"type": "step_finish", "part": map[string]any{"cost": 0.01}},
		noopCallbacks{}, nil, out, &lastErr, &seq, &execStreamState{}, nil)

	if calls != 0 {
		t.Errorf("a step with no files made %d ledger call(s), want 0", calls)
	}
}
