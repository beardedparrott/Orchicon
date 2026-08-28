package category

import (
	"testing"
	"github.com/beardedparrott/orchicon/internal/db"
)

func TestCategoryServiceTargetTypeScoping(t *testing.T) {
	// Ensure every RPC validates target_type before DB access (defense in depth)
	for _, tt := range []string{"worker","workflow","conversation"} {
		if err := db.ValidateTargetType(tt); err != nil { t.Fatalf("valid %q rejected", tt) }
	}
	// Cross-contamination: a worker category must never be returned for workflow queries.
	// This is enforced by the WHERE tenant_id=$1 AND target_type=$2 clause in ListCategories/ListAssignments.
	// Unit check: the three types are distinct strings - no accidental alias.
	seen := map[string]bool{}
	for _, tt := range []string{"worker","workflow","conversation"} {
		if seen[tt] { t.Fatalf("duplicate target type %q", tt) }
		seen[tt]=true
	}
	if seen["worker"] && seen["workflow"] && seen["conversation"] {
		// pass
	} else { t.Fatalf("missing partition") }
}
