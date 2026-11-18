package retention

// retention_test.go — the parts of the retention pass that are decisions rather than database calls.
//
// THE VACUUM IS NOT TESTED HERE and cannot be usefully: it needs a live Postgres, and asserting "VACUUM
// ran" would be testing the language's own SQL. What IS tested is the POLICY READ — specifically the
// three-state distinction the whole feature rests on, because getting it wrong is silent:
//
//	disabled                      → prune nothing (the shipped default)
//	enabled, keep = nil           → keep DefaultKeepExecutions
//	enabled, keep = 0             → keep NOTHING (a real, meaningful setting)
//	enabled, keep = N             → keep N
//
// A regression that flattened nil to 0 would delete an operator's entire execution history on the first
// scheduled pass, and nothing about that failure is loud. So it is pinned here.

import (
	"testing"
)

// TestKeepDefaultsAndExplicitZeroAreDistinct pins the distinction between "unset" and "zero" that the
// nullable execution_retention_keep column exists to preserve.
func TestKeepDefaultsAndExplicitZeroAreDistinct(t *testing.T) {
	zero := int64(0)
	forty := int64(40)

	cases := []struct {
		name string
		keep *int64
		want int
	}{
		{"unset falls back to the built-in default", nil, DefaultKeepExecutions},
		{"explicit zero means keep nothing", &zero, 0},
		{"explicit N is honoured", &forty, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKeep(tc.keep)
			if got != tc.want {
				t.Fatalf("resolveKeep(%v) = %d, want %d", tc.keep, got, tc.want)
			}
		})
	}
}

// TestNegativeKeepFallsBackRatherThanDiscardingEverything pins the deliberate choice that a negative keep
// is treated as the default. Clamping it to 0 would do the most destructive thing possible in response to
// nonsense input.
func TestNegativeKeepFallsBackRatherThanDiscardingEverything(t *testing.T) {
	neg := int64(-5)
	if got := resolveKeep(&neg); got != DefaultKeepExecutions {
		t.Fatalf("resolveKeep(-5) = %d, want the default %d (a negative must never mean 'keep nothing')",
			got, DefaultKeepExecutions)
	}
}

// TestVacuumTableListIsSane guards the one place a table name is interpolated into SQL. The list is a
// constant precisely so that interpolation is safe; this fails if that ever stops being true.
func TestVacuumTableListIsSane(t *testing.T) {
	if len(vacuumTables) == 0 {
		t.Fatal("vacuumTables is empty — the pass would prune without ever reclaiming disk")
	}
	seen := map[string]bool{}
	for _, tbl := range vacuumTables {
		if tbl == "" {
			t.Fatal("vacuumTables contains an empty name — that would build the statement 'VACUUM '")
		}
		for _, r := range tbl {
			if !(r == '_' || (r >= 'a' && r <= 'z')) {
				t.Fatalf("vacuumTables entry %q has a character outside [a-z_]; it is interpolated into SQL "+
					"unparameterised, so anything else is an injection risk", tbl)
			}
		}
		if seen[tbl] {
			t.Fatalf("vacuumTables lists %q twice; the second VACUUM would be wasted work", tbl)
		}
		seen[tbl] = true
	}
}
