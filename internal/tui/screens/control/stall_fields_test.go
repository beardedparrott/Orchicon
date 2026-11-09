package control

// stall_fields_test.go — every stall knob must be reachable from the TUI.

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// allStallFields is the coverage contract: every stall dimension the settings row carries.
var allStallFields = []string{
	"stall_no_progress_window_seconds",
	"stall_no_file_diff_window_seconds",
	"stall_text_loop_window_seconds",
	"stall_repetition_count",
	"stall_repetition_window_seconds",
	"stall_nudge_max",
	"stall_nudge_reply_window_seconds",
	"stall_nudge_cooldown_seconds",
	"stall_tool_hang_seconds",
}

// The complaint was that the settings were not there, so this asserts the form RENDERS a field for each one.
func TestSettingsFormCarriesEveryStallKnob(t *testing.T) {
	f := (&Model{}).settingsForm()
	have := map[string]bool{}
	for _, s := range f.Specs {
		have[s.Name] = true
	}
	for _, name := range allStallFields {
		if !have[name] {
			t.Errorf("the settings form has no field %q — that stall threshold cannot be seen or set from the TUI", name)
		}
	}
}

// AND THE SUBMIT MUST CARRY THEM, which is the half a rendered field does not prove.
func TestSettingsSubmitCarriesTheNudgeTimers(t *testing.T) {
	m, _ := newWriteModel(t)

	var got *apiv1.TenantSettings
	m.rpcUpdateSettings = func(_ context.Context, s *apiv1.TenantSettings) error {
		got = s
		return nil
	}
	m.settings = &apiv1.TenantSettings{}
	if !m.SelectSource("settings") {
		t.Fatal("no settings source")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		t.Fatal("opening the settings form should not dispatch a cmd")
	}
	if !m.formOpen() {
		t.Fatal("e did not open the settings form")
	}

	// The two that had no wiring, set to distinguishable values.
	m.activeForm().Set("stall_nudge_reply_window_seconds", "75")
	m.activeForm().Set("stall_nudge_cooldown_seconds", "30")

	cmd, err := m.activeForm().Submit()
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("save: %v", res.Err)
	}
	if got == nil {
		t.Fatal("the settings RPC never fired — nothing was saved")
	}
	if v := got.GetStallNudgeReplyWindowSeconds(); v != 75 {
		t.Errorf("stall_nudge_reply_window_seconds saved as %d, want 75 — the field renders but the submit drops it", v)
	}
	if v := got.GetStallNudgeCooldownSeconds(); v != 30 {
		t.Errorf("stall_nudge_cooldown_seconds saved as %d, want 30 — the field renders but the submit drops it", v)
	}

	// AND A BLANK FIELD STILL TRAVELS AS ABSENT, not as 0.
	m.activeForm().Set("stall_nudge_reply_window_seconds", "")
	cmd, err = m.activeForm().Submit()
	if err != nil {
		t.Fatalf("Submit with a blank field: %v", err)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("save with a blank field: %v", res.Err)
	}
	if got.GetStallNudgeReplyWindowSeconds() != 0 || got.StallNudgeReplyWindowSeconds != nil {
		t.Errorf("a BLANK nudge reply window travelled as %v (nil=%v), want absent — 0 would mean DISABLED, which is a different instruction",
			got.GetStallNudgeReplyWindowSeconds(), got.StallNudgeReplyWindowSeconds == nil)
	}
}
