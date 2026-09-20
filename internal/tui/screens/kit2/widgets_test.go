package kit2

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/mutate"
)

// fakeSink is the dock feedback surface for tests.
type fakeSink struct {
	progress, fail, notice []string
}

func (s *fakeSink) Progress(m string) { s.progress = append(s.progress, m) }
func (s *fakeSink) Fail(m string)     { s.fail = append(s.fail, m) }
func (s *fakeSink) Notice(m string)   { s.notice = append(s.notice, m) }

func dims(t *testing.T, name, s string, w, h int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	if len(lines) != h {
		t.Fatalf("%s: got %d lines, want %d", name, len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Fatalf("%s: line %d width %d, want %d (%q)", name, i, got, w, l)
		}
	}
}

// --- Panel: exact h×w at both floor sizes -------------------------------

func TestPanelExactSize(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		p := NewPanel("Workers", sz[0], sz[1])
		p.SetContent("alpha\nbeta")
		dims(t, "panel", p.View(), sz[0], sz[1])
	}
}

func TestSplitWidthsTilesExactly(t *testing.T) {
	ws := SplitWidths(80, 3, 1)
	sum := 0
	for _, w := range ws {
		sum += w
	}
	if sum+2 != 80 {
		t.Fatalf("split does not tile 80: %v (sum %d + gaps)", ws, sum)
	}
}

// --- Focus model: Tab / Shift+Tab / mouse agree -------------------------

func TestFocusKeysAndMouseAgree(t *testing.T) {
	f := NewFocus("list", "actions", "detail")
	if f.Current() != "list" {
		t.Fatal("initial focus")
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if !f.IsFocused("actions") {
		t.Fatal("tab did not advance")
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	if !f.IsFocused("list") {
		t.Fatal("shift+tab did not retreat")
	}
	// Mouse click addresses the SAME region identity the ring uses.
	if !f.Click("detail") || !f.IsFocused("detail") {
		t.Fatal("click did not focus region")
	}
	f.Next() // wraps detail -> list
	if !f.IsFocused("list") {
		t.Fatal("ring did not wrap")
	}
}

func TestBaseKeysAndMouseAgreeOnRegion(t *testing.T) {
	b := &Base{}
	b.AddSource("workers", "Workers", nil)
	b.SetSize(80, 24)

	// TAB must NOT toggle the pane: it used to share the enter/space case, which
	// made it a list↔detail focus toggle that the SCREEN consumed — so the shell's
	// tab-bar ring never saw the key. On the Work tab the operator's report was
	// "the tabbing between the left and right pane breaks the tab path in the top
	// of the tab menu and grabs focus". Tab is now left alone for the global ring.
	if handled, _ := b.updateKeyHandled(t, "tab"); handled {
		t.Fatal("tab must fall through to the shell's focus ring, not be consumed by the pane")
	}

	// SHIFT+TAB must ALSO fall through. It used to cycle this screen's region ring
	// in reverse, which consumed it on every kit2 screen — so the tab bar could not
	// be walked backwards from any pane ("Projects screen is stealing shift+tab").
	if handled, _ := b.updateKeyHandled(t, "shift+tab"); handled {
		t.Fatal("shift+tab must fall through to the shell's reverse focus ring")
	}

	// LEFT/RIGHT move focus BETWEEN THE TWO PANES — not the tab bar, and no longer
	// source cycling (that is h/l now).
	b.updateKey(t, "right")
	if b.FocusedRegion() != "detail" {
		t.Fatalf("right: region %q, want detail — arrows move between the panes", b.FocusedRegion())
	}
	b.updateKey(t, "left")
	if b.FocusedRegion() != "workers" {
		t.Fatalf("left: region %q, want workers — arrows move between the panes", b.FocusedRegion())
	}

	// ENTER still toggles list↔detail (activation is the pane's job).
	b.updateKey(t, "enter")
	if b.FocusedRegion() != "detail" {
		t.Fatalf("enter: region %q, want detail", b.FocusedRegion())
	}
	b.updateKey(t, "enter")
	if b.FocusedRegion() != "workers" {
		t.Fatalf("enter back: region %q, want workers", b.FocusedRegion())
	}
	// Mouse into the detail column focuses detail (same identity).
	b.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 79, Y: 5})
	if b.FocusedRegion() != "detail" {
		t.Fatalf("click: region %q, want detail", b.FocusedRegion())
	}
}

