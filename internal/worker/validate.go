// Package worker implements the WorkerService Connect handler
// (docs/07_API_Specification.md §3.3, docs/05_Worker_Specification.md).
//
// It is the API-layer boundary between the generated Connect handlers
// and the data-access layer. Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - enforce the Worker lifecycle (draft → published → deprecated →
//     retired) and versioning model (docs/05 §4, §5),
//   - manage edit locks for the visual Worker editor (docs/07 §3.3).
//
// No business logic lives here beyond input validation and lifecycle
// transitions; reconcilers and policy engines govern deeper behavior
// (AGENTS.md invariant #1).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Input size bounds (AGENTS.md security standards: size bounds on all
// inputs to prevent memory-exhaustion abuse).
const (
	maxNameLen        = 200
	maxSlugLen        = 63
	maxDescLen        = 1 << 14 // 16 KiB
	maxPurposeLen     = 1 << 14
	maxPromptLen      = 1 << 20 // 1 MiB — system prompts can be large
	maxJSONFieldLen  = 1 << 20 // 1 MiB for permissions/budgets/labels/etc.
	maxVersionNoteLen = 1 << 14
	maxActorLen       = 200
)

// slugRE defines the canonical slug character set: lowercase alphanumerics
// and hyphens, must start and end alphanumeric. This is the security
// gate that rejects path-traversal or injection-laden slugs before they
// reach the database (mirrors internal/project/validate.go).
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validateName trims and bounds-checks a worker name.
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
// from the name (mirrors internal/project/validate.go).
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

// deriveSlug produces a best-effort slug from a name (mirrors
// internal/project/validate.go).
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
		out = "worker"
	}
	return out
}

// validateTextField trims and bounds-checks a generic text field. Empty
// is allowed (defaults are applied at the DB layer).
func validateTextField(s string, max int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return s, nil
}

// validateJSONField validates a JSON-encoded string field: trims, checks
// size, and verifies valid JSON. Returns the canonical empty value for
// empty input so the DB always stores well-formed JSON (AGENTS.md
// security standards: JSON fields are validated).
func validateJSONField(s, empty, field string, max int) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte(empty), nil
	}
	if len(s) > max {
		return nil, fmt.Errorf("%s must be at most %d bytes", field, max)
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("%s must be valid JSON", field)
	}
	return []byte(s), nil
}

// validateActor trims and bounds-checks the actor field for edit locks.
func validateActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("actor must not be empty")
	}
	if utf8.RuneCountInString(actor) > maxActorLen {
		return "", fmt.Errorf("actor must be at most %d characters", maxActorLen)
	}
	return actor, nil
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
