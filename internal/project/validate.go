// Package project implements the ProjectService Connect handler
// (docs/07_API_Specification.md §3.1). It is the API-layer boundary
// between the generated Connect handlers and the data-access layer.
//
// Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - map db row types to the generated proto types.
//
// No business logic lives here beyond input validation and lifecycle
// transitions; reconcilers and policy engines govern deeper behavior
// (AGENTS.md invariant #1).
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxNameLen / maxSlugLen bound input size to prevent abuse. Slugs are
// also constrained to URL-safe characters by the slug regex.
const (
	maxNameLen  = 200
	maxSlugLen  = 63
	maxGoalsLen = 1 << 20 // 1 MiB; goals is a JSON document
)

// slugRE defines the canonical slug character set: lowercase alphanumerics
// and hyphens, must start and end alphanumeric. This is the security
// gate that rejects path-traversal or injection-laden slugs before they
// reach the database.
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validateName trims and bounds-checks a project name. An empty name
// after trimming is rejected (InvalidArgument).
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return name, nil
}

// normalizeSlug validates the slug if provided; otherwise derives one
// from the name (lowercased, spaces → hyphens, non-safe chars stripped).
// The result is always slugRE-matched before returning.
func normalizeSlug(slug, name string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = deriveSlug(name)
	}
	if !slugRE.MatchString(slug) {
		return "", fmt.Errorf("slug must match %s", slugRE.String())
	}
	if len(slug) > maxSlugLen {
		return "", fmt.Errorf("slug must be at most %d characters", maxSlugLen)
	}
	return slug, nil
}

// deriveSlug produces a best-effort slug from a name: lowercase, replace
// whitespace runs with single hyphens, drop characters outside [a-z0-9-].
// The result is guaranteed to match slugRE (a fully-stripped name falls
// back to "project" so the unique index never sees an empty slug).
func deriveSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "project"
	}
	return out
}

// validateGoals ensures the goals field is either empty (→ "{}") or valid
// JSON. Rejecting malformed JSON at the API boundary prevents storing
// garbage that the frontend cannot render. Size is bounded to mitigate
// memory-exhaustion abuse.
func validateGoals(goals string) ([]byte, error) {
	goals = strings.TrimSpace(goals)
	if goals == "" {
		return []byte("{}"), nil
	}
	if len(goals) > maxGoalsLen {
		return nil, fmt.Errorf("goals must be at most %d bytes", maxGoalsLen)
	}
	if !json.Valid([]byte(goals)) {
		return nil, errors.New("goals must be valid JSON")
	}
	return []byte(goals), nil
}

// requireTenant resolves the tenant id from the context. The middleware
// (internal/middleware/tenant.go) stores it; if it is missing the
// request was unscoped, which is an internal error (the middleware
// should have rejected it).
func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// statusToProto maps a domain status string to the proto enum value.
// Unknown statuses map to UNSPECIFIED so the API never crashes on a row
// with an unexpected status (defensive — the DB should prevent this).
func statusToProto(status string) int32 {
	switch status {
	case domain.ProjectDrafting:
		return 1
	case domain.ProjectActive:
		return 2
	case domain.ProjectPaused:
		return 3
	case domain.ProjectArchived:
		return 4
	case domain.ProjectDeleted:
		return 5
	default:
		return 0
	}
}

// rowToProto maps a db.ProjectRow to the generated proto Project type.
// Timestamps are converted to timestamppb.
func rowToProto(p db.ProjectRow) *apiv1.Project {
	return &apiv1.Project{
		Id:        p.ID,
		TenantId:  p.TenantID,
		Name:      p.Name,
		Slug:      p.Slug,
		Status:    apiv1.ProjectStatus(statusToProto(p.Status)),
		Goals:     string(p.Goals),
		Version:   int32(p.Version),
		CreatedAt: timestamppb.New(p.CreatedAt),
		UpdatedAt: timestamppb.New(p.UpdatedAt),
	}
}
