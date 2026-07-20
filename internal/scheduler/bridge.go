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
// (docs/03 §6). OnStall is the stall-detection trigger (docs/06 §2
// "stalled health state | no progress within stall window"): the adapter
// bridge's progress monitor raises it when a worker is stuck looping
// (repeated tool calls, no file changes, no token progress), and the
// TaskReconciler uses it to trigger recovery (idempotent — docs/06 §9).
type ExecutionCallbacks interface {
	OnStarted(ctx context.Context, execID string)
	// OnResult carries the worker's accumulated text output (PR B —
	// context propagation). The TaskReconciler extracts the ORCHICON
	// WORKER SUMMARY block from `output`, persists it as the work
	// item's `_summary`, and copies it onto the linked workflow step
	// run so downstream stages see it. `output` may be empty if the
	// adapter didn't accumulate any text (e.g. the worker errored
	// before producing output).
	OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string)
	OnHealth(ctx context.Context, execID, healthState string)
	// OnStall signals a detected stall (the reason carries which signal
	// tripped: stalled:no_progress | stalled:no_file_progress |
	// stalled:repetition:<sig>). The receiver triggers recovery.
	OnStall(ctx context.Context, execID, reason string)
	// OnToolCall notifies the runtime that the worker invoked a tool.
	// Published as a tool_call execution event for the live session pane.
	OnToolCall(ctx context.Context, execID, toolName string, input, output []byte)
	// OnText notifies the runtime that the worker produced text output.
	// Published as a telemetry execution event for the live session pane.
	OnText(ctx context.Context, execID string, text string)
	// OnArtifact notifies the runtime that the worker produced an output
	// artifact (e.g. a file via the `write` tool). The name is the file
	// path, artifactType is the MIME type or extension hint (e.g. "markdown",
	// "json", "text"), and content is the full artifact body. Published as
	// an EXECUTION_EVENT_TYPE_ARTIFACT event for inline display (docs/10 §11).
	OnArtifact(ctx context.Context, execID, name, artifactType, content string)
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

// TaskDispatcher is the interface the WorkflowReconciler uses to
// dispatch a ready work item to a WorkerExecution immediately after
// creating it (docs/03 §8 invariant #1: only the TaskReconciler creates
// WorkerExecutions). The WorkflowReconciler calls DispatchTask after
// its own transaction commits so the work item is visible to the
// TaskReconciler's dispatch transaction.
type TaskDispatcher interface {
	DispatchTask(ctx context.Context, taskID string) error
}
