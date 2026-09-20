package askorchicon

// modegate_test.go — THE BOUNDARY IS ENFORCED BY THE PLATFORM, NOT ASKED FOR.
//
// The operator: "Each mode of Ask Orchicon must NEVER just do the work it is not supposed to do and must always
// enforce the user switch the mode first no matter what the user says."
//
// The table below is the boundary, and the integration test is the part that matters: it drives the REAL tool
// choke point (ExecuteAskTool) with a mode-stamped context, so a future change that moves the gate, reorders it
// behind a branch, or loses the context stamp fails here rather than silently removing the boundary.

import (
	"context"
	"strings"
	"testing"
)

// THE WORK TOOLS ARE REFUSED BY EVERY MODE THAT IS NOT THE HANDS.
func TestBrainstormAndQuickWorkRefuseTheWork(t *testing.T) {
	for _, mode := range []string{modeBrainstorm, modeQuickWork} {
		for _, tool := range []string{"write", "edit", "batch_write", "bash"} {
			ok, refusal := modeAllowsTool(mode, tool)
			if ok {
				t.Errorf("%s may run %q — the action phase is Iteration's job and this is the whole boundary",
					mode, tool)
				continue
			}
			// The refusal has to be USABLE: it names the tool, says the PLATFORM refused it, and names the one
			// action that resolves it — the user switching.
			for _, want := range []string{tool, "REFUSED BY THE PLATFORM", "Iteration", "switch your own mode", "ASK THE USER"} {
				if !strings.Contains(refusal, want) {
					t.Errorf("%s/%s refusal is missing %q: %s", mode, tool, want, refusal)
				}
			}
		}
	}
}

// AND THEY KEEP EVERYTHING THAT LETS THEM UNDERSTAND THE PROJECT.
//
// The operator's condition on denying bash: "That is fine as long as Brainstorm mode can learn about the project
// and read the files." So the read-only suite is asserted positively — a gate that denied too much would satisfy
// the boundary and destroy the mode.
func TestTheReadOnlySuiteSurvivesInEveryMode(t *testing.T) {
	reads := []string{
		"read", "batch_read", "grep", "batch_grep", "glob", "list",
		"list_project_dir", "read_project_file", "ask_file_root",
	}
	for _, mode := range everyMode {
		for _, tool := range reads {
			if ok, refusal := modeAllowsTool(mode, tool); !ok {
				t.Errorf("%s may not run %q: %s — the mode could no longer read the project it is meant to "+
					"understand", mode, tool, refusal)
			}
		}
	}
}

// ITERATION MAY NOT AUTHOR OR DISPATCH THE PLAN.
//
// The operator, on the one they cared about: "Yes it is a hard block on create work item. That is brainstorm's
// job or ephemeral work items for quick_work mode."
func TestIterationRefusesThePlanAndTheDispatch(t *testing.T) {
	for _, tool := range []string{
		"create_work_item", "update_work_item",
		"schedule_work_item", "reorder_work_items", "control_sequence",
		"force_progress_workflow_run", "retry_failed_workflow_run",
	} {
		if ok, _ := modeAllowsTool(modeIteration, tool); ok {
			t.Errorf("iteration may run %q — writing the plan or firing the pipeline is not this mode's job", tool)
		}
	}
	// AND IT KEEPS ITS HANDS. A gate that stopped Iteration editing code would leave the mode with nothing.
	for _, tool := range []string{"write", "edit", "batch_write", "bash"} {
		if ok, refusal := modeAllowsTool(modeIteration, tool); !ok {
			t.Errorf("iteration may not run %q: %s — that is the ONE thing this mode is for", tool, refusal)
		}
	}
	// LIFECYCLE IS DELIBERATELY ALLOWED, and pinned so the exception is a decision rather than an oversight:
	// archiving or restoring is hygiene, not authoring or dispatch, and blocking it could strand a mistake.
	for _, tool := range []string{"archive_work_item", "restore_work_item", "delete_work_item"} {
		if ok, _ := modeAllowsTool(modeIteration, tool); !ok {
			t.Errorf("iteration may not run %q — lifecycle hygiene is deliberately outside the boundary", tool)
		}
	}
}

// ALL THE OTHER ORCHICON TOOLS STAY AVAILABLE TO EVERY MODE, which is the additive-but-not-destructive rule:
// the gate refuses a listed set and has no opinion about anything else.
func TestTheGateHasNoOpinionBeyondItsTable(t *testing.T) {
	for _, mode := range everyMode {
		for _, tool := range []string{
			"list_projects", "get_project", "create_project", "list_work_items", "get_work_item",
			"list_workers", "list_workflows", "create_worker", "create_workflow", "list_executions",
			"build_runtime_image", "install_mcp_server", "create_project_directory",
		} {
			if ok, refusal := modeAllowsTool(mode, tool); !ok {
				t.Errorf("%s was refused %q: %s — the operator confirmed in-Orchicon work stays available; only "+
					"the ACTION phase is locked", mode, tool, refusal)
			}
		}
	}
}

