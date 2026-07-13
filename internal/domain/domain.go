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
	EditLockResourceWorker = "worker"
)
