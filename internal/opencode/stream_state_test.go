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
