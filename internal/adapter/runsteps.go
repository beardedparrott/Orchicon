package adapter

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// RunStepResolution is one worker-bearing step of a run, resolved the way DISPATCH resolves it: the
// step-pinned version BY NUMBER when it carries one, else the latest published version.
type RunStepResolution struct {
	StepID        string
	Kind          string
	WorkerID      string
	WorkerVersion int
	// ModelRef is the resolved adapter model ref, verbatim.
	ModelRef string
	// Version is the resolved worker version row (zero value when the step
	// resolved to nothing).
	Version db.WorkerVersionRow
}

// RunStepResolutions is the ONE walk's result, in step order.
type RunStepResolutions []RunStepResolution

// ModelRefs returns the resolved model refs in step order ("" included for unresolvable steps — the
// demand set's conservative rule depends on seeing them).
func (r RunStepResolutions) ModelRefs() []string {
	out := make([]string, 0, len(r))
	for _, s := range r {
		out = append(out, s.ModelRef)
	}
	return out
}

// ResolveRunSteps is THE one walk over a run's steps.
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
