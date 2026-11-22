package askorchicon

// recurring_tool_test.go — the MCP tool surface must be able to CREATE, CHANGE and CLEAR a recurring
// work item.
//
// THE GAP THIS CLOSES. Recurrence was a complete feature in the domain — the JSONB column, the
// RecurringSchedule proto, ComputeNextRunAt, the per-fire run history — and the Connect API could set it.
// But create_work_item and update_work_item had no recurrence parameter at all, so an Ask session in
// Brainstorm or Iteration literally could not express one. The agent's only move was to describe the
// schedule in prose, which schedules nothing.
//
// These tests are DB-FREE where they can be: the argument shape, the proto conversion and the tool
// schema are pure. The invariants (flat top-level task, workflow binding) are asserted through the same
// functions the API uses, so the two surfaces cannot disagree about what a valid recurrence is.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

func dailyRecurrence() *recurrenceArg {
	return &recurrenceArg{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-12-01",
		StartTime: "09:00",
		Timezone:  "America/Chicago",
	}
}

// TestRecurrenceArgConvertsToProto pins the field-for-field bridge. A dropped field here would silently
// schedule something different from what the operator asked for.
func TestRecurrenceArgConvertsToProto(t *testing.T) {
	rec := dailyRecurrence()
	rec.Days = []string{"Mon", "Wed"}
	rec.OutputsMode = "idea"
	rec.WindowStart = "09:00"
	rec.WindowEnd = "17:00"

	got := rec.toProto("")
	if got == nil {
		t.Fatal("toProto returned nil for a populated recurrence")
	}
	if got.Frequency != "daily" || got.Interval != 1 {
		t.Errorf("frequency/interval = %q/%d, want daily/1", got.Frequency, got.Interval)
	}
	if got.StartDate != "2026-12-01" || got.StartTime != "09:00" {
		t.Errorf("start = %q %q, want 2026-12-01 09:00", got.StartDate, got.StartTime)
	}
	if got.Timezone != "America/Chicago" {
		t.Errorf("timezone = %q, want America/Chicago — a wall-clock rule without its zone fires at the wrong instant",
			got.Timezone)
	}
	if len(got.Days) != 2 || got.Days[0] != "Mon" {
		t.Errorf("days = %v, want [Mon Wed]", got.Days)
	}
	if got.OutputsMode != "idea" {
		t.Errorf("outputs_mode = %q, want idea", got.OutputsMode)
	}
	if got.WindowStart != "09:00" || got.WindowEnd != "17:00" {
		t.Errorf("window = %q..%q, want 09:00..17:00", got.WindowStart, got.WindowEnd)
	}
}

// TestNilRecurrenceStaysOneShot: absence must mean "one-shot", not "empty schedule".
func TestNilRecurrenceStaysOneShot(t *testing.T) {
	var rec *recurrenceArg
	if got := rec.toProto(""); got != nil {
		t.Fatalf("a nil recurrence produced %+v, want nil (one-shot)", got)
	}
}

// TestOmittedTimezoneFallsBackToTenant keeps a missing zone from becoming UTC by accident. The domain
// treats empty as UTC for LEGACY rows; a new schedule must never inherit that by omission.
func TestOmittedTimezoneFallsBackToTenant(t *testing.T) {
	rec := dailyRecurrence()
	rec.Timezone = ""
	if got := rec.toProto("Europe/Berlin"); got.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q, want the tenant's Europe/Berlin", got.Timezone)
	}
}

