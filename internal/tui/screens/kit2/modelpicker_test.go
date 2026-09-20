package kit2

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// forceColorForTest makes lipgloss emit real escape sequences. Under the test colour profile lipgloss
// strips ALL styling, so a styled and an unstyled render are byte-identical — which makes every
// assertion about colour or borders pass vacuously. That blindness is how the Ask rail's hardcoded
// `p.Focused = false` survived, so tests that assert on styling must force the profile AND assert
// that sequences came out at all.
func forceColorForTest(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// mpLoads records the load keys a picker asked for, so a test can assert the
// cascade order AND that nothing is fetched twice.
type mpLoads struct{ keys []string }

func (l *mpLoads) add(k string) {
	l.keys = append(l.keys, k)
}

func (l *mpLoads) last() string {
	if len(l.keys) == 0 {
		return ""
	}
	return l.keys[len(l.keys)-1]
}

func (l *mpLoads) count(k string) int {
	n := 0
	for _, key := range l.keys {
		if key == k {
			n++
		}
	}
	return n
}

// mpOutcome reads the modal's result the way a HOST does: Done gates everything,
// and only a commit carries a ref to apply.
func mpOutcome(mp *ModelPicker) (string, bool, bool) {
	return mp.Ref(), mp.Committed(), mp.Done()
}

// mpFixture builds a picker and its recording load hooks.
func mpFixture(t *testing.T) (*ModelPicker, *mpLoads) {
	t.Helper()
	mp := NewModelPicker("Select model")
	mp.PreferredAdapter = "orchicon"
	loads := &mpLoads{}
	mp.LoadAdapters = func() tea.Cmd { loads.add("adapters"); return nil }
	mp.LoadProviders = func(a string) tea.Cmd { loads.add("providers:" + a); return nil }
	mp.LoadModels = func(a, p string) tea.Cmd { loads.add("models:" + a + "/" + p); return nil }
	return mp, loads
}

// mpPress drives the picker with one key.
func mpPress(mp *ModelPicker, k tea.KeyMsg) tea.Cmd {
	_, cmd := mp.HandleKey(k)
	return cmd
}

func mpEnter(mp *ModelPicker) tea.Cmd { return mpPress(mp, tea.KeyMsg{Type: tea.KeyEnter}) }

// The operator: "select orchicon (first in the list) or opencode, then provider,
// then model with a search box as well pinned to the adapter/provider for model
// selection ... Then the text it displays after the model is selected is the
// adapter/provider/model name."
//
// So the walk is three deliberate steps and the committed value is the full
// three-segment ref.
func TestModelPickerWalksTheThreeTiersAndCommitsTheRef(t *testing.T) {
	mp, loads := mpFixture(t)

	// Nothing can be listed before the tier that scopes everything.
	mp.Open("", "", "")
	if got := loads.last(); got != "adapters" {
		t.Fatalf("the first load must be the adapter kinds, got %q", got)
	}

	// The Dispatcher reports opencode first; the picker must still list the
	// PREFERRED kind first and default to it.
	mp.SetAdapters([]string{"opencode", "orchicon"}, []string{"orchicon"})
	if got := mp.Adapter(); got != "orchicon" {
		t.Fatalf("a fresh selection must default to the preferred adapter, got %q", got)
	}
	mp.Sync()
	if got := loads.last(); got != "providers:orchicon" {
		t.Fatalf("the adapter tier must load its providers, got %q", got)
	}
	mp.SetProviders("orchicon", []PickerOption{
		{Value: "anthropic", Label: "anthropic"},
		{Value: "openai", Label: "openai"},
	})

	// Enter on the adapter advances to the provider tier (step two).
	mpEnter(mp)
	if mp.Tier() != TierProvider {
		t.Fatalf("choosing an adapter must advance to the provider tier, got %d", mp.Tier())
	}

	// Enter on the provider loads THAT provider's models (the search is pinned
	// to adapter+provider) and advances to the model tier.
	mpEnter(mp)
	if mp.Provider() != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", mp.Provider())
	}
	if got := loads.last(); got != "models:orchicon/anthropic" {
		t.Fatalf("the provider tier must load that provider's models, got %q", got)
	}
	mp.SetModels("orchicon", "anthropic", []PickerOption{
		{Value: "claude-sonnet-4", Label: "claude-sonnet-4", Meta: "200K ctx"},
		{Value: "claude-opus-4", Label: "claude-opus-4", Meta: "200K ctx"},
	}, false)

	// Enter on the model commits the full ref — the "name" the field displays.
	mpEnter(mp)
	if mp.Ref() != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("Ref() = %q, want the full adapter/provider/model", mp.Ref())
	}
	if !mp.Done() || !mp.Committed() {
		t.Fatalf("choosing the model must report Done+Committed to its host (done=%v committed=%v)", mp.Done(), mp.Committed())
	}
}