func (b *Base) updateKey(t *testing.T, key string) {
	t.Helper()
	k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "tab":
		k = tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		k = tea.KeyMsg{Type: tea.KeyShiftTab}
	}
	b.Update(k)
}

// updateKeyHandled feeds one key and reports whether the Base CONSUMED it.
func (b *Base) updateKeyHandled(t *testing.T, key string) (bool, tea.Cmd) {
	t.Helper()
	k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "tab":
		k = tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		k = tea.KeyMsg{Type: tea.KeyEnter}
	case "shift+tab":
		k = tea.KeyMsg{Type: tea.KeyShiftTab}
	}
	return b.Update(k)
}

// --- Dialog: overlay never breaks the h×w viewport contract -------------

func TestDialogOverlayKeepsViewportContract(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		base := FitLines(strings.Repeat("base row\n", sz[1]), sz[0], sz[1])
		d := Confirm("Disable webhook?", "this stops deliveries", "disable")
		d.Danger = true
		box := d.Box(40, 8)
		out := Center(base, box, sz[0], sz[1])
		dims(t, "dialog overlay", out, sz[0], sz[1])
		if !strings.Contains(out, "Disable webhook?") {
			t.Fatal("dialog title missing from overlay")
		}
	}
}

func TestDialogKeyResolution(t *testing.T) {
	d := Confirm("x", "y", "delete")
	if _, done := d.HandleKey(tea.KeyMsg{Type: tea.KeyTab}); done {
		t.Fatal("tab should not resolve the dialog")
	}
	if choice, done := d.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab}); done || choice != "" {
		t.Fatal("shift+tab should only move the selection")
	}
	choice, done := d.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !done || choice != "delete" {
		t.Fatalf("enter chose %q (done=%v), want delete", choice, done)
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if c, done := d.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}); !done || c != "cancel" {
		t.Fatalf("tab+enter chose %q, want cancel", c)
	}
	d2 := Confirm("x", "y", "delete")
	if choice, done := d2.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); !done || choice != "" {
		t.Fatalf("esc should dismiss (got %q done=%v)", choice, done)
	}
}

// --- Stream: offset preserved, follow only at bottom -------------------

func TestStreamPreservesOffsetWhileAppending(t *testing.T) {
	s := NewStream("Activity", 40, 5)
	for i := 0; i < 40; i++ {
		s.Append("line")
	}
	s.Wheel(-10) // operator scrolls back
	want := s.Offset
	if s.AtBottom() {
		t.Fatal("expected to be scrolled off the bottom")
	}
	s.Append("a new arrival")
	if s.Offset != want {
		t.Fatalf("append yanked offset %d -> %d", want, s.Offset)
	}
	// Follows only when already at the bottom.
	s.ScrollToBottom()
	s.Append("tail")
	if !s.AtBottom() {
		t.Fatal("did not auto-follow while pinned to the bottom")
	}
	last := s.Lines[len(s.Lines)-1]
	if last != "tail" {
		t.Fatalf("last line %q", last)
	}
}

// --- Table: selection, sorting, tree expansion -------------------------

