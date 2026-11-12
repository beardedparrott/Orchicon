package work

// project_activate_test.go — A DRAFTING PROJECT CANNOT HOLD WORK, AND THE TUI HAD NO WAY OUT OF
// DRAFTING.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// THE ACTION IS OFFERED FOR A DRAFTING PROJECT, and calling it activates that project.
func TestADraftingProjectOffersActivate(t *testing.T) {
	p := newPlane()
	pr := p.seedProject("proj-1", "Thing") // seeded ACTIVE (see the fake); the server creates DRAFTING
	pr.Status = apiv1.ProjectStatus_PROJECT_STATUS_DRAFTING

	m := newModel(t, p)
	m.SelectSource(srcProjects)
	load(t, m, srcProjects)

	act, ok := findProjectAction(m, "activate")
	if !ok {
		t.Fatalf("a drafting project offers no activate action, so it is a dead end in the TUI: work items are " +
			"refused for a non-active project and nothing offers the transition")
	}
	if act.Key == "" {
		t.Error("the activate action has no key, so it cannot be reached from the keyboard")
	}
	if act.Source != srcProjects {
		t.Errorf("the activate action's source is %q, want %q. Source is NOT a display filter — it is the "+
			"list RECONCILED after the write (mutate.Request.Source), so a wrong value refreshes the wrong "+
			"list and leaves the project's row showing its old status", act.Source, srcProjects)
	}
	if err := act.Do(context.Background()); err != nil {
		t.Fatalf("the activate action failed on a drafting project: %v", err)
	}
	if len(p.projActivated) != 1 || p.projActivated[0] != "proj-1" {
		t.Fatalf("ActivateProject was called with %v, want [proj-1]", p.projActivated)
	}
	// And it is no longer offered, because the project is no longer drafting — the
	// action cannot be run twice into a precondition failure.
	if got := p.projects["proj-1"].GetStatus(); got != apiv1.ProjectStatus_PROJECT_STATUS_ACTIVE {
		t.Fatalf("status after activating = %v, want ACTIVE", got)
	}
}

// AND NOT FOR ANY OTHER STATE.
func TestAnActiveProjectOffersNoActivate(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Thing") // ACTIVE

	m := newModel(t, p)
	m.SelectSource(srcProjects)
	load(t, m, srcProjects)

	if act, ok := findProjectAction(m, "activate"); ok {
		t.Errorf("an active project offers an activate action (%q). ActivateProject refuses a non-drafting "+
			"project, so this control would fail every time it was used correctly — and the fixture proves it: "+
			"the fake mirrors the server's precondition and would return FailedPrecondition.", act.Label)
	}
	// The other project action is still there, so the absence is specific to activate
	// rather than the toolbar having failed to build at all.
	if _, ok := findProjectAction(m, "create project dir"); !ok {
		t.Error("no project actions at all — the fixture did not reach projectActions, so the assertion above " +
			"would pass for the wrong reason")
	}
}

// THE STATUS READING CANNOT FALSE-POSITIVE ON A DIRECTORY NAME.
func TestProjectNeedsActivationReadsTheStatusToken(t *testing.T) {
	drafting := []string{
		"drafting",
		"drafting · /home/me/projects/thing",
		"drafting · ",
	}
	for _, meta := range drafting {
		if !projectNeedsActivation(meta) {
			t.Errorf("projectNeedsActivation(%q) = false — a drafting project must offer the only transition "+
				"that makes it usable", meta)
		}
	}
	notDrafting := []string{
		"",
		"active",
		"active · /home/me/projects/thing",
		"paused",
		"archived",
		"drafting-notes",                        // a directory, not the status
		"/home/me/drafting · /tmp/x",            // status active, path says drafting
		"active · /home/me/drafting",            // the word appears in the DIRECTORY
		"active · /home/me/projects/drafting/x", // and deeper in it
	}
	for _, meta := range notDrafting {
		if projectNeedsActivation(meta) {
			t.Errorf("projectNeedsActivation(%q) = true — the TUI would offer an activate action the server "+
				"refuses, and the DIRECTORY portion of Meta is operator-supplied text that must never be read "+
				"as state", meta)
		}
	}
}

// findProjectAction returns the project action whose label contains want.
func findProjectAction(m *Model, want string) (actionDoer, bool) {
	for _, a := range m.projectActions() {
		if strings.Contains(strings.ToLower(a.Label), want) {
			return actionDoer{Key: a.Key, Label: a.Label, Source: a.Source, Confirm: a.Confirm, Do: a.Do}, true
		}
	}
	return actionDoer{}, false
}

// actionDoer is the slice of kit2.Action these tests need, so the assertions read
// against a named shape rather than a positional struct literal.
type actionDoer struct {
	Key     string
	Label   string
	Source  string
	Confirm string
	Do      func(context.Context) error
}