// The preferred adapter is listed first even when the Dispatcher reports it
// later — the operator's "select orchicon (first in the list)".
func TestModelPickerListsThePreferredAdapterFirst(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.SetAdapters([]string{"opencode", "orchicon"}, nil)
	if len(mp.adapters) != 2 || mp.adapters[0] != "orchicon" || mp.adapters[1] != "opencode" {
		t.Fatalf("adapters = %v, want orchicon first then opencode", mp.adapters)
	}
	// An unknown preferred kind is never invented.
	mp2 := NewModelPicker("x")
	mp2.PreferredAdapter = "not-registered"
	mp2.SetAdapters([]string{"opencode", "orchicon"}, nil)
	if mp2.adapters[0] != "opencode" {
		t.Fatalf("an unregistered preference must be a no-op, got %v", mp2.adapters)
	}
}

// Switching adapter RESCOPES the lower tiers: a stale provider/model must never
// carry into the new adapter's scope (ADR-0004 stale-selection guard).
func TestModelPickerAdapterChangeResetsLowerTiers(t *testing.T) {
	// No preference set, so the reported order is kept: [opencode, orchicon].
	mp := NewModelPicker("x")
	mp.LoadAdapters = func() tea.Cmd { return nil }
	mp.LoadProviders = func(string) tea.Cmd { return nil }
	mp.LoadModels = func(string, string) tea.Cmd { return nil }

	mp.Open("opencode", "anthropic", "claude-sonnet-4")
	mp.SetAdapters([]string{"opencode", "orchicon"}, nil)
	if mp.adapters[0] != "opencode" {
		t.Fatalf("precondition: adapters = %v", mp.adapters)
	}

	// Walk back to the adapter tier (tab cycles the ring) and choose the OTHER
	// kind.
	mpPress(mp, tea.KeyMsg{Type: tea.KeyTab})
	if mp.Tier() != TierAdapter {
		t.Fatalf("tab from the model tier must reach the adapter tier, got %d", mp.Tier())
	}
	mpPress(mp, tea.KeyMsg{Type: tea.KeyDown}) // opencode → orchicon
	mpEnter(mp)

	if mp.Adapter() != "orchicon" {
		t.Fatalf("adapter = %q, want orchicon", mp.Adapter())
	}
	if mp.Provider() != "" || mp.Model() != "" {
		t.Fatalf("carried a stale selection across adapters: provider=%q model=%q",
			mp.Provider(), mp.Model())
	}
}

// The model segment is the VERBATIM remainder of the grammar, so a slashed model
// id must survive the round trip (ADR-0003: opencode/commandcode/deepseek/deepseek-v4-flash).
func TestModelPickerRefKeepsASlashedModelVerbatim(t *testing.T) {
	mp := NewModelPicker("x")
	mp.Open("orchicon", "commandcode", "deepseek/deepseek-v4-flash")
	if got := mp.Ref(); got != "orchicon/commandcode/deepseek/deepseek-v4-flash" {
		t.Fatalf("Ref() = %q, want the model remainder verbatim", got)
	}
}

// A partial selection never fabricates a segment.
func TestModelPickerRefCollapsesEmptySegments(t *testing.T) {
	mp := NewModelPicker("x")
	mp.Open("orchicon", "", "")
	if got := mp.Ref(); got != "orchicon" {
		t.Fatalf("Ref() = %q, want just the chosen segment", got)
	}
}

