package adapter

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// RunStepResolution is one worker-bearing step of a run, resolved the way
// DISPATCH resolves it: the step-pinned version BY NUMBER when it carries
// one, else the latest published version.
//
// It carries the resolved VERSION ROW, not just the model ref, because the
// version's permissions jsonb is where a worker's own (inline) MCP
// definitions and skill files live — the run-level union needs the whole
// row, and re-reading it in a second walk is exactly the drift this type
// exists to prevent.
type RunStepResolution struct {
	StepID        string
	Kind          string
	WorkerID      string
	WorkerVersion int
	// ModelRef is the resolved adapter model ref, verbatim. "" means the
	// step's worker could not be resolved — callers treat it
	// CONSERVATIVELY (the demand set folds it to the default adapter kind,
	// the union simply gets no inline definitions from that step).
	ModelRef string
	// Version is the resolved worker version row (zero value when the step
	// resolved to nothing).
	Version db.WorkerVersionRow
}

// RunStepResolutions is the ONE walk's result, in step order.
type RunStepResolutions []RunStepResolution

// ModelRefs returns the resolved model refs in step order ("" included for
// unresolvable steps — the demand set's conservative rule depends on seeing
// them).
func (r RunStepResolutions) ModelRefs() []string {
	out := make([]string, 0, len(r))
	for _, s := range r {
		out = append(out, s.ModelRef)
	}
	return out
}

// ResolveRunSteps is THE one walk over a run's steps.
//
// It resolves each task/approval step's worker version ONCE, and BOTH
// consumers read that one result: the adapter demand set
// (scheduler's runNeedsServe) and the MCP/skills union
// (mcpsettings' run-scope resolver). One walk, so the two cannot drift.
//
// The behaviour mirrors the previous in-line loop in
// runNeedsServe exactly: non-worker kinds are skipped, a step with an empty
// Ref is skipped, the step-pinned version is looked up BY NUMBER
// (GetWorkerVersionByNumber), and an empty resolution falls back to the
// latest PUBLISHED version. A load failure folds to the zero value (empty
// ModelRef), which the demand set reads as "conservative default".
//
// IMPORTANT — it takes STEPS, never an executing worker: the union half of
// its result is applied ONCE per container, so a per-execution input would
// make it order-dependent.
func ResolveRunSteps(ctx context.Context, tx pgx.Tx, tenantID string, steps []workflow.StepWire) RunStepResolutions {
	out := make(RunStepResolutions, 0, len(steps))
	for _, s := range steps {
		switch s.Kind {
		case domain.StepKindTask, domain.StepKindApproval:
		default:
			continue // no worker ref → no adapter, no inline definitions
		}
		if s.Ref == "" {
			continue
		}
		r := RunStepResolution{StepID: s.ID, Kind: s.Kind, WorkerID: s.Ref, WorkerVersion: s.WorkerVersion}
		if s.WorkerVersion > 0 {
			// By NUMBER, not by id (see GetWorkerVersionByNumber): a step
			// pins "version 3", which is not a row id.
			if v, err := db.GetWorkerVersionByNumber(ctx, tx, tenantID, s.Ref, s.WorkerVersion); err == nil {
				r.ModelRef = v.ModelRef
				r.Version = v
			}
		}
		if r.ModelRef == "" {
			if v, err := db.GetLatestWorkerVersion(ctx, tx, tenantID, s.Ref, true); err == nil {
				r.ModelRef = v.ModelRef
				r.Version = v
			}
		}
		out = append(out, r)
	}
	return out
}
