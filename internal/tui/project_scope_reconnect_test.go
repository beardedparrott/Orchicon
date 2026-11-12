package tui

// project_scope_reconnect_test.go — THE /connect RECONNECT SHAPE, end to end.

import (
	"testing"
)

// THE REGRESSION (criterion 7): the second, continuation shell still scopes the rail to the launch
// directory's project.
func TestAReconnectShellStillScopesToTheLaunchDirectory(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon", "ai-tools"},
		direcs: []string{"/home/me/projects/Orchicon", "/home/me/ai-tools"},
	}

	// SHELL 1 — a first shell: the directory is passed AND the prompt is armed.
	first := appWithProjectServiceOpts(t, projects,
		WithLaunchDir("/home/me/projects/Orchicon"),
		WithLaunchPrompt(),
	)
	runCtx(t, first, first.Init(), runCtxCmdBudget)
	if first.projectScope != "prj-Orchicon" {
		t.Fatalf("first shell scope = %q, want prj-Orchicon — the plain first launch is the baseline", first.projectScope)
	}
	// A MATCHED directory is silent: the default is applied and no question is raised (criterion 3).
	if first.launch != nil {
		t.Fatalf("first shell raised the launch prompt for an ATTACHED directory — beginLaunchPrompt must not " +
			"fire when checkLaunchProject reports no need")
	}

	// SHELL 2 — the CONTINUATION a /connect reconnect produces: SAME directory, prompt NOT armed.
	second := appWithProjectServiceOpts(t, projects,
		WithLaunchDir("/home/me/projects/Orchicon"),
	)
	runCtx(t, second, second.Init(), runCtxCmdBudget)

	if second.projectScope != "prj-Orchicon" {
		t.Errorf("the reconnect shell's scope = %q, want prj-Orchicon — the launch-directory default was lost "+
			"after a /connect reconnect (this is the bug)", second.projectScope)
	}
	// AND A NEW CHAT IS TIED TO IT, exactly as on the first shell.
	if got := second.activeProjectID(); got != "prj-Orchicon" {
		t.Errorf("activeProjectID() = %q after the reconnect, want prj-Orchicon — a new conversation would be "+
			"created unassigned", got)
	}
	// AND THE CONTINUATION DID NOT NAG.
	if second.launch != nil {
		t.Error("the reconnect shell raised the launch prompt — a continuation must never re-ask (criterion 2)")
	}
}

// A CONTINUATION NEVER NAGS, IN ANY DIRECTORY (criterion 2). An unattached directory DOES raise the prompt on
// a first launch — and the very same directory raises nothing on the continuation.
func TestAReconnectShellNeverRaisesTheLaunchPrompt(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon"},
		direcs: []string{"/home/me/projects/Orchicon"},
	}

	// SHELL 1 — unattached directory, prompt armed: it DOES ask.
	first := appWithProjectServiceOpts(t, projects,
		WithLaunchDir("/tmp/somewhere-else"),
		WithLaunchPrompt(),
	)
	runCtx(t, first, first.Init(), runCtxCmdBudget)
	if first.launch == nil {
		t.Fatalf("a first launch in an UNATTACHED directory raised no prompt — the feature is armed here")
	}

	// SHELL 2 — the continuation, same directory, not armed: it must ask NOTHING.
	second := appWithProjectServiceOpts(t, projects,
		WithLaunchDir("/tmp/somewhere-else"),
	)
	runCtx(t, second, second.Init(), runCtxCmdBudget)

	if second.launch != nil {
		t.Error("the continuation raised the launch prompt — re-asking on a reconnect is exactly the nagging " +
			"the feature exists to avoid")
	}
	// AND AN UNMATCHED DIRECTORY LEAVES THE SCOPE AT ALL PROJECTS — deliberate, not a bug.
	if second.projectScope != projectScopeAll {
		t.Errorf("scope = %q for an unmatched directory on a continuation, want All projects", second.projectScope)
	}
}

// THE EMPTY-DIRECTORY OPT-OUT HOLDS (criterion 7's second half): arming the prompt cannot resurrect it without
// a directory, so tests and embedders that pass no directory are unaffected.
func TestArmingWithoutADirectoryStillAsksNothing(t *testing.T) {
	m := appWithProjectServiceOpts(t, &stubProjectList{
		names:  []string{"Orchicon"},
		direcs: []string{"/home/me/projects/Orchicon"},
	}, WithLaunchDir(""), WithLaunchPrompt())

	runCtx(t, m, m.Init(), runCtxCmdBudget)

	if m.launchDir != "" {
		t.Errorf("launchDir = %q, want empty — an empty WithLaunchDir must set no directory", m.launchDir)
	}
	if m.launch != nil {
		t.Error("the prompt was raised with no launch directory — arming must not resurrect it")
	}
	if m.projectScope != projectScopeAll {
		t.Errorf("scope = %q with no launch directory, want All projects", m.projectScope)
	}
}

// AN UNREACHABLE PLANE ASKS NOTHING, ON BOTH SHELLS (criterion 8). A failed check must degrade to no question
// and no crash — never a modal the operator cannot answer.
func TestADeadPlaneAsksNothingOnBothShells(t *testing.T) {
	dead := &stubProjectList{failAlways: true}

	// SHELL 1 — armed, but the plane cannot be listed.
	first := appWithProjectServiceOpts(t, dead,
		WithLaunchDir("/home/me/projects/Orchicon"),
		WithLaunchPrompt(),
	)
	runCtx(t, first, first.Init(), runCtxCmdBudget)
	if first.launch != nil {
		t.Error("a first shell raised the prompt against a plane whose project list failed — a failure to look " +
			"must never become a question")
	}

	// SHELL 2 — the continuation, same dead plane.
	second := appWithProjectServiceOpts(t, dead,
		WithLaunchDir("/home/me/projects/Orchicon"),
	)
	runCtx(t, second, second.Init(), runCtxCmdBudget)
	if second.launch != nil {
		t.Error("the continuation raised the prompt against a dead plane")
	}
	if second.projectScope != projectScopeAll {
		t.Errorf("scope = %q against a dead plane, want All projects", second.projectScope)
	}
}