// Backspace shortens the query and an empty result set is honest about it.
func TestModelPickerSearchNarrowsTheModelTier(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.SetAdapters([]string{"orchicon"}, nil)
	mp.Open("orchicon", "anthropic", "")
	mp.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}})
	mp.SetModels("orchicon", "anthropic", []PickerOption{
		{Value: "claude-sonnet-4", Label: "claude-sonnet-4"},
		{Value: "claude-opus-4", Label: "claude-opus-4"},
		{Value: "gpt-5", Label: "gpt-5"},
	}, false)

	// Typing always targets the model search, whatever tier was focused.
	mpPress(mp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("opus")})
	if mp.Tier() != TierModel {
		t.Fatalf("typing must focus the model tier, got %d", mp.Tier())
	}
	if got := len(mp.filteredModels()); got != 1 {
		t.Fatalf("filtered = %d models, want 1", got)
	}
	mpEnter(mp)
	if mp.Ref() != "orchicon/anthropic/claude-opus-4" {
		t.Fatalf("Ref() = %q, want the narrowed match", mp.Ref())
	}

	// A query matching nothing yields an empty list (never a silent whole-list).
	mp2, _ := mpFixture(t)
	mp2.Open("orchicon", "anthropic", "")
	mp2.SetModels("orchicon", "anthropic", []PickerOption{{Value: "gpt-5"}}, false)
	mp2.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}})
	mpPress(mp2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")})
	if got := len(mp2.filteredModels()); got != 0 {
		t.Fatalf("filtered = %d, want 0 for a non-matching query", got)
	}
	// Backspace restores it.
	mpPress(mp2, tea.KeyMsg{Type: tea.KeyBackspace})
	mpPress(mp2, tea.KeyMsg{Type: tea.KeyBackspace})
	mpPress(mp2, tea.KeyMsg{Type: tea.KeyBackspace})
	if got := len(mp2.filteredModels()); got != 1 {
		t.Fatalf("filtered = %d after clearing the query, want 1", got)
	}
}

// Sync is idempotent: a scope already loaded (or in flight) is never re-fetched
// — without this every keypress would fire another RPC.
func TestModelPickerSyncIsIdempotent(t *testing.T) {
	mp, loads := mpFixture(t)
	mp.Open("", "", "")
	mp.Sync()
	mp.Sync()
	if n := loads.count("adapters"); n != 1 {
		t.Fatalf("adapters load fired %d times, want 1", n)
	}

	mp.SetAdapters([]string{"orchicon"}, nil)
	mp.Sync()
	mp.Sync()
	if n := loads.count("providers:orchicon"); n != 1 {
		t.Fatalf("providers load fired %d times, want 1", n)
	}

	mp.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}})
	// Focus alone never loads: only a CHOICE cascades.
	mp.tier = TierProvider
	mp.cursor[TierProvider] = 0
	mp.Sync()
	if got := loads.count("models:orchicon/anthropic"); got != 0 {
		t.Fatalf("models must not load before a provider is CHOSEN, fired %d", got)
	}
	// Choosing the provider loads exactly once, however often Sync runs.
	mpEnter(mp)
	mp.Sync()
	mp.Sync()
	if got := loads.count("models:orchicon/anthropic"); got != 1 {
		t.Fatalf("models load fired %d times, want 1", got)
	}
}

// A load landing for a scope the operator has already left is DROPPED.
func TestModelPickerDropsAStaleLoad(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.SetAdapters([]string{"orchicon", "opencode"}, nil) // defaults to orchicon
	mp.SetProviders("opencode", []PickerOption{{Value: "someone-else"}})
	if len(mp.providers) != 0 {
		t.Fatalf("a provider list for another adapter must be dropped, got %v", mp.providers)
	}
	mp.SetModels("orchicon", "other", []PickerOption{{Value: "nope"}}, false)
	if len(mp.models) != 0 {
		t.Fatalf("a model list for another scope must be dropped, got %v", mp.models)
	}
}

