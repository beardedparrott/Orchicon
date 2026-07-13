package scheduler

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ExecutionManifest is the StartExecution payload sent to the adapter
// (docs/04_Runtime_Adapter_SDK.md §3.1). It contains everything the
// adapter needs to begin a WorkerExecution: identity, prompts, model,
// context sources, budgets, permissions.
type ExecutionManifest struct {
	ExecutionID        string
	TaskID             string
	ProjectID          string
	WorkerID           string
	WorkerVersion      int
	SystemPrompt       string
	Goal               string
	AcceptanceCriteria string
	ModelRef           string // human-defined; no auto-failover (docs/05 §11)
	ContextSources     []byte // jsonb
	Budgets            []byte // jsonb
	Permissions        []byte // jsonb
}

// ExecutionCallbacks are the status callbacks the adapter bridge uses to
// inform the TaskReconciler of execution lifecycle transitions
// (docs/03 §6).
type ExecutionCallbacks interface {
	OnStarted(ctx context.Context, execID string)
	OnResult(ctx context.Context, execID string, succeeded bool)
	OnHealth(ctx context.Context, execID, healthState string)
}

// AdapterBridge is the control-plane side of the adapter contract. It
// starts an execution on a registered adapter and streams telemetry
// back via the callbacks. The bridge abstracts whether the adapter is a
// real gRPC sidecar (v0.2+) or an in-process CLI subprocess wrapper
// (v0.1 — docs/04 §6.0).
//
// The scheduler is the only component permitted to call Start
// (docs/03 §8 invariant #1).
type AdapterBridge interface {
	Start(ctx context.Context, exec db.ExecutionRow, manifest ExecutionManifest, callbacks ExecutionCallbacks) error
}

// TaskReconciler implements ExecutionCallbacks so the adapter bridge can
// notify it of lifecycle transitions without import cycles.
var _ ExecutionCallbacks = (*TaskReconciler)(nil)

// RecoveryTrigger is the interface the TaskReconciler uses to trigger
// recovery when an execution fails (docs/06 §2). Satisfied by the
// recovery.Engine; declared here to avoid a scheduler→recovery import
// (loose coupling).
type RecoveryTrigger interface {
	TriggerOnFailure(ctx context.Context, tenantID, taskID, failedExecID, triggerReason string) error
}

// PolicyEvaluator is the interface the WorkflowReconciler uses to
// evaluate gate policies (docs/02 §2.5 Tier 1). Satisfied by the
// policy.Engine; declared here to avoid a scheduler→policy import for
// the reconciler (the engine is still constructed in the server and
// injected).
type PolicyEvaluator interface {
	// EvaluateGate returns (allowed, error). allowed=false blocks the
	// step transition (docs/02 §2.5: gate denied → blocked).
	EvaluateGate(ctx context.Context, tenantID, gatePolicyRef, targetType, targetID string, input any) (bool, error)
}
