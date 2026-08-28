// Package workitem implements the WorkItemService Connect handler
// (docs/07_API_Specification.md §3.2, docs/02_Domain_Model.md §2.2).
//
// It is the API-layer boundary between the generated Connect handlers
// and the data-access layer. Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - enforce the work hierarchy (Epic → Feature → Task → Subtask,
//     max 4 levels — docs/02 §2.2),
//   - manage dependency edges with cycle rejection at admission
//     (recursive CTE — docs/09 §11),
//   - use optimistic concurrency (CAS) on the version column
//     (docs/09 §5).
//
// No business logic lives here beyond input validation and lifecycle
// transitions (AGENTS.md invariant #1).
package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// Input size bounds (AGENTS.md security standards).
const (
	maxTitleLen     = 500
	maxDescLen      = 1 << 20 // 1 MiB — descriptions can be large
	maxBudgetsLen   = 1 << 20
	maxWorkerRefLen = 1 << 14
	maxProjectIDLen = 200
)

// kindOrder maps a kind to its depth in the hierarchy (1-4). Used to
// enforce max 4 levels (docs/02 §2.2).
var kindOrder = map[string]int{
	domain.WorkItemKindEpic:    1,
	domain.WorkItemKindFeature: 2,
	domain.WorkItemKindTask:    3,
	domain.WorkItemKindSubtask: 4,
}

// validateTopLevelKind rejects a top-level (parentless) work item that is
// not an epic — the hierarchy invariant that only epics are top-level.
func validateTopLevelKind(kind string) error {
	if kind != domain.WorkItemKindEpic {
		return fmt.Errorf("a %s must have a parent; only epics can be top-level", kind)
	}
	return nil
}

// validateKindDepth rejects a parent whose depth is not strictly smaller
// than the child's (a child must be *deeper* than its parent). Because
// depth increases monotonically down the tree, this check alone makes
// reparenting cycle-safe: every ancestor has strictly smaller depth, so a
// node can never be moved under itself or one of its descendants.
func validateKindDepth(childKind, parentKind string) error {
	parentDepth := kindOrder[parentKind]
	childDepth := kindOrder[childKind]
	if childDepth <= parentDepth {
		return fmt.Errorf("a %s must be deeper than its parent (parent is %s)", childKind, parentKind)
	}
	return nil
}

// ValidateParent enforces the work-item hierarchy rules for a parent
// assignment. It is shared by the Create/Update Connect handlers and the
// Ask Orchicon update_work_item tool so the two paths cannot drift
// (AGENTS.md). parentID "" means "no parent" (top-level), which is only
// valid for epics. The parent is loaded inside the tenant transaction, so
// a cross-tenant parent is a NotFound, not a leak. The parent must belong
// to the given (effective) project.
//
// Errors: db.ErrNotFound when the parent id does not exist in the tenant;
// any other error is a plain hierarchy violation (CodeInvalidArgument).
func ValidateParent(ctx context.Context, tx pgx.Tx, tenantID, parentID, childKind, projectID string, recurringOpt ...bool) error {
	if parentID == "" {
		// Flat-recurring items are top-level tasks (kind=task, no parent).
		// They are exempt from the "only epics are top-level" rule; the flat
		// shape (task kind, no parent) is enforced separately by
		// ValidateRecurringFlatness (D1).
		if len(recurringOpt) > 0 && recurringOpt[0] {
			return nil
		}
		return validateTopLevelKind(childKind)
	}
	parent, err := db.GetWorkItem(ctx, tx, tenantID, parentID)
	if err != nil {
		return err
	}
	if parent.ProjectID != projectID {
		return errors.New("parent must be in the same project")
	}
	return validateKindDepth(childKind, parent.Kind)
}

// ValidateTitle trims and bounds-checks a work item title. Exported so the
// Ask Orchicon MCP tools share the API's boundary validation (AGENTS.md: the
// tool and the service cannot drift).
func ValidateTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("title must not be empty")
	}
	if utf8.RuneCountInString(title) > maxTitleLen {
		return "", fmt.Errorf("title must be at most %d characters", maxTitleLen)
	}
	return title, nil
}

