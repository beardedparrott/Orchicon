// Package workflow implements the WorkflowService Connect handler
// (docs/07_API_Specification.md §3.4, docs/02_Domain_Model.md §2.4).
//
// It is the API-layer boundary between the generated Connect handlers
// and the data-access layer. Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - enforce the Workflow lifecycle (draft → published → deprecated)
//     and versioning model (docs/02 §2.4),
//   - manage edit locks for the visual Workflow editor (docs/07 §3.3),
//   - start/abort workflow runs (handed to the WorkflowReconciler).
//
// No business logic lives here beyond input validation and lifecycle
// transitions; the WorkflowReconciler governs step DAG progression and
// gate evaluation (AGENTS.md invariant #1).
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Input size bounds (AGENTS.md security standards: size bounds on all
// inputs to prevent memory-exhaustion abuse).
const (
	maxNameLen            = 200
	maxVersionNoteLen     = 1 << 14
	maxReasonLen          = 1000
	maxActorLen           = 200
	maxStepsLen           = 1 << 20 // 1 MiB — steps JSON (array of Step messages)
	maxJSONFieldLen       = 1 << 20 // 1 MiB for inputs/outputs/run_context
)

// validateName trims and bounds-checks a workflow name.
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

// validateStepsField validates the steps JSON: it must be a JSON array
// (or empty for a fresh draft). Each element is a Step object; the
// shape of each Step is validated by the editor, but here we ensure the
// array is well-formed and bounded (AGENTS.md security standards).
func validateStepsField(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte("[]"), nil
	}
	if len(s) > maxStepsLen {
		return nil, fmt.Errorf("steps must be at most %d bytes", maxStepsLen)
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("steps must be valid JSON")
	}
	// Ensure it's a JSON array.
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("steps must be a JSON array: %w", err)
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
