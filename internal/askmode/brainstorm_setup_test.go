package askmode

// brainstorm_setup_test.go — BRAINSTORM CAN SET A PROJECT UP WITHOUT BEING THE HANDS.

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
