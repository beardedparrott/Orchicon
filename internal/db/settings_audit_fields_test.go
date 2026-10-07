package db_test

// settings_audit_fields_test.go — the audited shape of a settings row.
//
// WHY THIS IS A PURE TEST, and why it matters more than it looks. The operator asked, of a stall window
// they had set and later found unset: "I set 600 seconds in the TUI under settings edit and saved it yet
// you said you had to set it." The honest answer was that the trail could not say — `settings.updated`
// recorded the default models and the session TTLs and NOTHING ELSE, so a changed or blanked stall window
// left no trace, and neither could the agent-facing `update_settings` tool be seen to have written at all.
//
// This pins the SHAPE both write paths now record (db.TenantSettingsRow.AuditFields), because the shape is
// the diagnostic: every stall knob present, and a NULL preserved as null rather than collapsed.

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestAuditFieldsRecordEveryStallKnob(t *testing.T) {
	fields := db.TenantSettingsRow{}.AuditFields()

	// THE COMPLETE SET, so a knob added later without being audited fails here rather than staying invisible
	// until the next time someone asks "did my save land?".
	for _, key := range []string{
		"stall_no_progress_window_seconds",
		"stall_no_file_diff_window_seconds",
		"stall_text_loop_window_seconds",
		"stall_repetition_count",
		"stall_repetition_window_seconds",
		"stall_nudge_max",
		"stall_nudge_reply_window_seconds",
		"stall_nudge_cooldown_seconds",
		"stall_tool_hang_seconds",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the audit snapshot does not record %q — a change to it would leave no trace, which is "+
				"exactly how this question became unanswerable", key)
		}
	}

	// And the fields that were already audited must not have been dropped in the move.
	for _, key := range []string{
		"default_worker_model", "default_ask_orchicon_model",
		"session_access_token_ttl_seconds", "session_refresh_token_ttl_seconds",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the audit snapshot lost %q, which the RPC path used to record", key)
		}
	}
}

// A BLANK WINDOW AND A DISABLED ONE MUST BE DISTINGUISHABLE IN THE SNAPSHOT.
//
// These columns are nullable precisely because NULL means "use the built-in default" while 0 means
// DISABLED — two different instructions an operator may have chosen deliberately. Flattening a nil to 0 (or
// omitting the key) would make the snapshot unable to answer the ONE question it exists for: was this
// window blanked by a save, or set to off on purpose?
func TestAuditFieldsDistinguishBlankFromDisabled(t *testing.T) {
	disabled := int64(0)
	configured := int64(600)
	row := db.TenantSettingsRow{
		StallNoProgressWindowSeconds: &disabled,   // explicitly DISABLED
		StallNoFileDiffWindowSeconds: &configured, // explicitly 600
		// StallTextLoopWindowSeconds left nil: BLANK ("use the built-in default")
	}
	fields := row.AuditFields()

	if got, ok := fields["stall_no_progress_window_seconds"]; !ok || got == nil {
		t.Fatalf("an explicitly disabled window recorded as %v (present=%v), want 0 — blank and disabled are different instructions", got, ok)
	} else if *got.(*int64) != 0 {
		t.Errorf("the disabled window recorded as %v, want 0", *got.(*int64))
	}

	if got, ok := fields["stall_no_file_diff_window_seconds"]; !ok || got == nil || *got.(*int64) != 600 {
		t.Errorf("the configured window recorded as %v, want 600", got)
	}

	// THE KEY IS PRESENT AND MARSHALS TO null — not absent. An absent key reads as "this build does not
	// record that field"; a null reads as "recorded, and blank". The two are different diagnoses, and the
	// MARSHALLED form is what an auditor actually reads, so that is what is asserted.
	//
	// (A typed nil pointer does not compare equal to nil as an `any`, so checking the map value directly
	// would report a correct null as "not nil" — the assertion has to go through the JSON.)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal the snapshot: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal the snapshot: %v", err)
	}
	blank, ok := wire["stall_text_loop_window_seconds"]
	if !ok {
		t.Fatal("a blank window's key is ABSENT from the snapshot — a reader cannot tell \"blank\" from \"not recorded\"")
	}
	if string(blank) != "null" {
		t.Errorf("a blank window marshals as %s, want null — NULL is how the schema says \"use the built-in default\", and a 0 would read as DISABLED", blank)
	}
	if got := string(wire["stall_no_progress_window_seconds"]); got != "0" {
		t.Errorf("the explicitly disabled window marshals as %s, want 0", got)
	}
	if got := string(wire["stall_no_file_diff_window_seconds"]); got != "600" {
		t.Errorf("the configured window marshals as %s, want 600", got)
	}
}