// ValidateDescription trims and bounds-checks a description (empty ok).
func ValidateDescription(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescLen {
		return "", fmt.Errorf("description must be at most %d characters", maxDescLen)
	}
	return s, nil
}

// ValidateAcceptanceCriteria trims and bounds-checks (empty ok).
func ValidateAcceptanceCriteria(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescLen {
		return "", fmt.Errorf("acceptance_criteria must be at most %d characters", maxDescLen)
	}
	return s, nil
}

// ValidateAcceptanceReview trims and bounds-checks the acceptance review
// field (empty ok — a work item without a completed run has no review).
// Mirrors ValidateAcceptanceCriteria exactly so the human-written field
// and the reconciler-generated review share one boundary. Exported so the
// Ask Orchicon MCP tools reuse it (AGENTS.md: the tool and the service
// cannot drift).
func ValidateAcceptanceReview(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescLen {
		return "", fmt.Errorf("acceptance_review must be at most %d characters", maxDescLen)
	}
	return s, nil
}

// validateKind returns the domain kind string for a proto enum value,
// or an error if unspecified.
func validateKind(kind apiv1.WorkItemKind) (string, error) {
	switch kind {
	case apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC:
		return domain.WorkItemKindEpic, nil
	case apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE:
		return domain.WorkItemKindFeature, nil
	case apiv1.WorkItemKind_WORK_ITEM_KIND_TASK:
		return domain.WorkItemKindTask, nil
	case apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK:
		return domain.WorkItemKindSubtask, nil
	default:
		return "", errors.New("kind must be one of EPIC, FEATURE, TASK, SUBTASK")
	}
}

// validateStatus returns the domain status string for a proto enum value.
func validateStatus(status apiv1.WorkItemStatus) string {
	switch status {
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING:
		return domain.WorkItemPending
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY:
		return domain.WorkItemReady
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED:
		return domain.WorkItemAssigned
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING:
		return domain.WorkItemRunning
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_CHECKPOINTING:
		return domain.WorkItemCheckpointing
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED:
		return domain.WorkItemSucceeded
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED:
		return domain.WorkItemFailed
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED:
		return domain.WorkItemCancelled
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECOVERING:
		return domain.WorkItemRecovering
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED:
		return domain.WorkItemScheduled
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING:
		return domain.WorkItemRecurring
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_BLOCKED:
		return domain.WorkItemBlocked
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED:
		return domain.WorkItemSkipped
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED:
		return domain.WorkItemArchived
	case apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA:
		return domain.WorkItemIdea
	default:
		return domain.WorkItemPending
	}
}

// ValidateStatus normalizes a domain status string (lowercased, trimmed)
// against the canonical work item statuses. Exported so the Ask Orchicon
// MCP tools validate status the way the Connect path validates its proto
// enum (the tool's status arrives as a raw string, not a proto enum).
//
// "blocked" is DELIBERATELY not accepted: it is a system-managed status set
// only by the reconcilers when an upstream dependency is unsatisfied, so
// user/operator input must never set it directly (mirrors the transition
// matrix in the frontend — blocked items are not manually movable).
// "skipped" is likewise system-managed (set only by the reconciler when a
// bound run completes with a skipped step) and is not user-assignable.
func ValidateStatus(status string) (string, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case domain.WorkItemPending, domain.WorkItemScheduled, domain.WorkItemReady,
		domain.WorkItemAssigned, domain.WorkItemRunning, domain.WorkItemCheckpointing,
		domain.WorkItemSucceeded, domain.WorkItemFailed, domain.WorkItemCancelled,
		domain.WorkItemRecovering, domain.WorkItemRecurring:
		return status, nil
	default:
		return "", fmt.Errorf("status must be one of pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering, recurring")
	}
}

