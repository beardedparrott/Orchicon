package opencode

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestExecStreamStateBalancedSteps verifies that a run which emits matching
// step_start/step_finish pairs is NOT flagged as having an unfinished step —
// a normal completion must stay "succeeded".
func TestExecStreamStateBalancedSteps(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	stats := &execStreamState{}
	execRow := db.ExecutionRow{ID: "exec_test", TenantID: "tnt_dev"}
	manifest := scheduler.ExecutionManifest{}
	var output strings.Builder
	var lastStreamErr string
	textSeq := 0
	ctx := context.Background()

	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_start"}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)
	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_finish","part":{"tokens":{"input":1,"output":1},"cost":0.001}}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)

	if stats.stepStarts != 1 || stats.stepFinishes != 1 {
		t.Fatalf("expected 1/1 step start/finish, got %d/%d", stats.stepStarts, stats.stepFinishes)
	}
	if stats.stepStarts > stats.stepFinishes {
		t.Fatalf("balanced stream must not flag an unfinished step")
	}
}

// TestExecStreamStateUnfinishedStep verifies that a step_start with no
// matching step_finish (e.g. the final model response was dropped) is
// detectable so the caller can downgrade the result to a failure instead of
// reporting an empty run as a clean success.
func TestExecStreamStateUnfinishedStep(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	stats := &execStreamState{}
	execRow := db.ExecutionRow{ID: "exec_test", TenantID: "tnt_dev"}
	manifest := scheduler.ExecutionManifest{}
	var output strings.Builder
	var lastStreamErr string
	textSeq := 0
	ctx := context.Background()

	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_start"}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)

	if stats.stepStarts != 1 || stats.stepFinishes != 0 {
		t.Fatalf("expected 1/0 step start/finish, got %d/%d", stats.stepStarts, stats.stepFinishes)
	}
	if !(stats.stepStarts > stats.stepFinishes) {
		t.Fatalf("an unpaired step_start must be flagged as an unfinished step")
	}
}

// TestExecStreamStateTruncatedFinish verifies that a step_finish with reason
// "unknown" and zero tokens — the signature of an interrupted/truncated model
// turn — is flagged as truncated even though the step counters are balanced
// (the original bug: a 38/38 stream where the FINAL turn was cut off was
// recorded as a clean success with no decision signal).
func TestExecStreamStateTruncatedFinish(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	stats := &execStreamState{}
	execRow := db.ExecutionRow{ID: "exec_test", TenantID: "tnt_dev"}
	manifest := scheduler.ExecutionManifest{}
	var output strings.Builder
	var lastStreamErr string
	textSeq := 0
	ctx := context.Background()

	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_start"}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)
	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_finish","part":{"reason":"unknown","tokens":{"input":0,"output":0},"cost":0}}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)

	if stats.stepStarts != 1 || stats.stepFinishes != 1 {
		t.Fatalf("expected 1/1 step start/finish, got %d/%d", stats.stepStarts, stats.stepFinishes)
	}
	if !stats.truncatedFinish {
		t.Fatalf("a step_finish with reason unknown + zero tokens must flag truncatedFinish")
	}
	if !stats.unfinished() {
		t.Fatalf("balanced-but-truncated stream must still report unfinished")
	}
}

// TestExecStreamStateNormalFinishNotTruncated verifies a NORMAL step_finish
// (non-empty reason, real tokens) is NOT flagged as truncated — so a healthy
// completion stays "succeeded".
func TestExecStreamStateNormalFinishNotTruncated(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	stats := &execStreamState{}
	execRow := db.ExecutionRow{ID: "exec_test", TenantID: "tnt_dev"}
	manifest := scheduler.ExecutionManifest{}
	var output strings.Builder
	var lastStreamErr string
	textSeq := 0
	ctx := context.Background()

	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_start"}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)
	a.parseStdoutLine(ctx, execRow, manifest, `{"type":"step_finish","part":{"reason":"done","tokens":{"input":10,"output":5},"cost":0.001}}`, nil, nil, &output, &lastStreamErr, &textSeq, stats)

	if stats.truncatedFinish {
		t.Fatalf("a normal step_finish must not flag truncatedFinish")
	}
	if stats.unfinished() {
		t.Fatalf("a balanced normal stream must not report unfinished")
	}
}

// TestAllTokensZero covers the zero-token predicate directly (empty map,
// all-zero values, and a non-zero value).
func TestAllTokensZero(t *testing.T) {
	if !allTokensZero(nil) {
		t.Fatalf("nil tokens must be zero")
	}
	if !allTokensZero(map[string]any{}) {
		t.Fatalf("empty tokens must be zero")
	}
	if !allTokensZero(map[string]any{"input": 0.0, "output": 0.0}) {
		t.Fatalf("all-zero tokens must be zero")
	}
	if allTokensZero(map[string]any{"input": 5.0, "output": 0.0}) {
		t.Fatalf("a non-zero token must NOT be zero")
	}
}