// AN UNSTAMPED OR UNKNOWN MODE ALLOWS EVERYTHING, deliberately — see modeAllowsTool.
func TestAnUnstampedModeIsNotABoundary(t *testing.T) {
	for _, mode := range []string{"", "not-a-mode"} {
		if ok, _ := modeAllowsTool(mode, "write"); !ok {
			t.Errorf("mode %q refused write — a missing or unrecognised mode must not disable a session's tools",
				mode)
		}
	}
	if got := askModeFromContext(context.Background()); got != "" {
		t.Errorf("an unstamped context reported mode %q, want empty", got)
	}
}

// THE MODE RIDES THE CONTEXT, which is how it reaches the gate at all.
func TestTheModeRidesTheContext(t *testing.T) {
	for _, mode := range everyMode {
		if got := askModeFromContext(withAskMode(context.Background(), mode)); got != mode {
			t.Errorf("round trip lost the mode: got %q, want %q", got, mode)
		}
	}
}

// AND THE REFUSAL HAPPENS AT THE REAL CHOKE POINT, BEFORE ANY WORK.
//
// This is the integration half: it calls the tool surface the bridge calls, with a mode-stamped context, and
// asserts the call is refused. The refusal must arrive as an ERROR because that is what the bridge turns into
// the model-visible RESULT — so the model is handed the message and can relay it.
func TestTheRealToolSurfaceRefusesTheWork(t *testing.T) {
	svc := &Service{toolRegistry: testToolRegistry()}
	p := svc.NativeAskTools()

	brainstorm := withAskMode(context.Background(), modeBrainstorm)
	_, err := p.ExecuteAskTool(brainstorm, "write", `{"path":"x","content":"y"}`)
	if err == nil {
		t.Fatal("brainstorm ran write — the platform let the action phase through")
	}
	if !strings.Contains(err.Error(), "REFUSED BY THE PLATFORM") || !strings.Contains(err.Error(), "Iteration") {
		t.Errorf("the refusal does not tell the model what happened and what to ask for: %v", err)
	}

	// THE MCP-STYLE PREFIX IS TOLERATED EVERYWHERE ELSE, so it must not be a way AROUND the gate: the system
	// prompt advertises `orchicon_<tool>`, and a gate that only matched bare names would be bypassed by the
	// spelling the prompt itself teaches.
	_, err = p.ExecuteAskTool(brainstorm, "orchicon_bash", `{"command":"git checkout main"}`)
	if err == nil {
		t.Fatal("the orchicon_ prefix bypassed the gate — a branch cut went through as Brainstorm")
	}

	// AND ITERATION, WHOSE REFUSALS ARE THE OTHER SET.
	iteration := withAskMode(context.Background(), modeIteration)
	_, err = p.ExecuteAskTool(iteration, "create_work_item", `{"title":"t","project_id":"p"}`)
	if err == nil {
		t.Fatal("iteration created a work item — the hard block the operator asked for is not in force")
	}
	if !strings.Contains(err.Error(), "Brainstorm") {
		t.Errorf("iteration's refusal does not name the mode that DOES this: %v", err)
	}
}

// AND THE MODEL IS NEVER OFFERED WHAT IT MAY NOT RUN.
//
// The refusal makes "no matter what the user says" true; this is the other half. A model that is OFFERED write in
// Brainstorm will try it, be refused, and can be talked into trying again — every attempt a wrong turn in the
// transcript. Hiding it means the tool LIST states the boundary before any call exists to refuse, so a mode that
// cannot do the work no longer advertises that it can.
//
// It also pins the two lists to ONE table: if the offered surface and the enforced surface were ever computed
// separately, this test is where the drift shows up.
func TestTheToolSurfaceHidesWhatTheModeRefuses(t *testing.T) {
	svc := &Service{toolRegistry: testToolRegistry()}
	p := svc.NativeAskTools()

	names := func(ctx context.Context) map[string]bool {
		out := map[string]bool{}
		for _, d := range p.AskToolDefs(ctx) {
			out[d.Name] = true
		}
		return out
	}

	brainstorm := names(withAskMode(context.Background(), modeBrainstorm))
	for _, hidden := range []string{"write", "edit", "batch_write", "bash"} {
		if brainstorm[hidden] {
			t.Errorf("brainstorm is OFFERED %q — a tool the gate refuses must not be advertised either", hidden)
		}
	}
	for _, kept := range []string{"read", "grep", "glob", "list_projects", "create_work_item"} {
		if !brainstorm[kept] {
			t.Errorf("brainstorm is not offered %q — the read-only suite and the platform tools are exactly what "+
				"this mode is for", kept)
		}
	}

	// ITERATION IS THE MIRROR IMAGE: it keeps the work tools and loses the plan tools.
	iteration := names(withAskMode(context.Background(), modeIteration))
	if iteration["create_work_item"] {
		t.Error("iteration is OFFERED create_work_item — the hard block the operator asked for is not in force")
	}
	for _, kept := range []string{"write", "edit", "bash", "read"} {
		if !iteration[kept] {
			t.Errorf("iteration is not offered %q — that is this mode's whole job", kept)
		}
	}

	// AND AN UNSTAMPED CONTEXT IS THE FULL SURFACE, exactly as it was before any of this existed. That is the
	// invariant that keeps the change from breaking a caller that does not stamp a mode.
	full := names(context.Background())
	for _, want := range []string{"write", "edit", "batch_write", "bash", "create_work_item", "read"} {
		if !full[want] {
			t.Errorf("an unstamped context lost %q — with no mode there is no boundary, and the surface must be "+
				"what it always was", want)
		}
	}
}