// esc backs out without committing.
func TestModelPickerEscCancels(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.Open("orchicon", "anthropic", "")
	mpPress(mp, tea.KeyMsg{Type: tea.KeyEsc})
	if !mp.Done() {
		t.Fatal("esc must report Done, or the host cannot dismiss the modal")
	}
	if mp.Committed() {
		t.Fatal("esc must not commit")
	}
}

// A failed load is surfaced (never a silently blank list) and 'r' retries.
func TestModelPickerSurfacesALoadFailureAndRetries(t *testing.T) {
	mp, loads := mpFixture(t)
	mp.Open("", "", "")
	mp.SetLoadErr("model discovery is not configured")
	if mp.err == "" {
		t.Fatal("a failed load must be recorded, not swallowed")
	}
	before := len(loads.keys)
	mpPress(mp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if len(loads.keys) <= before {
		t.Fatal("r must retry the failed load")
	}
}

// The mouse is a first-class path: clicking a model ROW commits it, addressed
// against the SAME geometry the box was rendered at.
func TestModelPickerMouseClickCommitsAModel(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.SetScreen(100, 30)
	mp.SetAdapters([]string{"orchicon"}, nil)
	mp.Open("orchicon", "anthropic", "")
	mp.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}})
	mp.SetModels("orchicon", "anthropic", []PickerOption{
		{Value: "claude-sonnet-4", Label: "claude-sonnet-4"},
		{Value: "claude-opus-4", Label: "claude-opus-4"},
	}, false)

	// The box is centered; the first model row is interior row listTop, which is
	// one row below the border.
	left, top := mp.origin()
	_, hits := mp.layout(mp.boxW()-2, mp.boxH()-2)
	x := left + 3
	y := top + 1 + hits.listTop + 1 // the SECOND model row (index 1)

	handled, _ := mp.HandleMouse(tea.MouseMsg{
		X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	if !handled {
		t.Fatal("a click inside the picker must be handled")
	}
	if mp.Ref() != "orchicon/anthropic/claude-opus-4" {
		t.Fatalf("clicking a model row chose %q, want that row's model", mp.Ref())
	}
	if !mp.Done() || !mp.Committed() {
		t.Fatal("a model click must report Done+Committed so the host closes and writes")
	}

	// A click OUTSIDE the box is not ours (the host must still receive it).
	mp2, _ := mpFixture(t)
	mp2.SetScreen(100, 30)
	if handled, _ := mp2.HandleMouse(tea.MouseMsg{
		X: 0, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}); handled {
		t.Fatal("a click outside the box must not be handled by the picker")
	}
}

// The rendered box is a readable modal: every tier is labelled, the model list
// is present, and the key contract is stated.
func TestModelPickerViewRendersTheTiersAndTheKeyContract(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.SetScreen(100, 30)
	mp.SetAdapters([]string{"opencode", "orchicon"}, []string{"orchicon"})
	mp.Open("orchicon", "anthropic", "claude-sonnet-4")
	mp.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}, {Value: "openai"}})
	mp.SetModels("orchicon", "anthropic", []PickerOption{
		{Value: "claude-sonnet-4", Label: "claude-sonnet-4", Meta: "200K ctx"},
		{Value: "claude-opus-4", Label: "claude-opus-4", Meta: "200K ctx"},
	}, false)

	v := ansi.Strip(mp.View())
	for _, want := range []string{"ADAPTER", "PROVIDER", "orchicon", "opencode",
		"anthropic", "openai", "claude-sonnet-4", "search:", "esc: cancel"} {
		if !strings.Contains(v, want) {
			t.Errorf("the picker must render %q:\n%s", want, v)
		}
	}
	// The box must be exactly its declared size, or the host's Center() splice
	// shifts every row and the mouse mapping breaks.
	if got := len(strings.Split(mp.View(), "\n")); got != mp.boxH() {
		t.Fatalf("box rendered %d rows, want %d", got, mp.boxH())
	}
}