// IsActiveRunStatus reports whether a work item is currently bound to an
// in-flight workflow run (running / checkpointing / recovering). These are
// the statuses ScheduledRunReconciler must never re-arm against, and the
// statuses the Schedules "Running" view shows (ADR-001/002 in
// architecture-notes/running-workflows-not-showing-in-schedules.md).
// Exported so the Ask Orchicon MCP tools apply the same scheduled-status
// flip guard as the Connect Update handler.
func IsActiveRunStatus(status string) bool {
	switch status {
	case domain.WorkItemRunning, domain.WorkItemCheckpointing, domain.WorkItemRecovering:
		return true
	default:
		return false
	}
}

// IsIdeaStatus reports whether a work item status is the system-managed
// idea state (feature 4.1/5.1). An idea is an automation-produced item
// awaiting triage; it is excluded from every normal work-item view and may
// only leave idea state via PromoteIdea (→ pending) or DismissIdea
// (→ cancelled). Exported so the Ask Orchicon MCP tools apply the identical
// not-idea gate as the Connect handlers (AGENTS.md: the tool and the
// service cannot drift).
func IsIdeaStatus(status string) bool {
	return status == domain.WorkItemIdea
}

// ErrWorkItemIsIdea is the shared guidance returned when a generic
// mutation path (UpdateWorkItem / DeleteWorkItem / HardDeleteWorkItem /
// ArchiveWorkItem, or the corresponding Ask Orchicon tools) targets an
// idea-state work item. Exported so the service handlers wrap it in a
// FailedPrecondition error and the MCP tools surface the same message.
func ErrWorkItemIsIdea() error {
	return errors.New("idea is a system-managed status scoped to the Idea Cloud; act on an idea only via PromoteIdea (approve) or DismissIdea (discard)")
}

// IsStartableForAutoStart reports whether UPDATE-path auto-start may fire
// for a work item in this status. Only pre-run statuses may be armed by an
// edit: pending, scheduled, ready, assigned. Every other status (running,
// checkpointing, recovering, succeeded, failed, cancelled, skipped,
// recurring, blocked, archived) is declined — an edit must never resurrect
// or duplicate work, no matter what stored auto_start_workflow flag says.
//
// This is the primary fix for stale legacy rows carrying
// auto_start_workflow=true (created before migration
// 20260807120000_work_item_auto_start_default_false.sql): with this gate,
// such a row is behaviorally inert on the update path even when the edit
// touches workflow_id. Exported so the Ask Orchicon MCP update tool applies
// the identical precondition (AGENTS.md: the tool and the service cannot
// drift). The CREATE path needs no gate — new items always start pending.
func IsStartableForAutoStart(status string) bool {
	switch status {
	case domain.WorkItemPending, domain.WorkItemScheduled,
		domain.WorkItemReady, domain.WorkItemAssigned:
		return true
	default:
		return false
	}
}

// AutoStartDeclinedWarning returns the user-facing explanation carried by
// UpdateWorkItemResponse.warning and the Ask Orchicon update tool result
// when an EXPLICIT auto_start_workflow=true was declined because the
// item's status is not startable. Shared verbatim so the two surfaces
// never drift.
func AutoStartDeclinedWarning(status string) string {
	return fmt.Sprintf("Auto-start was NOT applied: this item's status is %q. "+
		"Start-immediately-on-save requires the item to be pending, scheduled, ready, or assigned. "+
		"Your changes were saved.", status)
}

// validateDependencyType returns the domain type for a proto enum.
func validateDependencyType(t apiv1.DependencyType) (string, error) {
	switch t {
	case apiv1.DependencyType_DEPENDENCY_TYPE_BLOCKS:
		return domain.DependencyBlocks, nil
	case apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON:
		return domain.DependencyDependsOn, nil
	case apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO:
		return domain.DependencyRelatesTo, nil
	default:
		return "", errors.New("type must be one of BLOCKS, DEPENDS_ON, RELATES_TO")
	}
}

