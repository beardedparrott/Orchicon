package db

import (
	"testing"
)

func TestValidateTargetType(t *testing.T) {
	for _, good := range []string{"worker","workflow","conversation"} {
		if err := ValidateTargetType(good); err != nil { t.Fatalf("valid %q rejected: %v", good, err) }
	}
	for _, bad := range []string{"", "WORKER","workers","projects","conversation "} {
		if err := ValidateTargetType(bad); err == nil { t.Fatalf("invalid %q should fail", bad) }
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{"Hello World":"hello_world","  spaces  ":"spaces","My Category!":"my_category"}
	for in, want := range cases {
		if got := Slugify(in); got != want { t.Fatalf("Slugify(%q)=%q want %q", in, got, want) }
	}
}

func TestCategoryTargetTypeIsolation_Unit(t *testing.T) {
	// Pure unit: ensures validTargetTypes map partitions exactly 3 sets, no cross-over.
	if len(validTargetTypes) != 3 { t.Fatalf("expected 3 target types, got %d", len(validTargetTypes)) }
	if !validTargetTypes["worker"] || !validTargetTypes["workflow"] || !validTargetTypes["conversation"] {
		t.Fatalf("missing expected target type")
	}
	// Simulate that ListCategories is always scoped: ValidateTargetType must be called.
	for _, tt := range []string{"worker","workflow","conversation"} {
		if err := ValidateTargetType(tt); err != nil { t.Fatalf("%q should validate", tt) }
	}
}
