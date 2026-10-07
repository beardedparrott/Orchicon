package tui

// rail_scope_notice_test.go — the automatic launch-directory scope must SAY WHAT IT HIDES.
//
// The defect this pins: `applyLaunchDirScope` narrows the rail to the launch directory's
// project, and it was completely silent about it. Combined with a rail that loaded only its
// first page, a tenant holding 184 conversations showed ONE row — and because the scope is
// re-derived from the same launch directory on every launch, restarting orch did not clear
// it. The operator read that as permanent data loss ("ALL conversations except this one
// disappeared ... even after restarting orch they are missing").
//
// The scope LABEL stays quiet (this is an automatic default, not a declared switch), but the
// consequence no longer is.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// Narrowing the rail tells the operator what it hid, and how to widen it.
func TestTheLaunchDirectoryScopeReportsWhatItHides(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/home/me/projects/Orchicon"
	m.dock.SetNotice("")
	m.applyLaunchDirScope()

	if m.projectScope != "p-orch" {
		t.Fatalf("scope = %q, want p-orch", m.projectScope)
	}
	got := m.dock.Notice
	if !strings.Contains(got, "2 conversation") {
		t.Errorf("notice %q does not say how many conversations were hidden (c-2 and c-3 are outside p-orch)", got)
	}
	if !strings.Contains(got, "/project") {
		t.Errorf("notice %q does not say how to widen the scope", got)
	}
	if !strings.Contains(got, "Orchicon") {
		t.Errorf("notice %q does not name the workspace it scoped to", got)
	}
}

// And it stays quiet when it hides nothing: a notice on every launch that narrows nothing is
// noise, and noise is what makes the operator stop reading the ones that matter.
func TestTheLaunchDirectoryScopeIsQuietWhenNothingIsHidden(t *testing.T) {
	m := scopePlane2(t)
	// Only the launch directory's own project has conversations.
	m.conversations = []chat.Conversation{{ID: "c-1", Title: "orch chat", ProjectID: "p-orch"}}
	m.launchDir = "/home/me/projects/Orchicon"
	m.dock.SetNotice("")
	m.applyLaunchDirScope()

	if m.projectScope != "p-orch" {
		t.Fatalf("scope = %q, want p-orch", m.projectScope)
	}
	if strings.Contains(m.dock.Notice, "not shown") {
		t.Errorf("notice %q reports hidden conversations when the scope hid none", m.dock.Notice)
	}
}
