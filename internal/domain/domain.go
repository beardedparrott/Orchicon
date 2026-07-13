// Package domain defines Orchicon's core domain types.
//
// These types mirror the entities in docs/02_Domain_Model.md. They are
// the Go-level representation of the data-access layer's row shapes and
// the API layer's response payloads. Domain types hold no business
// logic — reconcilers and services operate on them.
package domain

import "time"

// Tenant is the root of multi-tenant isolation. All tenant_id-bearing
// tables scope to a Tenant. See docs/09_Database_Schema.md §3.1.
type Tenant struct {
	ID        string
	Slug      string
	Name      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Identity represents a user or service account within a tenant. OIDC
// subjects and API keys both resolve to an Identity.
type Identity struct {
	ID          string
	TenantID    string
	Subject     string // OIDC sub or API key id
	DisplayName string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Project is the top-level tenant of work state. Every schedulable
// entity FKs to a Project. See docs/02_Domain_Model.md §2.1.
type Project struct {
	ID        string
	TenantID  string
	Name      string
	Slug      string
	Status    string
	Goals     []byte // jsonb
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Project lifecycle states. See docs/02_Domain_Model.md §2.1.
const (
	ProjectDrafting = "drafting"
	ProjectActive   = "active"
	ProjectPaused   = "paused"
	ProjectArchived = "archived"
	ProjectDeleted  = "deleted"
)

// Worker lifecycle states. draft → published → deprecated → retired
// (docs/05_Worker_Specification.md §4).
const (
	WorkerDraft      = "draft"
	WorkerPublished  = "published"
	WorkerDeprecated = "deprecated"
	WorkerRetired    = "retired"
)

// WorkerVersion lifecycle states (docs/05 §4). A version is draft
// until published; deprecation is per-version.
const (
	WorkerVersionDraft      = "draft"
	WorkerVersionPublished  = "published"
	WorkerVersionDeprecated = "deprecated"
)

// WorkItem kinds — the four levels of the work hierarchy
// (docs/02_Domain_Model.md §2.2). Depth is constrained to 4 levels.
const (
	WorkItemKindEpic    = "epic"
	WorkItemKindFeature = "feature"
	WorkItemKindTask    = "task"
	WorkItemKindSubtask = "subtask"
)

// WorkItemStatus — schedulable kinds follow:
// pending → ready → assigned → running → checkpointing →
// succeeded | failed | cancelled | recovering (docs/02 §2.2).
const (
	WorkItemPending       = "pending"
	WorkItemReady        = "ready"
	WorkItemAssigned     = "assigned"
	WorkItemRunning      = "running"
	WorkItemCheckpointing = "checkpointing"
	WorkItemSucceeded    = "succeeded"
	WorkItemFailed       = "failed"
	WorkItemCancelled    = "cancelled"
	WorkItemRecovering   = "recovering"
)

// Dependency types — edges in the work DAG
// (docs/02_Domain_Model.md §2.2).
const (
	DependencyBlocks     = "blocks"
	DependencyDependsOn  = "depends_on"
	DependencyRelatesTo  = "relates_to"
)

// Resource types for edit locks (docs/07 §3.3).
const (
	EditLockResourceWorker   = "worker"
	EditLockResourceWorkflow = "workflow"
)

// Workflow lifecycle states (docs/02 §2.4):
// draft → published → deprecated. A published version is immutable.
const (
	WorkflowDraft      = "draft"
	WorkflowPublished  = "published"
	WorkflowDeprecated = "deprecated"
)

// WorkflowVersion lifecycle states (docs/02 §2.4). A version is draft
// until published; published is immutable.
const (
	WorkflowVersionDraft      = "draft"
	WorkflowVersionPublished  = "published"
	WorkflowVersionDeprecated = "deprecated"
)

// WorkflowRun lifecycle states (docs/02 §2.4, docs/03 §2):
// pending → running → completed | failed | aborted | paused.
const (
	WorkflowRunPending   = "pending"
	WorkflowRunRunning   = "running"
	WorkflowRunCompleted = "completed"
	WorkflowRunFailed    = "failed"
	WorkflowRunAborted   = "aborted"
	WorkflowRunPaused    = "paused"
)

// StepKind — the five step types (docs/02 §2.4):
//   - task: dispatches a Worker (creates a WorkerExecution)
//   - decision: branches based on a prior step's result
//   - approval: blocks until a human approves (gate)
//   - parallel: fans out to multiple sub-steps, joins on completion
//   - recover: invokes a recovery workflow
const (
	StepKindTask      = "task"
	StepKindDecision  = "decision"
	StepKindApproval  = "approval"
	StepKindParallel  = "parallel"
	StepKindRecover   = "recover"
)

// StepRun lifecycle states (docs/03 §2, docs/09 §3.4):
// pending → ready → running → succeeded | failed | skipped | blocked |
// approval_pending.
const (
	StepRunPending          = "pending"
	StepRunReady            = "ready"
	StepRunRunning          = "running"
	StepRunSucceeded        = "succeeded"
	StepRunFailed           = "failed"
	StepRunSkipped          = "skipped"
	StepRunBlocked          = "blocked"
	StepRunApprovalPending  = "approval_pending"
)

// WorkflowEventType — event kinds streamed via StreamWorkflowEvents
// (docs/07 §3.4, docs/10 §4.1). Step transitions + run lifecycle.
const (
	WorkflowEventRunStarted       = "workflow.run_started"
	WorkflowEventRunCompleted     = "workflow.run_completed"
	WorkflowEventRunFailed        = "workflow.run_failed"
	WorkflowEventRunAborted       = "workflow.run_aborted"
	WorkflowEventStepReady        = "workflow.step_ready"
	WorkflowEventStepStarted      = "workflow.step_started"
	WorkflowEventStepSucceeded    = "workflow.step_succeeded"
	WorkflowEventStepFailed       = "workflow.step_failed"
	WorkflowEventStepSkipped      = "workflow.step_skipped"
	WorkflowEventStepBlocked      = "workflow.step_blocked"
	WorkflowEventStepApproval     = "workflow.step_approval_pending"
)

// AdapterStatus — runtime adapter registration lifecycle
// (docs/04_Runtime_Adapter_SDK.md §2):
// registered → ready → draining → expired
const (
	AdapterRegistered = "registered"
	AdapterReady      = "ready"
	AdapterDraining   = "draining"
	AdapterExpired    = "expired"
)

// ExecutionStatus — WorkerExecution lifecycle
// (docs/02_Domain_Model.md §2.7, docs/03 §6):
// dispatching → running → healthy|stalled|unhealthy → terminating → terminated
const (
	ExecutionDispatching    = "dispatching"
	ExecutionRunning        = "running"
	ExecutionHealthy        = "healthy"
	ExecutionStalled        = "stalled"
	ExecutionUnhealthy      = "unhealthy"
	ExecutionTerminating   = "terminating"
	ExecutionTerminated     = "terminated"
	ExecutionFailedToStart = "failed_to_start"
)

// HealthState — union of heartbeat freshness, progress rate, error rate,
// context-window usage, runtime-reported health (docs/03 §5).
const (
	HealthHealthy     = "healthy"
	HealthStalled     = "stalled"
	HealthUnhealthy   = "unhealthy"
	HealthTerminating = "terminating"
)

// ExecutionEventType — event kinds streamed via StreamExecutionEvents
// (docs/04 §4, docs/07 §3.8).
const (
	ExecEventStarted          = "started"
	ExecEventTelemetry        = "telemetry"
	ExecEventToolCall         = "tool_call"
	ExecEventCheckpoint       = "checkpoint"
	ExecEventApprovalRequest  = "approval_request"
	ExecEventHealth           = "health"
	ExecEventResult           = "result"
	ExecEventError            = "error"
	ExecEventControl          = "control"
)
