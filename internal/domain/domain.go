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

// Policy lifecycle states (docs/02 §2.5):
// draft → published → superseded. A published Policy version is immutable.
const (
	PolicyDraft      = "draft"
	PolicyPublished  = "published"
	PolicySuperseded = "superseded"
)

// PolicyVersion lifecycle states (docs/02 §2.5).
const (
	PolicyVersionDraft      = "draft"
	PolicyVersionPublished  = "published"
	PolicyVersionSuperseded = "superseded"
)

// DecisionPoint — Tier 1 lifecycle points where Policy is evaluated
// (docs/02 §2.5). Not every tool call is gated — only these transitions.
const (
	DecisionPointAdmission  = "admission"
	DecisionPointDispatch   = "dispatch"
	DecisionPointBudget     = "budget"
	DecisionPointApproval   = "approval"
	DecisionPointRecovery   = "recovery"
	DecisionPointCompletion = "completion"
)

// PolicyScope — attachment scope (docs/02 §2.5). Narrowest scope wins.
const (
	PolicyScopeTenant  = "tenant"
	PolicyScopeProject = "project"
	PolicyScopeWorker  = "worker"
	PolicyScopeTask    = "task"
)

// PolicyEffect — the decision a Policy asserts (docs/02 §2.5).
const (
	PolicyEffectAllow            = "allow"
	PolicyEffectDeny             = "deny"
	PolicyEffectRequireApproval  = "require_approval"
	PolicyEffectRequireReview    = "require_review"
)

// RecoveryStatus lifecycle (docs/06 §3, §7):
// pending → running → resumed | escalated | failed | cancelled | blocked.
const (
	RecoveryPending    = "pending"
	RecoveryRunning    = "running"
	RecoveryResumed    = "resumed"
	RecoveryEscalated  = "escalated"
	RecoveryFailed     = "failed"
	RecoveryCancelled  = "cancelled"
	RecoveryBlocked    = "blocked"
)

// RecoveryStepStatus lifecycle (docs/06 §3, §9).
const (
	RecoveryStepPending   = "pending"
	RecoveryStepReady     = "ready"
	RecoveryStepRunning   = "running"
	RecoveryStepSucceeded = "succeeded"
	RecoveryStepFailed    = "failed"
	RecoveryStepSkipped   = "skipped"
	RecoveryStepBlocked   = "blocked"
)

// RecoveryStep IDs for the default 6-step recovery workflow
// (docs/06 §3): capture → summarize → preserve → review → plan → resume.
const (
	RecoveryStepCapture   = "capture"
	RecoveryStepSummarize = "summarize"
	RecoveryStepPreserve  = "preserve"
	RecoveryStepReview    = "review"
	RecoveryStepPlan      = "plan"
	RecoveryStepResume    = "resume"
)

// DefaultRecoverySteps is the ordered default recovery workflow
// (docs/06 §3). Organizations may replace any or all of them.
var DefaultRecoverySteps = []string{
	RecoveryStepCapture,
	RecoveryStepSummarize,
	RecoveryStepPreserve,
	RecoveryStepReview,
	RecoveryStepPlan,
	RecoveryStepResume,
}

// RecoveryLevel (docs/06 §7): L0 normal, L1 recovery, L2 recovery-of-
// recovery, L3 human escalation.
const (
	RecoveryLevelL1 int32 = 1
	RecoveryLevelL2 int32 = 2
	RecoveryLevelL3 int32 = 3
)

// ResumptionPath (docs/06 §4): direct checkpoint replay vs full
// summarize-resume. The engine attempts direct replay first and falls
// back on incompatibility.
const (
	ResumptionPathCheckpoint    = "checkpoint"
	ResumptionPathSummarizeResume = "summarize_resume"
)

// PlanStatus for ContinuationPlan (docs/06 §8).
const (
	PlanPending  = "pending"
	PlanApproved = "approved"
	PlanRejected = "rejected"
)

// Bounded auto-relax thresholds (docs/06 §11): recovery may
// automatically increase a Task's budget by up to 25% of the original
// (with an audit event); beyond 150% of the original budget, human
// approval is required.
const (
	BudgetRelaxAutoMaxFraction = 0.25 // +25% automatic
	BudgetRelaxHumanThreshold  = 1.50 // >150% requires human approval
)

// RecoveryEventType — event kinds streamed via StreamRecoveryEvents
// (docs/07 §3.6, docs/06 §11).
const (
	RecoveryEventTriggered      = "recovery.triggered"
	RecoveryEventStepStarted    = "recovery.step.started"
	RecoveryEventStepCompleted  = "recovery.step.completed"
	RecoveryEventStepFailed     = "recovery.step.failed"
	RecoveryEventPlanProduced   = "recovery.plan.produced"
	RecoveryEventPlanApproved   = "recovery.plan.approved"
	RecoveryEventPlanRejected   = "recovery.plan.rejected"
	RecoveryEventEscalated      = "recovery.escalated"
	RecoveryEventResumed        = "recovery.resumed"
	RecoveryEventFailed         = "recovery.failed"
	RecoveryEventCancelled     = "recovery.cancelled"
	RecoveryEventBlocked        = "recovery.blocked"
)

// PolicyEventType — event kinds for the policy.evaluated event
// (docs/08 §4.4).
const (
	PolicyEventEvaluated   = "policy.evaluated"
	PolicyEventPublished   = "policy.published"
	PolicyEventSuperseded  = "policy.superseded"
)
