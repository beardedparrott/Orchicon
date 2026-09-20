package work

// schedule_test.go — the scheduled start: a DAY AND A TIME, chosen from one modal.
//
// The operator: "What I would really like is a calendar and time picker as opposed to the canned times."
//
// WHAT WAS HERE BEFORE. A KPicker fed by schedulePresets(): ten canned offsets ("in 15 minutes" …
// "in 1 week") whose labels printed a raw UTC timestamp. The operator's earlier report — "Having to type
// the time in the EXACT format or pick very specific time jumps like 15 minutes is very limiting" — is a
// description of that control's two failure modes: take a time you did not want, or type RFC3339 by hand.
// The test that asserted the preset list existed is DELETED with the list. Asserting that a list of the
// wrong control is present cannot survive the list being the problem.
//
// WHAT REPLACED IT. A KDateTime field (read-only, chosen from a modal) plus this screen hosting
// kit2.DateTimePicker. The modal's own navigation, stepping, carry and zone label are pinned in
// kit2/datetimepicker_test.go. What is pinned HERE is the WIRING, because that is the layer that breaks
// in silence:
//
//	a field whose host hook was never installed looks exactly like a field with nothing to open;
//	a picker that commits into the wrong form drops the value without a word.
//
// Both are asserted below, because both are invisible until the operator has already lost the time they
// picked.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// openItemEditor opens the details editor for an item, optionally already scheduled.
//
// The `e` gesture is the path the scheduled start actually lives on: the field is on the INLINE
// detail-pane editor, not on a modal form, which matters for the finish hook (see
// finishDateTimePicker's note about ActiveForm).
func openItemEditor(t *testing.T, start *time.Time) (*Model, *kit2.Form) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	item := &apiv1.WorkItem{
		Id:        "wi-1",
		Title:     "Item",
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1",
		Status:    apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	}
	if start != nil {
		item.ScheduledStartAt = timestamppb.New(*start)
	}
	p.addItem(item)
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	return m, f
}

// tap sends a key and dispatches any command it produced.
//
// UNLIKE run IT TOLERATES A NIL COMMAND, which several of these gestures produce: opening and closing the
// calendar are pure STATE changes with nothing to await, and run's "expected a command, got nil" would
// fail them for being correct.
func tap(t *testing.T, m *Model, k tea.KeyMsg) {
	t.Helper()
	if _, cmd := m.Update(k); cmd != nil {
		run(t, m, cmd)
	}
}

// dtSpec returns the scheduled-start field's spec.
func dtSpec(t *testing.T, f *kit2.Form) kit2.FieldSpec {
	t.Helper()
	for _, s := range f.Specs {
		if s.Name == "scheduled_start" {
			return s
		}
	}
	t.Fatal("the editor must carry a scheduled-start field")
	return kit2.FieldSpec{}
}

// THE FIELD IS THE NEW KIND, AND CARRIES NO PRESET LIST.
//
// Asserting the ABSENCE of the options is the load-bearing half: a preset list reintroduced beside the
// picker would rebuild the exact control the operator rejected, and the picker would still work — so
// nothing else would notice.
func TestScheduledStartIsADateTimeFieldWithNoPresets(t *testing.T) {
	_, f := openItemEditor(t, nil)
	s := dtSpec(t, f)

	if s.Kind != kit2.KDateTime {
		t.Errorf("the scheduled-start field is kind %q, want the date-time kind whose control is the "+
			"combined calendar and clock", s.Kind)
	}
	if len(s.Options) != 0 {
		t.Errorf("the field still offers %d canned times (%v) — the canned times ARE the complaint",
			len(s.Options), s.Options)
	}
	if s.Display == nil {
		t.Error("the field has no Display hook, so it would show the raw RFC3339 wire value rather than a " +
			"local time with its zone")
	}
	if s.Validate == nil {
		t.Error("the field must still validate: a stored value can arrive from anywhere")
	}
}