func TestTableSortSelectExpand(t *testing.T) {
	tbl := NewTable("Items", Column{Title: "name"})
	tbl.SetRows([]Row{
		{ID: "c", Cells: []string{"cobra"}},
		{ID: "a", Cells: []string{"ant"}},
		{ID: "b", Cells: []string{"bee"}},
	})
	tbl.SortBy(0)
	if tbl.Rows[0].ID != "a" {
		t.Fatalf("sort asc: first row %q", tbl.Rows[0].ID)
	}
	tbl.SortBy(0) // toggle
	if tbl.Rows[0].ID != "c" {
		t.Fatalf("sort desc: first row %q", tbl.Rows[0].ID)
	}
	tbl.SetRows([]Row{
		{ID: "p", Cells: []string{"parent"}, Expand: true},
		{ID: "k", Cells: []string{"child"}, Parent: "p", Depth: 1},
	})
	if len(tbl.VisibleRows()) != 1 {
		t.Fatalf("collapsed tree should hide the child")
	}
	if !tbl.Toggle() {
		t.Fatal("toggle on a expandable node failed")
	}
	if len(tbl.VisibleRows()) != 2 {
		t.Fatalf("expanded tree should show the child")
	}
	tbl.SetRows([]Row{{ID: "x"}, {ID: "y"}})
	tbl.Click(1)
	if tbl.SelectedID() != "y" {
		t.Fatalf("click select got %q", tbl.SelectedID())
	}
}

// --- Form: typed fields + submit through the mutation layer ------------

func TestFormSubmitsThroughMutationLayer(t *testing.T) {
	var rpcName, rpcValue, rpcScope string
	ex := &mutate.Executor{Sink: &fakeSink{}}
	form := NewForm("New secret",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Required: true},
		FieldSpec{Name: "value", Label: "Value", Kind: KSecret, Required: true},
		FieldSpec{Name: "scope", Label: "Scope", Kind: KSelect, Options: []Option{{Value: "tenant"}, {Value: "global"}}},
		FieldSpec{Name: "notes", Label: "Notes", Kind: KTextArea},
		FieldSpec{Name: "ttl", Label: "TTL", Kind: KNumber, Initial: "30"},
		FieldSpec{Name: "enabled", Label: "Enabled", Kind: KCheckbox, Initial: "true"},
		FieldSpec{Name: "tags", Label: "Tags", Kind: KMultiSelect, Options: []Option{{Value: "prod"}, {Value: "staging"}}},
		FieldSpec{Name: "meta", Label: "Meta", Kind: KJSON},
		FieldSpec{Name: "cfg", Label: "Config", Kind: KYAML},
	)
	var multiTags []string
	form.OnSubmit = func(v map[string]string, m map[string][]string) (tea.Cmd, error) {
		multiTags = m["tags"]
		// The write goes through the mutation layer, never a direct RPC.
		ex.RunSync(mutate.Request{
			Name: "create secret", Source: "secrets",
			Do: func(ctx context.Context) error {
				rpcName, rpcValue, rpcScope = v["name"], v["value"], v["scope"]
				return nil
			},
		})
		return nil, nil
	}
	form.Set("name", "API_KEY")
	form.Set("value", "s3cr3t")
	form.Set("scope", "global")
	form.Set("notes", "rotated weekly")
	form.SetMulti("tags", "prod", true)
	form.Set("meta", `{"a":1}`)
	form.Set("cfg", "key: value")
	if _, err := form.Submit(); err != nil {
		t.Fatalf("submit: %v (errors %v)", err, form.Errors)
	}
	if rpcName != "API_KEY" || rpcValue != "s3cr3t" || rpcScope != "global" {
		t.Fatalf("RPC did not fire with the field values: name=%q value=%q scope=%q", rpcName, rpcValue, rpcScope)
	}
	if len(multiTags) != 1 || multiTags[0] != "prod" {
		t.Fatalf("multi-select values not carried: %v", multiTags)
	}
}

