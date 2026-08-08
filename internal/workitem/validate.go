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
	"unicode/utf8"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// Input size bounds (AGENTS.md security standards).
const (
	maxTitleLen       = 500
	maxDescLen        = 1 << 20 // 1 MiB — descriptions can be large
	maxBudgetsLen     = 1 << 20
	maxWorkerRefLen   = 1 << 14
	maxProjectIDLen   = 200
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
func ValidateParent(ctx context.Context, tx pgx.Tx, tenantID, parentID, childKind, projectID string) error {
	if parentID == "" {
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
	default:
		return domain.WorkItemPending
	}
}

// ValidateStatus normalizes a domain status string (lowercased, trimmed)
// against the canonical work item statuses. Exported so the Ask Orchicon
// MCP tools validate status the way the Connect path validates its proto
// enum (the tool's status arrives as a raw string, not a proto enum).
func ValidateStatus(status string) (string, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case domain.WorkItemPending, domain.WorkItemScheduled, domain.WorkItemReady,
		domain.WorkItemAssigned, domain.WorkItemRunning, domain.WorkItemCheckpointing,
		domain.WorkItemSucceeded, domain.WorkItemFailed, domain.WorkItemCancelled,
		domain.WorkItemRecovering:
		return status, nil
	default:
		return "", fmt.Errorf("status must be one of pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering")
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
