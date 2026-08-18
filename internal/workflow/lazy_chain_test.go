package workflow

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestLazyConflictChainStepIDs verifies that the lazy-step classification
// only flags steps that are wired EXCLUSIVELY through a loop_decision gate's
// conflict_chain (free-floating, no static depends_on edge): those must never
// be seeded or dispatched on the first clean pass. A step that is both in a
// conflict_chain and referenced by a static depends_on edge is a normal DAG
// member and must NOT be classified lazy.
func TestLazyConflictChainStepIDs(t *testing.T) {
	cases := []struct {
		name  string
		steps []StepWire
		want  map[string]bool
	}{
		{
			name: "free-floating integrator in conflict_chain is lazy",
			steps: []StepWire{
				{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{}},
				{ID: "gate", Name: "Merge Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{"merge"},
					Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
				{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
			},
			want: map[string]bool{"integrator": true},
		},
		{
			name: "conflict_chain step that is also a static dep is not lazy",
			steps: []StepWire{
				{ID: "approval", Name: "Approval", Kind: "task", DependsOn: []string{}},
				{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{"approval"}},
				{ID: "gate", Name: "Merge Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{"merge"},
					Config: `{"conflict_chain":["integrator","merge"]}`},
				{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
			},
			// merge is a static dep of nothing but appears in the chain and
			// IS depended on by the gate — but the gate's depends_on makes
			// it static; integrator is the only free-floating lazy step.
			want: map[string]bool{"integrator": true},
		},
		{
			name: "no conflict_chain means no lazy steps",
			steps: []StepWire{
				{ID: "a", Name: "A", Kind: "task", DependsOn: []string{}},
				{ID: "gate", Name: "Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{"a"},
					Config: `{"loop_branch":"a"}`},
			},
			want: nil,
		},
		{
			name:  "empty steps",
			steps: nil,
			want:  nil,
		},
		{
			name: "malformed gate config is ignored",
			steps: []StepWire{
				{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
				{ID: "gate", Name: "Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{},
					Config: `{not json`},
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LazyConflictChainStepIDs(tc.steps)
			if len(got) != len(tc.want) {
				t.Fatalf("LazyConflictChainStepIDs() = %v, want %v", got, tc.want)
			}
			for id := range tc.want {
				if !got[id] {
					t.Errorf("missing lazy step %q in %v", id, got)
				}
			}
		})
	}
}