// The FORM hook: a KModel field renders the committed ref and its activation
// gesture opens the host's picker instead of advancing to the next field.
func TestFormModelFieldOpensThePicker(t *testing.T) {
	f := NewForm("Edit tenant settings",
		FieldSpec{Name: "default_worker_model", Label: "Default worker model", Kind: KModel,
			Initial: "opencode/anthropic/claude-sonnet-4"},
		FieldSpec{Name: "max_concurrent_runs", Label: "Max concurrent runs", Kind: KNumber, Initial: "4"},
	)
	f.Width = 70
	f.Focused = true
	f.FocusName("default_worker_model")

	var gotName, gotCurrent string
	f.OnOpenModelPicker = func(name, current string) tea.Cmd {
		gotName, gotCurrent = name, current
		return nil
	}

	_, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled {
		t.Fatal("enter on a model field must be consumed (it opens the picker)")
	}
	if gotName != "default_worker_model" || gotCurrent != "opencode/anthropic/claude-sonnet-4" {
		t.Fatalf("picker opened with (%q, %q), want the field name and its current ref",
			gotName, gotCurrent)
	}
	// Enter must NOT advance: a model field is a reference, not a text step.
	if f.CurrentName() != "default_worker_model" {
		t.Fatalf("enter advanced to %q; a model field must open the picker instead", f.CurrentName())
	}
	// The field displays the adapter/provider/model ref (the operator's "the
	// text it displays after the model is selected").
	if v := ansi.Strip(f.View()); !strings.Contains(v, "opencode/anthropic/claude-sonnet-4") {
		t.Fatalf("the field must display the committed ref:\n%s", v)
	}
}

// An UNSET model field still shows the affordance, so a blank row reads as
// "go choose one" rather than "not applicable".
func TestFormModelFieldShowsTheAffordanceWhenUnset(t *testing.T) {
	f := NewForm("Settings",
		FieldSpec{Name: "default_ask_model", Label: "Default ask model", Kind: KModel},
	)
	f.Width = 70
	f.Focused = true
	v := ansi.Strip(f.View())
	if !strings.Contains(v, "— none —") {
		t.Fatalf("an unset model field must show the affordance:\n%s", v)
	}
	if !strings.Contains(v, "enter: choose model") {
		t.Fatalf("the focused model field must state the gesture:\n%s", v)
	}
}

// The modal reports its OUTCOME to the host instead of closing itself through a
// callback. Both entry points are pinned here: enter on a model commits, esc
// cancels, and each one flips Done so the host can close the modal.
//
// This is the regression test for the modal that could not be dismissed. The
// picker used to call a `Commit`/`Cancel` closure that set the host's
// `modelPicker = nil`. Every host is a bubbletea model with value-receiver
// Update methods, so the closure captured the copy that was live when the modal
// opened — and the runtime replaces that copy on the next message. The closure's
// assignment therefore mutated a discarded model: enter and esc both appeared
// dead, with the modal stuck on screen forever.
func TestModelPickerReportsItsOutcomeToTheHost(t *testing.T) {
	mp, _ := mpFixture(t)
	mp.Open("orchicon", "anthropic", "")
	mp.SetProviders("orchicon", []PickerOption{{Value: "anthropic"}})
	mp.SetModels("orchicon", "anthropic", []PickerOption{{Value: "claude-sonnet-4"}}, false)
	mp.tier = TierModel

	if mp.Done() {
		t.Fatal("a freshly opened picker must not report Done")
	}
	mpEnter(mp)
	if !mp.Done() {
		t.Fatal("choosing a model must report Done so the host can close the modal")
	}
	if !mp.Committed() {
		t.Fatal("choosing a model must report Committed")
	}
	if ref, _, _ := mpOutcome(mp); ref != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("Ref = %q, want the chosen model", ref)
	}
	// esc is covered by TestModelPickerEscCancels; this one pins the COMMIT
	// outcome, which is what a host turns into a write.
}

// THE CHOSEN CHIP IS AN UNFILLED OUTLINE, NOT A FILLED BLOCK.
//
// The operator, on the standard LIGHT theme: "a grey or black background just looks odd and hard to
// look at as well ... Is it possible to maybe just have a box with a border color based in the theme