func TestFormValidationAndSecretMasking(t *testing.T) {
	form := NewForm("x",
		FieldSpec{Name: "name", Kind: KText, Required: true},
		FieldSpec{Name: "value", Kind: KSecret, Required: true},
		FieldSpec{Name: "ttl", Kind: KNumber},
		FieldSpec{Name: "meta", Kind: KJSON},
	)
	form.Focused = true
	form.Width = 40
	if form.Validate() {
		t.Fatal("required fields should fail validation")
	}
	form.Set("name", "n")
	form.Set("value", "topsecret")
	form.Set("ttl", "abc")
	form.Set("meta", "{bad json")
	if form.Validate() {
		t.Fatal("bad number/json should fail")
	}
	if form.Errors["ttl"] != "not a number" || form.Errors["meta"] != "invalid JSON" {
		t.Fatalf("errors: %v", form.Errors)
	}
	view := form.View()
	if strings.Contains(view, "topsecret") {
		t.Fatal("secret value was rendered")
	}
	if !strings.Contains(view, "••••") {
		t.Fatal("secret should render a mask")
	}
	// Typing edits the focused field and tab moves it.
	form.FocusName("name")
	form.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if form.Values["name"] != "nx" {
		t.Fatalf("typing: %q", form.Values["name"])
	}
	form.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if form.CurrentName() != "value" {
		t.Fatalf("tab focus %q", form.CurrentName())
	}
}

// --- Action: confirm + optimistic apply + rollback ----------------------

func TestActionConfirmOptimisticRollback(t *testing.T) {
	model := []string{"a", "b", "c"}
	sink := &fakeSink{}
	ex := &mutate.Executor{Sink: sink}
	rpcerr := errors.New("permission_denied: token lacks control:write")
	act := Action{
		Label: "disable", Key: "x", Danger: true, Source: "webhooks",
		Confirm:  "Disable this webhook?",
		Apply:    func() { model = []string{"a", "c"} },
		Rollback: func() { model = []string{"a", "b", "c"} },
		Do:       func(ctx context.Context) error { return rpcerr },
	}
	if !act.NeedsConfirm() {
		t.Fatal("action should require confirmation")
	}
	applied, do := act.Invoke()
	if !applied || len(model) != 2 {
		t.Fatalf("optimistic apply did not run: %v", model)
	}
	if err := do(); err == nil {
		t.Fatal("expected the RPC to fail")
	}
	ex.Apply(mutate.Result{Name: act.Label, Source: act.Source, Err: rpcerr, Rollback: act.Rollback})
	if len(model) != 3 || model[1] != "b" {
		t.Fatalf("rollback did not restore the local model: %v", model)
	}
	if len(sink.fail) == 0 || !strings.Contains(sink.fail[0], "permission_denied") {
		t.Fatalf("failure text not surfaced in the dock: %v", sink.fail)
	}
}

func TestActionSuccessReconcilesSource(t *testing.T) {
	sink := &fakeSink{}
	reconciled := ""
	ex := &mutate.Executor{Sink: sink, Reconcile: func(src string) tea.Cmd { reconciled = src; return nil }}
	ex.RunSync(mutate.Request{Name: "create", Source: "secrets", Do: func(ctx context.Context) error { return nil }})
	if reconciled != "secrets" {
		t.Fatalf("reconcile source %q", reconciled)
	}
	if len(sink.notice) == 0 {
		t.Fatal("success notice missing")
	}
}

// --- Every widget renders at 80×24 and 120×40 --------------------------

func TestAllWidgetsRenderAtFloorSizes(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		w, h := sz[0], sz[1]
		p := NewPanel("Panel", w, h)
		p.SetContent("x")
		dims(t, "panel", p.View(), w, h)

		tbl := NewTable("Table", Column{Title: "name"}, Column{Title: "status"})
		tbl.Width, tbl.Height = w-2, h-2
		tbl.SetRows([]Row{{ID: "1", Cells: []string{"a", "ok"}}, {ID: "2", Cells: []string{"b", "warn"}}})
		dims(t, "table", FitLines(tbl.View(), w, h), w, h)

		form := NewForm("Form", FieldSpec{Name: "n", Kind: KText})
		form.Width = w - 4
		dims(t, "form", FitLines(form.View(), w, h), w, h)

		st := NewStream("Stream", w-4, h-6)
		st.Append("hello", "world")
		dims(t, "stream", FitLines(st.View(), w-4, h-6), w-4, h-6)

		bar := NewActionBar(Action{Label: "run", Key: "r"}, Action{Label: "del", Key: "d", Danger: true})
		bar.View()

		d := Confirm("t", "b", "ok")
		dims(t, "dialog", d.Box(min(w, 60), 10), min(w, 60), 10)
	}
}

