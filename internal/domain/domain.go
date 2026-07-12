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