// THE CHOSEN CHIP IS PLAIN THEME TEXT WITH AN UNDERLINE.
//
// The operator, after two earlier attempts: "I think we should just make those normal theme text with
// an underline showing selection and avoid any kind of a background color or border at all."
//
// Attempt 1 was a FILLED chip (white on the Select fill — a near-black block on the light theme);
// attempt 2 was an OUTLINE (two border rules in the accent colour). Both read as a box rather than as a
// word, and both added cells the strip's width maths had to allow for. An underline is a text
// attribute: it marks the selection, costs no width, and cannot be a fill.
func TestPickerChipIsUnderlinedTextNotABox(t *testing.T) {
	forceColorForTest(t)

	// The TOKEN's own properties, so a future restyle has to be deliberate about all three.
	chip := theme.PickerChip.Render(" DeepSeekAPI ")
	if !strings.Contains(chip, "\x1b[4m") && !strings.Contains(chip, "4m") {
		t.Errorf("the chosen chip is not underlined: %q", chip)
	}
	if strings.Contains(chip, ";48;") {
		t.Errorf("the chosen chip carries a FILL — the operator asked for no background colour: %q", chip)
	}
	if strings.ContainsAny(chip, "│┌┐└┘─") {
		t.Errorf("the chosen chip is framed — the operator asked for no border at all: %q", chip)
	}
	if w := lipgloss.Width(chip); w != len(" DeepSeekAPI ") {
		t.Errorf("the chip renders %d cells for a %d-cell label — an underline must add no width, or the "+
			"strip's layout maths has to allow for a frame that is not there", w, len(" DeepSeekAPI "))
	}

	// AND THE PICKER USES IT. A token test alone is not a test of the picker: an earlier version of
	// this file asserted only the token, and kept passing when chipStrip was mutated back to the filled
	// style, because chipStrip's use of it was never exercised.
	mp := NewModelPicker("Ask model")
	mp.SetScreen(90, 30)
	mp.SetAdapters([]string{"orchicon"}, nil)
	mp.SetProviders("orchicon", []PickerOption{{Value: "DeepSeekAPI"}, {Value: "HalogenLocal"}})
	mp.SetModels("orchicon", "DeepSeekAPI", []PickerOption{{Value: "m"}}, false)
	mp.Open("orchicon", "DeepSeekAPI", "")

	var row string
	for _, l := range strings.Split(mp.View(), "\n") {
		if strings.Contains(ansi.Strip(l), "PROVIDER") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("no PROVIDER row rendered")
	}
	if !strings.Contains(row, theme.PickerChip.Render(" DeepSeekAPI ")) {
		t.Errorf("the picker does not render its chosen chip through theme.PickerChip:\n%q", row)
	}
	if strings.Contains(row, ";48;2;11;129;147") {
		t.Error("the chosen chip is filled with the Select colour again")
	}
}

// THE CHIP COSTS NO WIDTH, so a label that fits is never shortened by its own styling.
//
// The framed version needed two extra cells for its rules, and when the strip ran out of room it lost
// its CLOSING rule — an unclosed box reading as a rendering fault. With an underline there is no frame
// to lose.
func TestPickerChipCostsNoWidth(t *testing.T) {
	forceColorForTest(t)
	mp := NewModelPicker("Ask model")
	mp.SetScreen(90, 30)
	mp.SetAdapters([]string{"orchicon"}, nil)
	mp.SetProviders("orchicon", []PickerOption{{Value: "DeepSeekAPI"}, {Value: "HalogenLocal"}})
	mp.SetModels("orchicon", "DeepSeekAPI", []PickerOption{{Value: "m"}}, false)
	mp.Open("orchicon", "DeepSeekAPI", "")

	var row string
	for _, l := range strings.Split(mp.View(), "\n") {
		if strings.Contains(ansi.Strip(l), "PROVIDER") {
			row = ansi.Strip(l)
		}
	}
	for _, label := range []string{"DeepSeekAPI", "HalogenLocal"} {
		if !strings.Contains(row, label) {
			t.Errorf("the label %q is missing from the PROVIDER row, which has room for it: %s", label, row)
		}
	}
	// The row is exactly the box's inner width — a chip that cost cells would push it over.
	boxW, _ := mp.boxSize()
	if w := lipgloss.Width(row); w > boxW {
		t.Errorf("the strip renders %d cells inside a %d-cell box", w, boxW)
	}
}