// Regression: with HideSources the detail pane fills the region, so a click in
// the LEFT half must not select a row in the (invisible) source table.
//
// View() renders the detail only, but mouseRegion kept a two-pane split, so any
// left-half click resolved to "the focused source pane", ran table.Click(row)
// against a table nobody can see, and called loadDetail. On the Ask tab that
// table is the conversations list — so clicking the launch page silently OPENED
// an arbitrary old conversation instead of doing nothing.
func TestHiddenSourcesClickDoesNotSelectAnInvisibleRow(t *testing.T) {
	b := &Base{}
	b.AddSource("conversations", "Conversations", nil)
	b.SetSize(120, 40)
	b.LoadItems("conversations", []Item{
		{ID: "conv-1", Title: "hello"},
		{ID: "conv-2", Title: "second"},
	}, "")
	b.HideSources = true

	// The precise contract: every column belongs to the detail.
	for _, x := range []int{0, 10, 40} {
		if idx, isDetail := b.mouseRegion(x); idx != -1 || !isDetail {
			t.Fatalf("x=%d resolved to (idx=%d, detail=%v); with sources hidden it must be the detail",
				x, idx, isDetail)
		}
	}

	// And behaviourally, at a Y that REALLY maps onto a row (the earlier version
	// of this test clicked a row index outside the table, so it passed even with
	// the bug present).
	before := b.ActiveTable().SelectedID()
	clickY := b.tableTopRow() // the first data row
	if clickY >= b.height {
		t.Fatalf("fixture: click row %d is off-screen (height %d)", clickY, b.height)
	}
	b.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 10, Y: clickY,
	})
	if got := b.ActiveTable().SelectedID(); got != before {
		t.Fatalf("a click on the launch page selected %q in an INVISIBLE table (was %q)", got, before)
	}
}

// DetailWidth must agree with the pane the detail is actually drawn in, the
// moment the layout is applied — not one render later.
//
// HideSources renders the detail at the FULL width, while SetSize used to store
// the SPLIT width and leave the correction to the next render. Callers use
// DetailWidth() as their WRAP width, so until that render ran the content was
// laid out for a different pane than it was drawn in: a wrap width wider than
// the pane gets truncated at the right edge (cutting a right-aligned line — an
// operator's own chat message — down to its leading whitespace), and a narrower
// one wastes the pane.
func TestDetailWidthMatchesTheRenderedPaneWhenSourcesAreHidden(t *testing.T) {
	b := &Base{HideSources: true}
	b.width, b.height = 80, 20
	b.AddSource("conversations", "Conversations", func(ctx context.Context, page string) ([]Item, string, error) {
		return nil, "", nil
	})
	b.SetSize(80, 20)
	if got := b.DetailWidth(); got != 78 {
		t.Fatalf("DetailWidth = %d, want 78 (the full-width pane's inner width); a split width here is what truncated right-aligned content", got)
	}
	// And the rendered pane agrees.
	p := NewPanel("Detail", b.width, b.height)
	p.SetContent("x")
	if got, want := p.innerW(), b.DetailWidth(); got != want {
		t.Fatalf("panel innerW = %d but DetailWidth = %d — the wrap width and the pane disagree", got, want)
	}
}