// TestRecurrenceValidationIsTheDomainsOwn walks the cases the shared validator rejects, so the tool
// surface cannot accept a recurrence the Connect API would refuse.
func TestRecurrenceValidationIsTheDomainsOwn(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*recurrenceArg)
		wantErr string
	}{
		{"bad frequency", func(r *recurrenceArg) { r.Frequency = "fortnightly" }, "frequency"},
		{"zero interval", func(r *recurrenceArg) { r.Interval = 0 }, "interval"},
		{"bad day", func(r *recurrenceArg) { r.Days = []string{"Funday"} }, "days"},
		{"missing start_date", func(r *recurrenceArg) { r.StartDate = "" }, "start_date"},
		{"bad start_time", func(r *recurrenceArg) { r.StartTime = "9am" }, "start_time"},
		{"window half set", func(r *recurrenceArg) { r.WindowStart = "09:00" }, "window_start and window_end"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := dailyRecurrence()
			tc.mutate(rec)
			_, err := workitem.ValidateRecurringSchedule(rec.toProto(""))
			if err == nil {
				t.Fatalf("%s was accepted, but the shared validator rejects it", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestRecurringItemMustBeFlatTopLevelTask and the workflow binding are the two invariants that make a
// recurrence schedulable. Asserted here through the domain's own guards.
func TestRecurringItemMustBeFlatTopLevelTask(t *testing.T) {
	if err := workitem.ValidateRecurringFlatness(true, "task", ""); err != nil {
		t.Fatalf("a flat top-level task was rejected: %v", err)
	}
	if err := workitem.ValidateRecurringFlatness(true, "epic", ""); err == nil {
		t.Error("an epic recurrence was accepted; recurring items must be flat tasks")
	}
	if err := workitem.ValidateRecurringFlatness(true, "task", "parent-1"); err == nil {
		t.Error("a recurrence with a parent was accepted; recurring items must be top-level")
	}
}

func TestRecurringItemMustBeBoundToAWorkflow(t *testing.T) {
	if err := workitem.ValidateRecurringWorkflowBinding(true, false, ""); err == nil {
		t.Error("a recurrence with no workflow was accepted; the fire would have nothing to start")
	}
	if err := workitem.ValidateRecurringWorkflowBinding(true, false, "wf-1"); err != nil {
		t.Errorf("a recurrence bound to a workflow was rejected: %v", err)
	}
}

// TestCreateToolSchemaExposesRecurrence is the regression guard for the actual complaint: the parameter
// must be VISIBLE to the model, or it will never be sent no matter what the prompt says.
func TestCreateToolSchemaExposesRecurrence(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	for _, name := range []string{"create_work_item", "update_work_item"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if _, ok := tool.Properties["recurrence"]; !ok {
			t.Errorf("%s has no recurrence parameter — the agent cannot create a recurring item", name)
		}
		if !strings.Contains(strings.ToLower(tool.Properties["recurrence"].Description), "timezone") {
			t.Errorf("%s recurrence description does not mention timezone", name)
		}
	}
	// update needs the clear flag too, or a recurrence can be set and never removed.
	upd, _ := reg.Get("update_work_item")
	if _, ok := upd.Properties["clear_recurrence"]; !ok {
		t.Error("update_work_item has no clear_recurrence — an agent could create a recurrence it cannot stop")
	}
}

// TestRecurrenceArgUnmarshalsFromToolJSON proves the wire shape the model actually sends.
func TestRecurrenceArgUnmarshalsFromToolJSON(t *testing.T) {
	raw := `{
		"title": "Morning digest", "project_id": "p1", "kind": "task",
		"recurrence": {
			"frequency": "daily", "interval": 1, "start_date": "2026-12-01",
			"start_time": "09:00", "timezone": "America/Chicago"
		}
	}`
	var params struct {
		Title      string         `json:"title"`
		Recurrence *recurrenceArg `json:"recurrence"`
	}
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if params.Recurrence == nil {
		t.Fatal("recurrence did not decode")
	}
	if params.Recurrence.Frequency != "daily" || params.Recurrence.Timezone != "America/Chicago" {
		t.Fatalf("decoded %+v", params.Recurrence)
	}
	if got := params.Recurrence.toProto(""); got == nil || got.Timezone != "America/Chicago" {
		t.Fatalf("toProto lost the zone: %+v", got)
	}
}

// TestComputeNextRunAtProducesASchedule is the last link: a valid recurrence must yield a next fire.
func TestComputeNextRunAtProducesASchedule(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-11-30T12:00:00Z")
	if err != nil {
		t.Fatalf("parse reference time: %v", err)
	}
	next := workitem.ComputeNextRunAt(dailyRecurrence().toProto(""), now)
	if next == nil {
		t.Fatal("ComputeNextRunAt returned nil for a valid daily recurrence — the item would never fire")
	}
}

// keep the proto import honest: the validator's contract is a proto message.
var _ = func() *apiv1.RecurringSchedule { return dailyRecurrence().toProto("") }
var _ = db.WorkItemRow{}
