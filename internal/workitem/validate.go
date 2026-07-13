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
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
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

// validateTitle trims and bounds-checks a work item title.
func validateTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("title must not be empty")
	}
	if utf8.RuneCountInString(title) > maxTitleLen {
		return "", fmt.Errorf("title must be at most %d characters", maxTitleLen)
	}
	return title, nil
}

// validateDescription trims and bounds-checks a description (empty ok).
func validateDescription(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescLen {
		return "", fmt.Errorf("description must be at most %d characters", maxDescLen)
	}
	return s, nil
}

// validateAcceptanceCriteria trims and bounds-checks (empty ok).
func validateAcceptanceCriteria(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescLen {
		return "", fmt.Errorf("acceptance_criteria must be at most %d characters", maxDescLen)
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
	default:
		return domain.WorkItemPending
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

// validateBudgets validates a JSON-encoded budgets field (empty → "{}").
func validateBudgets(s string) ([]byte, error) {
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

// validateWorkerRef validates a JSON-encoded worker ref (worker_id + version).
func validateWorkerRef(s string) ([]byte, error) {
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
