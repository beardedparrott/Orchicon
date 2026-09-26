package askorchicon

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestExtractAskActionPrefersTypedFields is the adapter-neutrality guard for the
// consent event, mirroring the tool-result one: an adapter that can NAME the
// action must not have to shape it into opencode's property vocabulary.
//
// The native bridge knows the tool, the command and the argument JSON at the
// moment it is about to run a call. Before this, the only way to raise an ask was
// to hand-build Detail (permission/title, metadata.filepath, patterns,
// toolInput) — making opencode the reference dialect, exactly as with tool
// resolution.
func TestExtractAskActionPrefersTypedFields(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{
		Kind:    "permission",
		Tool:    "bash",
		Command: "cp /etc/hostname /tmp/probe",
	})
	if got.Tool != "bash" {
		t.Errorf("Tool = %q, want the typed tool name", got.Tool)
	}
	if got.Command != "cp /etc/hostname /tmp/probe" {
		t.Errorf("Command = %q, want the typed command — the card must say what is being approved", got.Command)
	}
}

// TestExtractAskActionTypedTargetsForAWrite: a write ask names its paths through
// Targets, which the extractor cleans the same way it cleans the Detail path.
func TestExtractAskActionTypedTargetsForAWrite(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{
		Kind:    "permission",
		Tool:    "write",
		Targets: []string{"/etc/hosts"},
	})
	if len(got.Targets) != 1 || got.Targets[0] != "/etc/hosts" {
		t.Errorf("Targets = %v, want the typed path", got.Targets)
	}
}

// TestExtractAskActionTypedInputSuppliesMissingTarget: a tool whose ask names no
// path of its own (an MCP-style call) still resolves through its argument JSON,
// so it cannot be judged "inside the project" and silently approved.
func TestExtractAskActionTypedInputSuppliesMissingTarget(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{
		Kind:      "permission",
		Tool:      "write",
		InputJSON: `{"filePath":"/etc/hosts"}`,
	})
	if len(got.Targets) != 1 || got.Targets[0] != "/etc/hosts" {
		t.Errorf("Targets = %v, want the path taken from the argument JSON", got.Targets)
	}
	if got.Input == nil {
		t.Error("Input must be carried, or a later reader cannot re-derive the action")
	}
}

// TestExtractAskActionTypedFieldsWinOverDetail: when an adapter supplies BOTH, the
// typed fields are the authority — the Detail map is the legacy transport, not a
// second source of truth.
func TestExtractAskActionTypedFieldsWinOverDetail(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{
		Kind:    "permission",
		Tool:    "bash",
		Command: "typed command",
		Detail: map[string]any{
			"title":    "detail command",
			"metadata": map[string]any{"command": "detail command"},
		},
	})
	if got.Command != "typed command" {
		t.Errorf("Command = %q, want the typed value to win", got.Command)
	}
}

// TestExtractAskActionDetailStillWorks is the CONTROL for the fallback: opencode
// is the only adapter that emits Detail, and it must keep working unchanged.
func TestExtractAskActionDetailStillWorks(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{
		Kind: "permission",
		Detail: map[string]any{
			"title":    "bash",
			"metadata": map[string]any{"command": "ls -la"},
		},
	})
	if got.Tool != "bash" {
		t.Errorf("Tool = %q, want the detail-derived tool name", got.Tool)
	}
	if got.Command != "ls -la" {
		t.Errorf("Command = %q, want the detail-derived command", got.Command)
	}
}

// TestExtractAskActionEmptyEventResolvesToNothing pins the fail-closed posture:
// an event carrying neither typed fields nor detail must resolve to an action
// with NO key, so the decision path cannot treat it as a pre-approved scope.
func TestExtractAskActionEmptyEventResolvesToNothing(t *testing.T) {
	got := extractAskAction(scheduler.SessionEvent{Kind: "permission"})
	if got.Key != "" || got.Tool != "" || len(got.Targets) != 0 {
		t.Errorf("empty event extracted %+v, want nothing", got)
	}
	if askActionResolved(got) {
		t.Error("an ask with no target and no command must NOT be considered resolved")
	}
}