// ctrl+e expands the focused free-text field so its value WRAPS instead of being
// edited one horizontally-windowed line at a time.
//
// The operator: "we need a way to expand out fields like description, Behavior,
// etc. so we can see more of it when we are typing in our changes." A windowed
// line shows a slice; expanded shows what they are writing.
func TestExpandedFieldWrapsItsWholeValue(t *testing.T) {
	long := strings.Repeat("word ", 40) // far wider than the form
	f := &Form{
		Specs: []FieldSpec{
			{Name: "title", Label: "Title", Kind: KText},
			{Name: "behavior", Label: "Behavior", Kind: KTextArea, Initial: long},
		},
		Values:  map[string]string{"title": "t", "behavior": long},
		Focused: true,
		Width:   60,
		pos:     map[string]int{},
	}
	// Focus the textarea and expand it.
	f.Cursor = 1
	if cmd, _ := f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE}); cmd != nil {
		t.Fatal("expanding needs no command")
	}
	if f.Expanded() != "behavior" {
		t.Fatalf("Expanded() = %q, want the focused field", f.Expanded())
	}

	collapsed := f.View()
	rows := 0
	for _, l := range strings.Split(collapsed, "\n") {
		if strings.Contains(l, "word") {
			rows++
		}
	}
	if rows < 2 {
		t.Fatalf("expanded field rendered %d value rows, want it wrapped across several:\n%s", rows, collapsed)
	}
	// The hint names the way back, so the mode is never a trap.
	if !strings.Contains(collapsed, "ctrl+e to collapse") {
		t.Fatalf("expanded view must name the collapse gesture:\n%s", collapsed)
	}

	// ctrl+e again collapses it.
	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if f.Expanded() != "" {
		t.Fatalf("Expanded() = %q, want collapsed", f.Expanded())
	}

	// A one-line kind is not expandable (extra rows would be useless).
	f.Cursor = 0
	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if f.Expanded() != "" {
		t.Fatalf("a KText field must not expand, got %q", f.Expanded())
	}
}

// Expanding must not disturb any other field's value.
func TestExpandingAFieldDoesNotDisturbOthers(t *testing.T) {
	f := &Form{
		Specs: []FieldSpec{
			{Name: "a", Label: "A", Kind: KTextArea, Initial: "alpha"},
			{Name: "b", Label: "B", Kind: KTextArea, Initial: "beta"},
		},
		Values:  map[string]string{"a": "alpha", "b": "beta"},
		Focused: true,
		Width:   60,
		pos:     map[string]int{},
	}
	f.Cursor = 0
	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if v := f.Values["b"]; v != "beta" {
		t.Fatalf("b = %q, want it untouched", v)
	}
	if v := f.Values["a"]; v != "alpha" {
		t.Fatalf("a = %q, want it untouched", v)
	}
}

// The cursor must step over EVERY visible row, including a second row that
// shares the first one's id.
//
// Every cursor operation used to route through the row's ID (cursorVis →
// setCursorToID), and that round trip is not injective: visIndexOf returns the
// FIRST match and setCursorToID seats the cursor on the FIRST match. So a step
// ONTO a duplicated id landed back on its twin and the cursor FROZE — the
// operator's "if you move the arrow key down to one of them, it highlights both
// work items and then will not let you continue to hit the down key to move past
// them". Distinct work items legitimately share a TITLE (3 items titled "test"
// in the live tenant), and the fetch layer can emit rows that collide by id, so
// movement must not depend on ID uniqueness.
func TestCursorStepsPastDuplicatedRowIDs(t *testing.T) {
	tb := &Table{Width: 40, Height: 20}
	tb.SetItems([]Item{
		{ID: "a", Title: "first"},
		{ID: "b", Title: "dup"},
		{ID: "b", Title: "dup"}, // the twin
		{ID: "c", Title: "after"},
	}, "")

	var visited []string
	for i := 0; i < 3; i++ {
		tb.Move(1)
		visited = append(visited, tb.Selected().Cells[0])
	}
	if tb.Cursor != 3 {
		t.Fatalf("cursor = %d after three steps, want 3 (the row after the twins); visited %v", tb.Cursor, visited)
	}
	if got := tb.Selected().Cells[0]; got != "after" {
		t.Fatalf("selected = %q, want the row past the duplicates", got)
	}
	// And the cursor can come back without getting stuck on the twins either.
	for i := 0; i < 3; i++ {
		tb.Move(-1)
	}
	if tb.Cursor != 0 {
		t.Fatalf("cursor = %d after stepping back, want 0", tb.Cursor)
	}
}
