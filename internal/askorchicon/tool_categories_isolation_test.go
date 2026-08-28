package askorchicon

import (
	"testing"
	"github.com/beardedparrott/orchicon/internal/db"
)

func TestToolCategoriesTargetTypeValidation(t *testing.T) {
	for _, tt := range []string{"worker","workflow","conversation"} {
		if err := db.ValidateTargetType(tt); err != nil { t.Fatalf("valid %q rejected: %v", tt, err) }
	}
	// Ensure tool registry documents target_type as enum (prevents crossover in UI)
	if err := db.ValidateTargetType("workers"); err == nil {
		t.Fatalf("workers (plural) should be rejected - prevents worker/workflow crossover")
	}
}
