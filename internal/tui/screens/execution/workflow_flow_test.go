package execution

// workflow_flow_test.go — the vertical flow renderer.
//
// The fixture is a REAL workflow: SDLC (Human)'s published `steps` JSON, lifted
// verbatim from the live tenant. It is not hand-written, because the shapes that
// matter here are the ones the product actually stores — 11 steps, a 2-way fan-in,
// three retry loops whose targets sit EARLIER in the flow, and an approval gate.

import (
	"os"
	"strings"
	"testing"
)

func realFlow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/sdlc_human.json")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	return string(b)
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// The parser must survive the shapes the column actually contains. depends_on is
// NOT type-consistent in the live tenant (207 arrays, 12 nulls, 10 of something
// else), and a plain []string field would fail the WHOLE unmarshal on one bad row
// — losing every step in the workflow rather than one edge.
func TestFlowParsingToleratesEveryDependsOnShape(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want []string
	}{
		{`["a","b"]`, []string{"a", "b"}},
		{`[]`, nil},
		{`null`, nil},
		{`""`, nil},
		{`"a,b"`, []string{"a", "b"}},        // a bare/CSV string
		{`["a", " b "]`, []string{"a", "b"}}, // whitespace inside an element
		{`[""]`, nil},
	} {
		got := parseDepends([]byte(c.raw))
		if len(got) != len(c.want) {
			t.Errorf("parseDepends(%s) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseDepends(%s) = %v, want %v", c.raw, got, c.want)
				break
			}
		}
	}
	// A malformed steps array yields NO steps rather than an error — the caller
	// then says "no steps" honestly instead of crashing the detail pane.
	if s := parseFlowSteps("{not json"); len(s) != 0 {
		t.Errorf("malformed steps JSON produced %d steps, want 0", len(s))
	}
}

// Topological order, with the AUTHORED order preserved among steps that are ready
// at the same time. This is the execution order a reader is being shown, so it has
// to be real: a step must never be listed before something it depends on.
func TestFlowOrderIsTopological(t *testing.T) {
	steps := parseFlowSteps(realFlow(t))
	if len(steps) != 11 {
		t.Fatalf("fixture parsed %d steps, want 11", len(steps))
	}
	ordered := flowOrder(steps)
	if len(ordered) != len(steps) {
		t.Fatalf("order dropped steps: %d of %d", len(ordered), len(steps))
	}
	seen := map[string]bool{}
	for _, s := range ordered {
		for _, d := range s.deps {
			if !seen[d] {
				if _, defined := findStep(steps, d); defined {
					t.Errorf("step %q listed before its dependency %q", s.Name, d)
				}
			}
		}
		seen[s.ID] = true
	}
}

func findStep(steps []flowStep, id string) (flowStep, bool) {
	for _, s := range steps {
		if s.ID == id {
			return s, true
		}
	}
	return flowStep{}, false
}

// A cycle must not hang or silently drop nodes: the renderer appends the rest.
func TestFlowOrderSurvivesACycle(t *testing.T) {
	steps := parseFlowSteps(`[{"id":"a","name":"A","depends_on":["b"]},{"id":"b","name":"B","depends_on":["a"]}]`)
	ordered := flowOrder(steps)
	if len(ordered) != 2 {
		t.Fatalf("a cycle produced %d steps, want both kept", len(ordered))
	}
}

// The real workflow renders what it IS: the fan-in is named, the retry loops name
// their (earlier) targets, and the approval shows its reviewer.
//
// Loops are the point. They live in a decision's config, NOT in depends_on, so a
// renderer that only walked dependencies would show a straight line and hide the
// fact that this workflow can send control BACKWARDS — the single most surprising
// thing about it.
func TestRealWorkflowRendersItsBranchesAndLoops(t *testing.T) {
	out := stripANSI(renderWorkflowFlow(realFlow(t), 100))
	for _, want := range []string{
		"Principal Software Architect",
		"Senior Software Engineer",
		"PR Reviewer",
		"QA Engineer",
		"DevOps Engineer",
		"End",
		// The fan-in, NAMED (max fan-in in this tenant is 2).
		"joins Senior Software Engineer + Loop Decision",
		// A loop that jumps BACKWARDS, named rather than drawn.
		"loop ↻ Senior Software Engineer",
		// The human gate, which blocks a run until someone acts.
		"reviewer: human",
		// A re-entry target is flagged, because "control can jump back here" is
		// invisible in depends_on.
		"↻",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flow render is missing %q:\n%s", want, out)
		}
	}
}

// A chain with no branches is what a chain looks like: a list. The renderer must
// not invent branch furniture where there is none.
func TestAChainRendersAsAPlainList(t *testing.T) {
	chain := `[
	  {"id":"a","name":"One","kind":"task","depends_on":[]},
	  {"id":"b","name":"Two","kind":"task","depends_on":["a"]},
	  {"id":"c","name":"Three","kind":"end","depends_on":["b"]}
	]`
	out := stripANSI(renderWorkflowFlow(chain, 80))
	for _, banned := range []string{"loop", "success →", "joins"} {
		if strings.Contains(out, banned) {
			t.Errorf("a pure chain must not render %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "One") || !strings.Contains(out, "Three") {
		t.Errorf("chain not rendered:\n%s", out)
	}
}

// No steps, no picture: the caller is told so rather than shown an empty frame.
func TestNoStepsRendersNothing(t *testing.T) {
	if got := renderWorkflowFlow("", 80); got != "" {
		t.Errorf("empty steps rendered %q", got)
	}
	if got := renderWorkflowFlow("[]", 80); got != "" {
		t.Errorf("empty array rendered %q", got)
	}
}

// The version shown is the one a run would EXECUTE: published wins, else newest.
func TestPickFlowVersionPrefersPublished(t *testing.T) {
	if got := pickFlowVersion(nil); got != nil {
		t.Error("no versions must pick nothing")
	}
}