// ENTER OPENS THE CALENDAR, SEEDED FROM THE ITEM.
//
// The seed is the half that quietly breaks: without it the modal opens on NOW, so an operator editing a
// scheduled item is shown a time they never chose and can easily save it back.
func TestScheduledStartOpensTheCalendarSeededFromTheItem(t *testing.T) {
	start := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	m, f := openItemEditor(t, &start)

	if !f.FocusName("scheduled_start") {
		t.Fatal("fixture: the field is not focusable")
	}
	tap(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.dtPicker == nil {
		t.Fatal("enter on the scheduled-start field must open the calendar + clock; the field's host hook " +
			"is not installed, which reads to the operator as a field that does nothing")
	}
	if got := m.dtPicker.Time(); !got.Equal(start.In(time.Local)) {
		t.Errorf("the calendar opened on %v, want the item's existing scheduled start %v", got, start)
	}
}

// A PICKED MOMENT REACHES THE FIELD — through the INLINE detail editor, which is the path the operator
// uses and the one where the host form is Base.DetailForm() rather than Model.form.
func TestPickingAnInstantWritesItIntoTheScheduledStartField(t *testing.T) {
	m, f := openItemEditor(t, nil)
	if !f.FocusName("scheduled_start") {
		t.Fatal("fixture: the field is not focusable")
	}
	tap(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.dtPicker == nil {
		t.Fatal("fixture: the calendar did not open")
	}

	// Move off the seed, so the assertion cannot pass on an untouched value: a day forward, an hour on,
	// then five minutes.
	tap(t, m, tea.KeyMsg{Type: tea.KeyRight})                     // day +1
	tap(t, m, tea.KeyMsg{Type: tea.KeyTab})                       // to the clock
	tap(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")}) // hours
	tap(t, m, tea.KeyMsg{Type: tea.KeyUp})                        // +1 hour
	tap(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")}) // minutes
	tap(t, m, tea.KeyMsg{Type: tea.KeyShiftUp})                   // +5 minutes

	// Captured BEFORE the commit, because committing closes the modal.
	want := m.dtPicker.Time()
	tap(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.dtPicker != nil {
		t.Fatal("committing must close the modal")
	}
	live := m.ActiveForm()
	if live == nil {
		t.Fatal("committing the calendar must not close the editor it was opened from")
	}
	got := live.Values["scheduled_start"]
	if got == "" {
		t.Fatal("the picked instant never reached the field — the calendar committed and changed nothing")
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("the field holds %q, which is not RFC3339: %v", got, err)
	}
	if !parsed.Equal(want) {
		t.Errorf("the field holds %v (%q), want the picked instant %v", parsed, got, want)
	}
}

// ESC CANCELS WITHOUT TOUCHING THE FIELD. Without this, dismissing the calendar would silently reschedule
// the item to whatever the cursor happened to be sitting on.
func TestCancellingTheCalendarLeavesTheScheduledStartAlone(t *testing.T) {
	start := time.Now().Add(48 * time.Hour).Truncate(time.Minute)
	m, f := openItemEditor(t, &start)
	before := f.Values["scheduled_start"]
	if before == "" {
		t.Fatal("fixture: the field is not seeded from the item's scheduled start")
	}

	if !f.FocusName("scheduled_start") {
		t.Fatal("fixture: the field is not focusable")
	}
	tap(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	tap(t, m, tea.KeyMsg{Type: tea.KeyRight}) // move the cursor somewhere else
	tap(t, m, tea.KeyMsg{Type: tea.KeyDown})
	tap(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.dtPicker != nil {
		t.Fatal("esc must close the modal")
	}
	if got := f.Values["scheduled_start"]; got != before {
		t.Errorf("esc changed the field: %q -> %q", before, got)
	}
}

// THE FIELD READS BACK AS LOCAL TIME WITH ITS ZONE.
//
// Asserting that the string contains "UTC" is deliberate and machine-independent: the full form always
// names the zone — the abbreviation plus an offset, or the bare word "UTC" when that is the whole story —
// so this holds anywhere and fails the moment the display goes back to the raw wire value.
func TestTheScheduledStartReadsBackInLocalTimeWithItsZone(t *testing.T) {
	start := time.Now().Add(24 * time.Hour).Truncate(time.Minute)
	_, f := openItemEditor(t, &start)
	s := dtSpec(t, f)
	if s.Display == nil {
		t.Fatal("fixture: no Display hook")
	}
	raw := f.Values["scheduled_start"]
	if raw == "" {
		t.Fatal("fixture: the field is not seeded")
	}
	shown := s.Display(raw)

	if shown == raw {
		t.Fatalf("the field shows the raw wire value %q rather than a local time", raw)
	}
	if !strings.Contains(shown, "UTC") {
		t.Errorf("the shown value %q carries no zone at all, so the operator cannot tell which zone the "+
			"time is in", shown)
	}
	local := start.In(time.Local).Format("2006-01-02 15:04")
	if !strings.Contains(shown, local) {
		t.Errorf("shown = %q, want it to contain the LOCAL wall clock %q", shown, local)
	}
	// An unscheduled item says so, rather than rendering an empty row or an epoch.
	if got := s.Display(""); !strings.Contains(got, "not scheduled") {
		t.Errorf("an unscheduled field shows %q, want it to say it is not scheduled", got)
	}
}

// THE VALIDATOR ACCEPTS THE PICKER'S OWN OUTPUT. The form used to take a hand-typed value, so it required
// strict RFC3339; the picker now produces RFC3339 WITH A LOCAL OFFSET, and a validator that only accepted
// the trailing "Z" would reject every time the operator actually chose.
func TestScheduledStartValidatesRFC3339(t *testing.T) {
	if err := validateOptionalRFC3339(""); err != nil {
		t.Errorf("an unscheduled field must be valid: %v", err)
	}
	if err := validateOptionalRFC3339("2026-09-01T09:00:00Z"); err != nil {
		t.Errorf("a UTC timestamp must be accepted: %v", err)
	}
	if err := validateOptionalRFC3339("2026-09-01T09:00:00-04:00"); err != nil {
		t.Errorf("a local-offset timestamp must be accepted — it is the picker's own output: %v", err)
	}
	if err := validateOptionalRFC3339("tomorrow morning"); err == nil {
		t.Error("a malformed timestamp must be rejected")
	}
}

// ---------------------------------------------------------------------------------------------
// IS A LOCAL→UTC "CONVERTER" NEEDED? (No — and hand-rolling one is the classic way to break this.)
// ---------------------------------------------------------------------------------------------
//
// The proposal was a converter that "turns the user's system time into UTC so it all works nicely".
// The conversion is already happening, correctly, and has been the whole time — it is just not a step
// anybody wrote, because it is a property of the types:
//
//	time.Time IS an absolute instant. Its Location is a presentation detail attached to that instant,
//	not part of its value.
//	timestamppb.Timestamp IS seconds+nanos since the epoch — i.e. already UTC-normalised on the wire.
//
// So `timestamppb.New(central)` and `timestamppb.New(utc)` carry the SAME number for the same moment.
// There is no separate conversion to insert, and the tests below pin that — plus the failure mode a
// hand-rolled converter introduces, which is a DOUBLE SHIFT. That is the reason to write the proof
// rather than the converter: the naive version is not merely redundant, it is wrong.

// chicago is the operator's zone (they confirmed Central). Loaded by IANA name so CST and CDT are the
// REAL ones, DST included, rather than an offset someone typed once.
func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no zone database on this machine — the Central-time properties are unobservable here")
	}
	return loc
}

// THE WIRE VALUE IS THE INSTANT, NOT THE WALL CLOCK. The same moment written in Central and in UTC must
// reach the request as the same number of seconds — which is what makes a converter unnecessary.
func TestTheScheduledInstantIsAbsoluteWhateverZoneItWasChosenIn(t *testing.T) {
	loc := chicago(t)

	// ONE moment, two spellings. 09:00 CDT on 2026-07-15 is 14:00 UTC.
	central := time.Date(2026, time.July, 15, 9, 0, 0, 0, loc)
	utc := time.Date(2026, time.July, 15, 14, 0, 0, 0, time.UTC)
	if !central.Equal(utc) {
		t.Fatalf("fixture: 09:00 Central is not 14:00 UTC on 2026-07-15 (%v vs %v)", central, utc)
	}

	// The picker emits the LOCAL spelling with its offset (that is Value()); the field parses it.
	fromCentral := tsOrNil(central.Format(time.RFC3339))
	fromUTC := tsOrNil(utc.Format(time.RFC3339))
	if fromCentral == nil || fromUTC == nil {
		t.Fatal("the field must parse both spellings — they are both plain RFC3339")
	}
	if fromCentral.GetSeconds() != fromUTC.GetSeconds() {
		t.Fatalf("the wire value depends on the zone it was written in: Central → %d, UTC → %d. The "+
			"request carries an INSTANT, so these must be one and the same number.",
			fromCentral.GetSeconds(), fromUTC.GetSeconds())
	}
	// And it is the instant the operator meant, to the second.
	if got := fromCentral.AsTime(); !got.Equal(utc) {
		t.Errorf("the request would carry %v, want the chosen moment %v", got, utc)
	}
}

// CENTRAL MEANS CST **AND** CDT. This is the half a fixed "-6" would get wrong, and it is why the zone
// is carried as a NAME (time.Local / an IANA location) rather than as an offset baked in once.
func TestCentralTimeFollowsDaylightSaving(t *testing.T) {
	loc := chicago(t)

	// Both are 09:00 on the operator's wall clock, six months apart.
	winter := time.Date(2026, time.January, 15, 9, 0, 0, 0, loc)
	summer := time.Date(2026, time.July, 15, 9, 0, 0, 0, loc)

	if got := winter.UTC().Hour(); got != 15 {
		t.Errorf("09:00 Central in January is %02d:00 UTC, want 15:00 — CST is UTC-6", got)
	}
	if got := summer.UTC().Hour(); got != 14 {
		t.Errorf("09:00 Central in July is %02d:00 UTC, want 14:00 — CDT is UTC-5", got)
	}
	// Stated as the property that matters: the SAME wall clock is two different offsets, so any
	// implementation that hard-codes one of them is wrong for half the year.
	if winter.UTC().Equal(summer.UTC()) {
		t.Error("the same wall clock produced the same instant in January and July — the zone is being " +
			"treated as a fixed offset, which reintroduces an hour of drift every spring and autumn")
	}
	name, _ := winter.Zone()
	nameSummer, _ := summer.Zone()
	if name == nameSummer {
		t.Errorf("January and July both report the zone name %q; Central should read CST in winter and "+
			"CDT in summer", name)
	}
}

// THE CONVERTER WOULD DOUBLE SHIFT. This is the reason the proof is worth writing: reinterpreting a
// wall clock as UTC is the natural-looking implementation of "convert system time to UTC", and it moves
// the chosen time by the zone offset. An operator scheduling 09:00 would get 04:00.
func TestAHandRolledZoneConversionWouldDoubleShift(t *testing.T) {
	loc := chicago(t)

	// 09:00 CDT = 14:00 UTC. This IS the value the request should carry, with nothing done to it.
	central := time.Date(2026, time.July, 15, 9, 0, 0, 0, loc)
	want := tsOrNil(central.Format(time.RFC3339))

	// The tempting "converter": keep the hours and minutes, call them UTC.
	naive := time.Date(central.Year(), central.Month(), central.Day(),
		central.Hour(), central.Minute(), 0, 0, time.UTC)
	got := tsOrNil(naive.Format(time.RFC3339))

	if got.GetSeconds() == want.GetSeconds() {
		t.Fatal("fixture: the naive reinterpretation changed nothing, so it proves nothing")
	}
	drift := got.AsTime().Sub(want.AsTime())
	if drift != -5*time.Hour {
		t.Errorf("the naive conversion moved the time by %v, want -5h (the CDT offset)", drift)
	}
	// Said the way the operator would experience it: they asked for 09:00 and it would fire at 04:00.
	if h := got.AsTime().In(loc).Hour(); h != 4 {
		t.Errorf("a hand-rolled conversion turns 09:00 into %02d:00 Central; the fire time would be wrong "+
			"by the zone offset", h)
	}
	// The correct form is a no-op, which is the whole point: the value is already the instant.
	if central.UTC() != want.AsTime().UTC() {
		t.Error("stating the instant in UTC must not change it")
	}
}

// AND THE PICKED INSTANT IS THE ONE SENT. The field flows through the ordinary submit path, so this pins
// the whole chain end to end rather than the two halves separately — and it uses a LOCAL OFFSET, which is
// the new thing worth pinning: a non-"Z" value must survive as the correct instant.
func TestThePickedInstantIsTheOneSentOnSave(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))

	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	f.Set("scheduled_start", "2026-09-01T09:00:00-04:00")
	run(t, m, press(t, m, "ctrl+s"))

	if len(p.updated) == 0 {
		t.Fatal("saving the editor must send an update")
	}
	req := p.updated[len(p.updated)-1]
	if req.GetScheduledStartAt() == nil {
		t.Fatal("the scheduled start was dropped from the request")
	}
	want := time.Date(2026, time.September, 1, 9, 0, 0, 0, time.FixedZone("", -4*3600))
	if got := req.GetScheduledStartAt().AsTime(); !got.Equal(want) {
		t.Errorf("scheduled start sent as %v, want the instant %v (-04:00 must be honoured, not discarded)",
			got, want)
	}
}
