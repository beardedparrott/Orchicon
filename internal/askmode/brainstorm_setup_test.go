package askmode

// brainstorm_setup_test.go — BRAINSTORM CAN SET A PROJECT UP WITHOUT BEING THE HANDS.
//
// The operator's ask was for Brainstorm to "help you get that beginning stuff set up" without editing code or
// committing. The boundary already supports exactly that, and this file pins it as a CONTRACT rather than an
// accident — because the obvious way to satisfy the ask (grant bash) would quietly remove the boundary:
//
//   - SETUP IS ALLOWED: create_project, update_project, create_project_directory and list_project_branches are
//     product tools the gate has no opinion about, so Brainstorm can create a project and offer real branches.
//   - THE GIT STATE IS NO LONGER BRAINSTORM'S PROBLEM AT ALL: EnsureGitState now runs on project create and
//     save (internal/project/git_setup.go), so the `git init` / first-commit / identity work happens WITHOUT any
//     agent running bash. That is the whole reason this needs no new tool: the platform does it, not the planner.
//   - THE HANDS STAY REFUSED: write, edit, batch_write and bash remain denied. The operator's decision on
//     re-reading this: "let's leave that alone for now and keep the responsibilities segregated."
//
// THE RELATIONSHIP NAMED FOR A FUTURE READER: internal/project.EnsureGitState runs on project create and
// update. If that ever stops happening, Brainstorm's "help me set it up" story silently becomes a lie —
// which is why the two halves above are asserted together rather than separately.
//
// The point of the file is the PAIRING. A change that grants bash to Brainstorm would break the second half; a
// change that moved project creation out of the shared tool set would break the first.

import "testing"

// TestBrainstormCanSetAProjectUp keeps the capability the operator asked for.
func TestBrainstormCanSetAProjectUp(t *testing.T) {
	for _, tool := range []string{
		"create_project",           // start the project
		"update_project",           // fix its directory / strategy after the fact
		"create_project_directory", // make the directory it will live in
		"list_project_branches",    // read the git identity so it can confirm a base/merge branch
	} {
		if !Allows(Brainstorm, tool) {
			t.Errorf("Brainstorm may not run %q — setting a project up is exactly the beginning-stuff help it is "+
				"for, and none of these tools edits code or commits", tool)
		}
	}
}

// TestBrainstormStillCannotBeTheHands is the other half, and the one that must not be traded away for
// convenience: the setup above requires no shell, because the platform performs the git initialisation itself.
func TestBrainstormStillCannotBeTheHands(t *testing.T) {
	for _, tool := range []string{"write", "edit", "batch_write", "bash"} {
		if Allows(Brainstorm, tool) {
			t.Errorf("Brainstorm may run %q. Project setup does NOT need it — EnsureGitState "+
				"(internal/project/git_setup.go) does the git initialisation on create and save, so granting a "+
				"shell here would remove the boundary for no capability gained", tool)
		}
	}
}
