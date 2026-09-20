package askmode

// askmode_test.go — THE NEUTRAL POLICY, TESTED WHERE IT LIVES.
//
// The adapter-level tests (askorchicon's modegate_test.go) cover the REFUSAL: the message, the withholding, and
// the fact that the real tool surface honours it. These cover the POLICY itself — the table, the vocabulary, and
// the two defaults — because that is the part every adapter will now share, and a fault here is a fault in all of
// them at once.

import (
	"context"
	"strings"
	"testing"
)

// THE TWO SETS, and the modes that own them. This is the boundary in one assertion: the doers are refused by
// everything that is not the hands, and the planners are refused by the hands.
func TestTheBoundaryTable(t *testing.T) {
	work := []string{"write", "edit", "batch_write", "bash"}
	plan := []string{
		"create_work_item", "update_work_item", "schedule_work_item", "reorder_work_items",
		"control_sequence", "force_progress_workflow_run", "retry_failed_workflow_run",
	}

	for _, mode := range []string{Brainstorm, QuickWork} {
		for _, tool := range work {
			if Allows(mode, tool) {
				t.Errorf("%s may run %q — the action phase is Iteration's job", mode, tool)
			}
		}
		// ...and it KEEPS the plan tools, which is what the mode is FOR.
		for _, tool := range plan {
			if !Allows(mode, tool) {
				t.Errorf("%s may not run %q — authoring the plan is exactly what this mode does", mode, tool)
			}
		}
	}

	for _, tool := range plan {
		if Allows(Iteration, tool) {
			t.Errorf("iteration may run %q — authoring the plan or firing the pipeline is not this mode's job", tool)
		}
	}
	for _, tool := range work {
		if !Allows(Iteration, tool) {
			t.Errorf("iteration may not run %q — that is the ONE thing this mode is for", tool)
		}
	}
}

// THE READ-ONLY SUITE SURVIVES EVERYWHERE. The operator's condition on denying bash: "That is fine as long as
// Brainstorm mode can learn about the project and read the files." A policy that denied too much would satisfy the
// boundary and destroy the mode.
func TestEveryModeKeepsTheReadOnlySuite(t *testing.T) {
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		for _, tool := range []string{
			"read", "batch_read", "grep", "batch_grep", "glob", "list",
			"list_project_dir", "read_project_file", "ask_file_root",
		} {
			if !Allows(mode, tool) {
				t.Errorf("%s may not run %q — it could no longer read the project it is meant to understand", mode, tool)
			}
		}
	}
}

// AND THE PLATFORM TOOLS STAY AVAILABLE TO EVERY MODE. The operator: "for in-Orchicon work it is fine but no
// cutting of branches, file editing, pull requests" — so only the ACTION phase is locked, and a gate that started
// refusing projects or workers would be a different, unrequested change.
func TestInOrchiconWorkStaysAvailable(t *testing.T) {
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		for _, tool := range []string{
			"list_projects", "create_project", "create_project_directory", "list_work_items",
			"get_work_item", "create_worker", "create_workflow", "build_runtime_image",
			"install_mcp_server", "archive_work_item", "restore_work_item", "delete_work_item",
		} {
			if !Allows(mode, tool) {
				t.Errorf("%s was denied %q — the operator confirmed in-Orchicon work stays available", mode, tool)
			}
		}
	}
}

// AN UNKNOWN OR EMPTY MODE IS NOT A BOUNDARY.
func TestAnUnknownModeAllowsEverything(t *testing.T) {
	for _, mode := range []string{"", "not-a-mode", "Brainstorm"} {
		if !Allows(mode, "write") {
			t.Errorf("mode %q denied write — a missing or unrecognised mode must not disable a session's tools", mode)
		}
		if p, ok := PolicyFor(mode); ok {
			t.Errorf("PolicyFor(%q) returned a policy (%+v) for a mode the table does not know", mode, p)
		}
	}
}

// EVERY POLICY NAMES A MODE TO SWITCH TO, and one that is not itself — a refusal must be a direction, and a
// refusal pointing at the mode you are already in is a dead end.
func TestEveryPolicyNamesADifferentTarget(t *testing.T) {
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		p, ok := PolicyFor(mode)
		if !ok {
			t.Fatalf("no policy for %q", mode)
		}
		if p.Mode != mode {
			t.Errorf("policy for %q carries Mode %q", mode, p.Mode)
		}
		if p.Why == "" {
			t.Errorf("policy for %q has no explanation", mode)
		}
		if p.SwitchTo == "" || p.SwitchTo == mode {
			t.Errorf("policy for %q names switch target %q — a refusal must point somewhere else", mode, p.SwitchTo)
		}
		if _, known := PolicyFor(p.SwitchTo); !known {
			t.Errorf("policy for %q points at %q, which is not a mode", mode, p.SwitchTo)
		}
	}
}

// DeniedNames IS SORTED AND COMPLETE, because an adapter that applies the policy through its own mechanism (a
// config block, a flag) is handed this list — and an unsorted one would make a generated policy non-deterministic
// and its test flaky.
func TestDeniedNamesIsSortedAndComplete(t *testing.T) {
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		names := DeniedNames(mode)
		if len(names) == 0 {
			t.Errorf("DeniedNames(%q) is empty", mode)
		}
		for i := 1; i < len(names); i++ {
			if names[i] < names[i-1] {
				t.Errorf("DeniedNames(%q) is not sorted: %v", mode, names)
				break
			}
		}
		if denied := DeniedNames("not-a-mode"); denied != nil {
			t.Errorf("DeniedNames of an unknown mode returned %v, want nil", denied)
		}
	}
	// Brainstorm and Quick Work refuse THE SAME SET — the table's shape, asserted rather than assumed.
	if got, want := strings.Join(DeniedNames(Brainstorm), ","), strings.Join(DeniedNames(QuickWork), ","); got != want {
		t.Errorf("brainstorm denies %q but quick work denies %q — they are the same disposition toward action", got, want)
	}
}

// THE MODE RIDES THE CONTEXT, which is how it reaches any adapter's enforcement point.
func TestTheModeRidesTheContext(t *testing.T) {
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		if got := ModeFromContext(WithMode(context.Background(), mode)); got != mode {
			t.Errorf("round trip lost the mode: got %q, want %q", got, mode)
		}
	}
	if got := ModeFromContext(context.Background()); got != "" {
		t.Errorf("an unstamped context reported %q, want empty", got)
	}
	// A nil context must not panic — the helper is called from paths that may not carry one.
	if got := ModeFromContext(nil); got != "" {
		t.Errorf("a nil context reported %q, want empty", got)
	}
}