// ValidateBudgets validates a JSON-encoded budgets field (empty → "{}").
func ValidateBudgets(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte("{}"), nil
	}
	if len(s) > maxBudgetsLen {
		return nil, fmt.Errorf("budgets must be at most %d bytes", maxBudgetsLen)
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("budgets must be valid JSON")
	}
	return []byte(s), nil
}

// ValidateWorkerRef validates a JSON-encoded worker ref (worker_id + version).
func ValidateWorkerRef(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil // nil = unassign
	}
	if len(s) > maxWorkerRefLen {
		return nil, fmt.Errorf("worker_ref must be at most %d bytes", maxWorkerRefLen)
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("worker_ref must be valid JSON")
	}
	return []byte(s), nil
}

// requireTenant resolves the tenant id from the context (mirrors
// internal/project/validate.go).
func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// ValidateSequenceSubtree performs the schedule-time / run-instant
// validation for a parent work item with children (architecture-notes/
// sequential-multi-workflow-runs.md §3). It walks the full subtree and
// rejects outright — nothing starts when any offender is found:
//
//   - noWorkflow: every descendant that must actually execute — every
//     LEAF — must have a non-empty workflow_id bound. Container children
//     with their own children arm nested sequences and are exempt, but
//     their descendants are validated recursively.
//   - oneShot: no worker-assigned (one-shot) child may exist anywhere in
//     the subtree. One-shots run through the standalone ready/assigned
//     path and remain available for standalone tasks only.
//   - badWorkflow: a leaf's bound workflow must resolve to a published or
//     deprecated workflow (the StartWorkflow precondition) so a fire-time
//     failure can't occur — reject at schedule time instead. A bound
//     workflow whose current version has an EMPTY step DAG is rejected too:
//     a run started on it can never progress (the reconciler fails it at
//     start), so arming the child would strand the chain at fire time.
//
// The walk is tenant-scoped and depth-bounded (max 4 levels). Shared by
// the Connect UpdateWorkItem paths and the Ask Orchicon tools so the two
// surfaces cannot drift (AGENTS.md Ask-Orchicon-sync rule).
func ValidateSequenceSubtree(ctx context.Context, tx pgx.Tx, tenantID string, parent db.WorkItemRow) (noWorkflow, oneShot, badWorkflow []string, err error) {
	var walk func(id string) error
	walk = func(id string) error {
		children, err := db.ListDirectChildren(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		for _, c := range children {
			if len(c.AssignedWorkerRef) > 0 {
				oneShot = append(oneShot, c.Title)
			}
			grandchildren, err := db.ListDirectChildren(ctx, tx, tenantID, c.ID)
			if err != nil {
				return err
			}
			if len(grandchildren) == 0 {
				// Leaf — must execute → must have a runnable workflow.
				if c.WorkflowID == nil || *c.WorkflowID == "" {
					noWorkflow = append(noWorkflow, c.Title)
				} else {
					wf, werr := db.GetWorkflow(ctx, tx, tenantID, *c.WorkflowID)
					if werr != nil || (wf.Status != domain.WorkflowPublished && wf.Status != domain.WorkflowDeprecated) {
						badWorkflow = append(badWorkflow, c.Title)
					} else if !workflowHasSteps(ctx, tx, tenantID, wf) {
						badWorkflow = append(badWorkflow, c.Title)
					}
				}
			} else if err := walk(c.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(parent.ID); err != nil {
		return nil, nil, nil, err
	}
	return noWorkflow, oneShot, badWorkflow, nil
}

// BuildSequenceValidationError composes the schedule-time rejection message
// for a sequence parent. For the "no workflow set" case it matches the
// design contract exactly:
//
//	> Cannot schedule "Parent Title": 2 children have no workflow set — "Feature A", "Task C". Bind workflows or remove them from the sequence.
func BuildSequenceValidationError(parentTitle string, noWorkflow, oneShot, badWorkflow []string) error {
	var parts []string
	if len(noWorkflow) > 0 {
		noun, verb := "child", "has"
		if len(noWorkflow) != 1 {
			noun, verb = "children", "have"
		}
		parts = append(parts, fmt.Sprintf("%d %s %s no workflow set — %s", len(noWorkflow), noun, verb, quoteTitles(noWorkflow)))
	}
	if len(badWorkflow) > 0 {
		noun, verb := "child", "has"
		if len(badWorkflow) != 1 {
			noun, verb = "children", "have"
		}
		parts = append(parts, fmt.Sprintf("%d %s %s a workflow that is not runnable — %s", len(badWorkflow), noun, verb, quoteTitles(badWorkflow)))
	}
	if len(oneShot) > 0 {
		parts = append(parts, fmt.Sprintf("%s worker-assigned (one-shot) and cannot run in a sequence", quoteTitles(oneShot)))
	}
	msg := fmt.Sprintf("Cannot schedule %q: %s.", parentTitle, strings.Join(parts, "; "))
	if len(noWorkflow) > 0 || len(badWorkflow) > 0 {
		msg += " Bind workflows or remove them from the sequence."
	} else if len(oneShot) > 0 {
		msg += " Remove them from the chain."
	}
	return errors.New(msg)
}

// quoteTitles renders a list of offending child titles as "A", "B".
func quoteTitles(titles []string) string {
	quoted := make([]string, 0, len(titles))
	for _, t := range titles {
		quoted = append(quoted, fmt.Sprintf("%q", t))
	}
	return strings.Join(quoted, ", ")
}

// workflowHasSteps reports whether the workflow's current version carries
// at least one step. A workflow whose published version has an EMPTY step
// DAG produces a run that can never progress — the reconciler fails such a
// run at start — so a sequence leaf bound to one would strand its parent's
// chain at fire time. Schedule-time validation rejects it up front.
func workflowHasSteps(ctx context.Context, tx pgx.Tx, tenantID string, wf db.WorkflowRow) bool {
	version, err := db.GetWorkflowVersion(ctx, tx, tenantID, wf.ID, wf.CurrentVersion)
	if err != nil {
		return false
	}
	var steps []json.RawMessage
	if len(version.Steps) > 0 {
		if err := json.Unmarshal(version.Steps, &steps); err != nil {
			return false
		}
	}
	return len(steps) > 0
}

// validFrequencies is the set of allowed recurring schedule frequencies.
var validFrequencies = map[string]bool{
	"minute":  true,
	"hourly":  true,
	"daily":   true,
	"weekly":  true,
	"monthly": true,
}

// validDays is the set of allowed day names for weekly schedules.
var validDays = map[string]bool{
	"Mon": true, "Tue": true, "Wed": true, "Thu": true,
	"Fri": true, "Sat": true, "Sun": true,
}

// recurringScheduleJSON is the JSON shape stored in the recurring_schedule
// JSONB column. It mirrors the proto RecurringSchedule for DB round-tripping.
type recurringScheduleJSON struct {
	Frequency   string   `json:"frequency"`
	Interval    int      `json:"interval"`
	Days        []string `json:"days"`
	StartDate   string   `json:"start_date"`
	StartTime   string   `json:"start_time"`
	OutputsMode string   `json:"outputs_mode"`
}

// IsRecurringScheduleEmpty reports whether a non-nil RecurringSchedule has all
// zero-value fields, which the API contract treats as "clear the schedule".
func IsRecurringScheduleEmpty(msg *apiv1.RecurringSchedule) bool {
	if msg == nil {
		return false
	}
	return strings.TrimSpace(msg.Frequency) == "" &&
		msg.Interval == 0 &&
		len(msg.Days) == 0 &&
		strings.TrimSpace(msg.StartDate) == "" &&
		strings.TrimSpace(msg.StartTime) == ""
}

// ValidateRecurringSchedule validates a proto RecurringSchedule message and
// returns its JSONB representation. Returns nil, nil when the input is nil
// (not recurring). Exported so the Ask Orchicon MCP tools share the API's
// boundary validation (AGENTS.md Ask-Orchicon-sync rule).
func ValidateRecurringSchedule(msg *apiv1.RecurringSchedule) ([]byte, error) {
	if msg == nil {
		return nil, nil
	}
	freq := strings.ToLower(strings.TrimSpace(msg.Frequency))
	if !validFrequencies[freq] {
		return nil, fmt.Errorf("recurring_schedule.frequency must be one of minute, hourly, daily, weekly, monthly; got %q", msg.Frequency)
	}
	if msg.Interval < 1 {
		return nil, fmt.Errorf("recurring_schedule.interval must be >= 1; got %d", msg.Interval)
	}
	days := make([]string, 0, len(msg.Days))
	for _, d := range msg.Days {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !validDays[d] {
			return nil, fmt.Errorf("recurring_schedule.days contains invalid day %q; must be one of Mon, Tue, Wed, Thu, Fri, Sat, Sun", d)
		}
		days = append(days, d)
	}
	startDate := strings.TrimSpace(msg.StartDate)
	if startDate == "" {
		return nil, errors.New("recurring_schedule.start_date must not be empty")
	}
	if _, err := time.Parse("2006-01-02", startDate); err != nil {
		return nil, fmt.Errorf("recurring_schedule.start_date must be YYYY-MM-DD; got %q", startDate)
	}
	startTime := strings.TrimSpace(msg.StartTime)
	if startTime == "" {
		return nil, errors.New("recurring_schedule.start_time must not be empty")
	}
	if _, err := time.Parse("15:04", startTime); err != nil {
		return nil, fmt.Errorf("recurring_schedule.start_time must be HH:MM; got %q", startTime)
	}
	schedule := recurringScheduleJSON{
		Frequency:   freq,
		Interval:    int(msg.Interval),
		Days:        days,
		StartDate:   startDate,
		StartTime:   startTime,
		OutputsMode: domain.NormalizeRecurringOutputsMode(msg.OutputsMode),
	}
	b, err := json.Marshal(schedule)
	if err != nil {
		return nil, fmt.Errorf("recurring_schedule: marshal: %w", err)
	}
	return b, nil
}

// ValidateRecurringFlatness enforces the flat-recurring invariant (D1): a
// recurring item is a flat top-level task with no parent and no hierarchy
// participation. kind is only "task" and parent_id is empty. Returns nil
// when the item is NOT recurring. Exported so the Ask Orchicon tools share
// the API's boundary validation (AGENTS.md Ask-Orchicon-sync rule).
func ValidateRecurringFlatness(recurring bool, kind, parentID string) error {
	if !recurring {
		return nil
	}
	if kind != domain.WorkItemKindTask {
		return fmt.Errorf("a recurring work item must be flat (kind=task); got kind %q", kind)
	}
	if parentID != "" {
		return errors.New("a recurring work item must be flat and cannot have a parent")
	}
	return nil
}

// ValidateRecurringWorkflowBinding enforces D7: a schedulable recurring
// LEAF must be bound to a workflow to be schedulable (the recurring fire has
// nothing to start for a workflow-less leaf). A recurring item WITH children
// is a sequence parent — it needs no workflow binding (the sequence
// reconciler fires its children). The rejection is result-state based: it
// applies to the resulting state (recurring && no children && no workflow)
// only, so a legacy recurring leaf can still be edited to bind a workflow,
// and a sequence parent remains valid without one. Exported so the Ask
// Orchicon tools share the API's boundary validation.
func ValidateRecurringWorkflowBinding(recurring, hasChildren bool, workflowID string) error {
	if recurring && !hasChildren && workflowID == "" {
		return errors.New("a recurring work item must be bound to a workflow to be schedulable; bind a workflow to this item")
	}
	return nil
}

// ComputeNextRunAt computes the first occurrence of a recurring schedule
// that is >= the given "now" time. The start_date + start_time define the
// anchor; the frequency/interval/days define the cadence. Returns nil when
// the schedule is nil (not recurring).
func ComputeNextRunAt(schedule *apiv1.RecurringSchedule, now time.Time) *time.Time {
	if schedule == nil {
		return nil
	}
	startDate := strings.TrimSpace(schedule.StartDate)
	startTime := strings.TrimSpace(schedule.StartTime)
	anchor, err := time.Parse("2006-01-02 15:04", startDate+" "+startTime)
	if err != nil {
		return nil
	}
	freq := strings.ToLower(strings.TrimSpace(schedule.Frequency))
	interval := int(schedule.Interval)
	if interval < 1 {
		interval = 1
	}

	// For weekly frequency, walk day-by-day so we hit every selected
	// weekday within each cadence week. Empty days means "every day"
	// (all 7 weekdays are valid). The previous approach (7*interval-day
	// jumps from the anchor) only ever landed on the anchor's weekday,
	// skipping all other selected days.
	if freq == "weekly" {
		cadenceDays := 7 * interval
		anchorWeekday := int(anchor.Weekday())
		// Precompute valid day offsets from the anchor's weekday.
		// For each selected day, the offset is the number of days after
		// the anchor's weekday within the same cadence week.
		validOffsets := make(map[int]bool)
		if len(schedule.Days) == 0 {
			// Empty days = every day of the week.
			for i := 0; i < 7; i++ {
				validOffsets[i] = true
			}
		} else {
			for _, d := range schedule.Days {
				wd := weekdayOffset(d)
				offset := (wd - anchorWeekday + 7) % 7
				validOffsets[offset] = true
			}
		}
		candidate := anchor
		for i := 0; i < 1000; i++ {
			if !candidate.Before(now) {
				daysSinceAnchor := int(candidate.Sub(anchor).Hours() / 24)
				if daysSinceAnchor >= 0 && validOffsets[daysSinceAnchor%cadenceDays] {
					return &candidate
				}
			}
			candidate = candidate.AddDate(0, 0, 1)
		}
		// Fallback: return the anchor even though it may be in the past.
		return &anchor
	}

	// Walk forward from the anchor until we find a time >= now.
	// Cap at 1000 iterations to prevent infinite loops on degenerate inputs.
	candidate := anchor
	for i := 0; i < 1000; i++ {
		if !candidate.Before(now) {
			return &candidate
		}
		candidate = advanceTime(candidate, freq, interval)
	}
	// Fallback: return the anchor even though it's in the past.
	return &anchor
}

// dayMatches reports whether the given time's weekday is in the allowed set.
func dayMatches(t time.Time, days []string) bool {
	weekday := t.Weekday().String()[:3] // "Mon", "Tue", etc.
	for _, d := range days {
		if d == weekday {
			return true
		}
	}
	return false
}

// weekdayOffset returns Go's native weekday number (0=Sun, 1=Mon, … 6=Sat)
// for a three-letter day name. This matches time.Weekday so offset
// calculations against anchor.Weekday() are consistent.
func weekdayOffset(day string) int {
	switch day {
	case "Sun":
		return 0
	case "Mon":
		return 1
	case "Tue":
		return 2
	case "Wed":
		return 3
	case "Thu":
		return 4
	case "Fri":
		return 5
	case "Sat":
		return 6
	default:
		return 0
	}
}

// advanceTime moves the candidate forward by one step of the given frequency.
func advanceTime(t time.Time, freq string, interval int) time.Time {
	switch freq {
	case "minute":
		return t.Add(time.Duration(interval) * time.Minute)
	case "hourly":
		return t.Add(time.Duration(interval) * time.Hour)
	case "daily":
		return t.AddDate(0, 0, interval)
	case "weekly":
		return t.AddDate(0, 0, 7*interval)
	case "monthly":
		return t.AddDate(0, interval, 0)
	default:
		return t.AddDate(0, 0, interval)
	}
}
// ValidateSecretIDs validates per-work-item secret selection: max 10, no empty, no duplicates.
func ValidateSecretIDs(ids []string) error {
	if len(ids) > 10 {
		return fmt.Errorf("too many secrets: max 10, got %d", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("secret id must not be empty")
		}
		if seen[id] {
			return fmt.Errorf("duplicate secret id %q", id)
		}
		seen[id] = true
	}
	return nil
}
