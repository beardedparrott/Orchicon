package kit2

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// PickerOption is one selectable entry in a model-picker tier.
//
// Value is what gets COMMITTED (an adapter kind, a provider id, or a model
// id); Label is the human text (defaults to Value); Meta is dim right-hand
// context (a provider hint, a context-window note) and is never part of the
// committed value.
type PickerOption struct {
	Value string
	Label string
	Meta  string
}

// Model-picker tiers, in the order the operator walks them.
const (
	TierAdapter = iota
	TierProvider
	TierModel
)

// ModelPicker is a dedicated MODAL control for choosing a model_ref in the
// pinned adapter/provider/model grammar (internal/adapter/modelref.go, ADR-0003).
//
// The operator chooses an ADAPTER, then a PROVIDER, then a MODEL by searching —
// the field then displays the full adapter/provider/model ref (the operator's
// "the text it displays after the model is selected is the adapter/provider/model
// name"). It replaces the plain-text model field, which asked an operator to
// type a ref no human can be expected to know.
//
// It is owned by the SCREEN, not by Base: the screen hosts it (so it can be
// layered over either a modal form or the inline detail editor), routes keys and
// mouse to it while open, and answers its load requests. kit2 therefore stays
// free of API dependencies — the picker never issues an RPC itself, it asks the
// screen to (Load* hooks), and the screen pushes results back in (Set*).
//
// The picker is also grammar-free: the screen parses the current ref
// (adapter.ParseModelRef) and hands Open the three segments; Ref() simply joins
// the chosen segments, so the pinned grammar has exactly ONE implementation.
type ModelPicker struct {
	title string

	// PreferredAdapter, when set and present in the loaded kinds, is listed
	// FIRST and used as the default for a fresh selection. The screens set it
	// to the native adapter kind per ADR-0005 D5 ("select orchicon (first in
	// the list) or opencode"). The picker never hardcodes a kind.
	PreferredAdapter string

	adapters   []string
	askCapable map[string]bool

	providers []PickerOption
	models    []PickerOption

	adapter  string
	provider string
	model    string

	// tier is the focused tier; cursor is the highlight within each tier's
	// list; query filters the MODEL tier (the search box, pinned to the
	// selected adapter+provider).
	tier   int
	cursor [3]int
	query  string

	// providersFor / modelsFor record WHICH adapter (and provider) the loaded
	// lists belong to, so Sync is idempotent: re-entering a scope never
	// re-fetches what is already loaded, and a stale load landing after the
	// operator moved on is dropped instead of poisoning the new scope.
	providersFor string
	modelsFor    string
	// inFlight is the one load currently outstanding.
	inFlight string

	loading  bool
	err      string
	degraded bool

	// Load hooks (set by the screen). Each returns the screen's RPC command.
	LoadAdapters  func() tea.Cmd
	LoadProviders func(adapter string) tea.Cmd
	LoadModels    func(adapter, provider string) tea.Cmd
	// done/committed record the modal's OUTCOME. The picker deliberately does
	// NOT close itself through a host callback.
	//
	// Why: every host is a bubbletea model with VALUE-receiver Update methods
	// (App.Update and the screens' Update all copy the model). A `Commit`/
	// `Cancel` closure capturing `m` therefore captures the COPY that was live
	// when the picker opened — and the runtime replaces that copy on the very
	// next message, so the closure's `m.modelPicker = nil` mutated a model
	// nobody renders any more. The modal then NEVER closed: enter and esc both
	// appeared dead while the RPC still fired (the operator's "when hitting
	// enter on deepseek-flash, the screen doesn't go away and I can't esc out of
	// it either").
	//
	// The host must instead read Done/Committed in the SAME Update that handled
	// the key, and clear its own field there — where its mutation lands on the
	// copy the runtime keeps.
	done      bool
	committed bool

	// screenW/screenH are the SCREEN content dimensions. The picker derives its
	// centered box from them with the same arithmetic as Center(), so mouse
	// hit-testing addresses what is actually on screen (the host centers the
	// box; the picker must agree where it landed).
	screenW, screenH int
}

// Done reports that the modal has finished and the host must CLOSE it (see the
// done/committed field comment for why the host owns that).
func (mp *ModelPicker) Done() bool { return mp.done }

// Committed reports whether Done was reached by CHOOSING a model (true) or by
// cancelling (false). Only meaningful once Done is true.
func (mp *ModelPicker) Committed() bool { return mp.committed }

// pickerHits records where each interactive row landed inside the box interior
// (0-based rows and columns), so clicks hit-test against the real layout.
type pickerHits struct {
	adapterRow  int
	adapterX    [][2]int // [x0,x1] per adapter index
	providerRow int
	providerX   [][2]int // [x0,x1] per provider index
	listTop     int
	listRows    int
	listOffset  int
}

// NewModelPicker builds an empty picker; the screen sets the hooks and calls
// Open to seed it from the current ref.
func NewModelPicker(title string) *ModelPicker {
	if title == "" {
		title = "Select model"
	}
	return &ModelPicker{title: title, askCapable: map[string]bool{}}
}

// SetScreen tells the picker the screen content size, so it can compute the
// same centered origin the host uses when it renders the box.
func (mp *ModelPicker) SetScreen(w, h int) { mp.screenW, mp.screenH = w, h }

// SetSize is an alias for SetScreen kept for call-site symmetry with the other
// widgets (Base.SetSize).
func (mp *ModelPicker) SetSize(w, h int) { mp.SetScreen(w, h) }

const (
	pickerBoxMaxW = 78
	pickerBoxMinW = 40
	pickerBoxMinH = 10
	pickerBoxRows = 18
)

// boxSize is the picker's box dimensions for the current screen.
func (mp *ModelPicker) boxSize() (w, h int) {
	w, h = pickerBoxMaxW, pickerBoxRows
	if mp.screenW > 0 && mp.screenW-4 < w {
		w = mp.screenW - 4
	}
	if w < pickerBoxMinW {
		w = pickerBoxMinW
	}
	if mp.screenH > 0 && mp.screenH-2 < h {
		h = mp.screenH - 2
	}
	if h < pickerBoxMinH {
		h = pickerBoxMinH
	}
	return w, h
}

// boxW / boxH are the box dimensions (the host and tests read them).
func (mp *ModelPicker) boxW() int { w, _ := mp.boxSize(); return w }
func (mp *ModelPicker) boxH() int { _, h := mp.boxSize(); return h }

// origin is the box's top-left cell — the same centering arithmetic as
// Center(), so mouse coordinates line up with the rendered box.
func (mp *ModelPicker) origin() (left, top int) {
	w, h := mp.boxSize()
	sw, sh := mp.screenW, mp.screenH
	if sw <= 0 {
		sw = w
	}
	if sh <= 0 {
		sh = h
	}
	left, top = (sw-w)/2, (sh-h)/2
	if left < 0 {
		left = 0
	}
	if top < 0 {
		top = 0
	}
	return left, top
}

// --- seeding + cascade ------------------------------------------------------

// Open seeds the picker from the current ref's three segments and issues the
// first load. The tier focus starts where the choice is INCOMPLETE (or on the
// model tier when the ref is complete — most edits are "change the model").
func (mp *ModelPicker) Open(adapter, provider, model string) tea.Cmd {
	mp.adapter, mp.provider, mp.model = adapter, provider, model
	switch {
	case model != "":
		mp.tier = TierModel
	case provider != "":
		mp.tier = TierProvider
	default:
		mp.tier = TierAdapter
	}
	return mp.Sync()
}

// Ref joins the chosen segments into the pinned adapter/provider/model ref.
// Empty segments collapse, so a partial selection never fabricates a segment.
func (mp *ModelPicker) Ref() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{mp.adapter, mp.provider, mp.model} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "/")
}

// Adapter / Provider / Model expose the current selection (tests + screens).
func (mp *ModelPicker) Adapter() string  { return mp.adapter }
func (mp *ModelPicker) Provider() string { return mp.provider }
func (mp *ModelPicker) Model() string    { return mp.model }

// Tier exposes the focused tier.
func (mp *ModelPicker) Tier() int { return mp.tier }

func modelKeyOf(adapter, provider string) string { return adapter + "\x00" + provider }

// Sync issues the load the current state is missing, if any, and reports the
// command. It is idempotent: a scope already loaded (or in flight) is never
// re-fetched, which is what keeps the cascade from firing duplicate RPCs on
// every keypress.
func (mp *ModelPicker) Sync() tea.Cmd {
	switch {
	case len(mp.adapters) == 0:
		return mp.begin("adapters", mp.LoadAdapters)
	case mp.adapter == "":
		return nil // waiting on the operator
	case mp.providersFor != mp.adapter:
		a := mp.adapter
		return mp.begin("providers:"+a, func() tea.Cmd { return mp.callProviders(a) })
	case mp.provider == "":
		return nil // waiting on the operator
	case mp.modelsFor != modelKeyOf(mp.adapter, mp.provider):
		a, p := mp.adapter, mp.provider
		return mp.begin("models:"+modelKeyOf(a, p), func() tea.Cmd { return mp.callModels(a, p) })
	}
	return nil
}

func (mp *ModelPicker) begin(key string, fn func() tea.Cmd) tea.Cmd {
	if fn == nil || mp.inFlight == key {
		return nil
	}
	mp.inFlight = key
	mp.loading = true
	mp.err = ""
	return fn()
}

func (mp *ModelPicker) callProviders(adapter string) tea.Cmd {
	if mp.LoadProviders == nil {
		return nil
	}
	return mp.LoadProviders(adapter)
}

func (mp *ModelPicker) callModels(adapter, provider string) tea.Cmd {
	if mp.LoadModels == nil {
		return nil
	}
	return mp.LoadModels(adapter, provider)
}

// SetAdapters installs the registered adapter kinds (with the Ask-capable
// subset). The PreferredAdapter is listed first; a fresh selection defaults to
// it, so the tier is never blank.
func (mp *ModelPicker) SetAdapters(kinds, askCapable []string) {
	mp.adapters = orderAdapters(dedupeStrings(kinds), mp.PreferredAdapter)
	mp.askCapable = map[string]bool{}
	for _, k := range askCapable {
		mp.askCapable[k] = true
	}
	mp.clearLoad()
	if mp.adapter == "" && len(mp.adapters) > 0 {
		mp.adapter = mp.adapters[0]
	}
	for i, k := range mp.adapters {
		if k == mp.adapter {
			mp.cursor[TierAdapter] = i
			break
		}
	}
}

// SetProviders installs a load's provider list. A list for an adapter the
// operator has since left is DROPPED (a stale load never poisons the new scope).
func (mp *ModelPicker) SetProviders(adapter string, opts []PickerOption) {
	if adapter != mp.adapter {
		return
	}
	mp.providers = opts
	mp.providersFor = adapter
	mp.clearLoad()
	mp.cursor[TierProvider] = 0
	if mp.provider != "" {
		for i, o := range opts {
			if o.Value == mp.provider {
				mp.cursor[TierProvider] = i
				break
			}
		}
	}
}

// SetModels installs a load's model list, highlighting the seeded model.
// A list for a scope the operator has since left is DROPPED.
func (mp *ModelPicker) SetModels(adapter, provider string, opts []PickerOption, degraded bool) {
	if adapter != mp.adapter || provider != mp.provider {
		return
	}
	mp.models = opts
	mp.modelsFor = modelKeyOf(adapter, provider)
	mp.degraded = degraded
	mp.clearLoad()
	mp.cursor[TierModel] = 0
	if mp.model != "" {
		for i, o := range mp.filteredModels() {
			if o.Value == mp.model {
				mp.cursor[TierModel] = i
				break
			}
		}
	}
}

// SetLoadErr records a failed load (a probe failure is surfaced, never a
// silently blank list) and clears the in-flight marker so 'r' can retry.
func (mp *ModelPicker) SetLoadErr(msg string) {
	mp.err = msg
	mp.loading = false
	mp.inFlight = ""
}

// Retry re-issues the missing load after a failure.
func (mp *ModelPicker) Retry() tea.Cmd {
	mp.err = ""
	mp.inFlight = ""
	return mp.Sync()
}

func (mp *ModelPicker) clearLoad() { mp.inFlight = ""; mp.loading = false; mp.err = "" }

// IsAskCapable reports whether the adapter kind supports Ask chat (the picker
// flags a non-Ask kind in Ask mode; ADR-0004 D1).
func (mp *ModelPicker) IsAskCapable(kind string) bool { return mp.askCapable[kind] }

// --- navigation -------------------------------------------------------------

// tierLen is the number of entries in the focused tier.
func (mp *ModelPicker) tierLen() int {
	switch mp.tier {
	case TierAdapter:
		return len(mp.adapters)
	case TierProvider:
		return len(mp.providers)
	default:
		return len(mp.filteredModels())
	}
}

// filteredModels is the model tier narrowed by the search query — matched on
// the id AND the label, so either the model id or its display name finds it.
func (mp *ModelPicker) filteredModels() []PickerOption {
	q := strings.ToLower(strings.TrimSpace(mp.query))
	if q == "" {
		return mp.models
	}
	out := make([]PickerOption, 0, len(mp.models))
	for _, o := range mp.models {
		if strings.Contains(strings.ToLower(o.Value), q) ||
			strings.Contains(strings.ToLower(o.Label), q) {
			out = append(out, o)
		}
	}
	return out
}

// clamp keeps the focused tier's cursor inside its list.
func (mp *ModelPicker) clamp() {
	n := mp.tierLen()
	if n == 0 {
		mp.cursor[mp.tier] = 0
		return
	}
	if mp.cursor[mp.tier] >= n {
		mp.cursor[mp.tier] = n - 1
	}
	if mp.cursor[mp.tier] < 0 {
		mp.cursor[mp.tier] = 0
	}
}

// move walks the highlighted entry within the focused tier.
func (mp *ModelPicker) move(delta int) {
	n := mp.tierLen()
	if n == 0 {
		return
	}
	c := mp.cursor[mp.tier] + delta
	if c < 0 {
		c = 0
	}
	if c >= n {
		c = n - 1
	}
	mp.cursor[mp.tier] = c
}

// nextTier / prevTier cycle the tier ring (tab / shift+tab).
func (mp *ModelPicker) nextTier() { mp.tier = (mp.tier + 1) % 3 }
func (mp *ModelPicker) prevTier() { mp.tier = (mp.tier + 2) % 3 }

// HandleKey drives the picker. handled=false falls through to the host (so a
// key the picker does not use can never be swallowed by a modal it owns).
func (mp *ModelPicker) HandleKey(k keyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "esc":
		mp.done = true
		mp.committed = false
		return true, nil
	case "tab":
		mp.nextTier()
		return true, nil
	case "shift+tab":
		mp.prevTier()
		return true, nil
	case "up", "left":
		mp.move(-1)
		return true, nil
	case "down", "right":
		mp.move(1)
		return true, nil
	case "enter", " ", "space":
		return true, mp.choose()
	case "r":
		// Retry a failed load without losing the selection.
		if mp.err != "" {
			return true, mp.Retry()
		}
		return false, nil
	case "backspace":
		if mp.tier == TierModel {
			q := []rune(mp.query)
			if len(q) > 0 {
				mp.query = string(q[:len(q)-1])
				mp.clamp()
			}
			return true, nil
		}
		return false, nil
	}
	if len(k.Runes) > 0 {
		// Typing always targets the MODEL search — the operator's "search box
		// pinned to the adapter/provider for model selection".
		mp.tier = TierModel
		mp.query += string(k.Runes)
		mp.clamp()
		return true, nil
	}
	return false, nil
}

// choose commits the highlighted entry of the focused tier. Choosing an adapter
// or a provider ADVANCES to the next tier (the deliberate three-step walk);
// choosing a model finishes the selection.
func (mp *ModelPicker) choose() tea.Cmd {
	switch mp.tier {
	case TierAdapter:
		if i := mp.cursor[TierAdapter]; i >= 0 && i < len(mp.adapters) {
			a := mp.adapters[i]
			if a != mp.adapter {
				// An adapter change RESCOPES the lower tiers: the previous
				// provider/model is never carried into the new scope
				// (ADR-0004 stale-selection guard).
				mp.adapter = a
				mp.resetLower()
			}
			mp.tier = TierProvider
			return mp.Sync()
		}
	case TierProvider:
		opts := mp.providers
		if i := mp.cursor[TierProvider]; i >= 0 && i < len(opts) {
			p := opts[i].Value
			if p != mp.provider {
				mp.provider = p
				mp.model = ""
				mp.query = ""
				mp.models = nil
				mp.modelsFor = ""
				mp.cursor[TierModel] = 0
			}
			mp.tier = TierModel
			return mp.Sync()
		}
	case TierModel:
		opts := mp.filteredModels()
		if i := mp.cursor[TierModel]; i >= 0 && i < len(opts) {
			mp.model = opts[i].Value
			// The host reads Done/Committed and applies the choice; no command is
			// returned, so nothing depends on a callback surviving across models.
			mp.done, mp.committed = true, true
			return nil
		}
	}
	return nil
}

// resetLower clears the provider+model selection and their loaded lists after an
// adapter change.
func (mp *ModelPicker) resetLower() {
	mp.provider, mp.model, mp.query = "", "", ""
	mp.providers, mp.providersFor = nil, ""
	mp.models, mp.modelsFor = nil, ""
	mp.cursor[TierProvider], mp.cursor[TierModel] = 0, 0
}

// --- mouse ------------------------------------------------------------------

// HandleMouse routes a mouse event, returning handled=true when it belonged to
// the picker (so the host does not also act on it). Wheel walks the focused
// tier; a click selects a chip or a model ROW (and commits a model, since
// clicking a model is the mouse form of "enter").
func (mp *ModelPicker) HandleMouse(m tea.MouseMsg) (bool, tea.Cmd) {
	if m.Action == tea.MouseActionMotion || m.Action == tea.MouseActionRelease {
		return false, nil
	}
	left, top := mp.origin()
	w, h := mp.boxSize()
	if m.X < left || m.X >= left+w || m.Y < top || m.Y >= top+h {
		return false, nil // outside the box: not ours
	}
	// Recompute the layout so the click addresses what is on screen, even if
	// no render has happened yet.
	_, hits := mp.layout(w-2, h-2)
	row, col := m.Y-top-1, m.X-left-1

	switch m.Button {
	case tea.MouseButtonWheelUp:
		mp.move(-1)
		return true, nil
	case tea.MouseButtonWheelDown:
		mp.move(1)
		return true, nil
	case tea.MouseButtonLeft:
		if row == hits.adapterRow {
			if i, ok := hitChip(hits.adapterX, col); ok {
				mp.tier, mp.cursor[TierAdapter] = TierAdapter, i
				return true, mp.choose()
			}
		}
		if row == hits.providerRow {
			if i, ok := hitChip(hits.providerX, col); ok {
				mp.tier, mp.cursor[TierProvider] = TierProvider, i
				return true, mp.choose()
			}
		}
		if hits.listRows > 0 && row >= hits.listTop && row < hits.listTop+hits.listRows {
			idx := hits.listOffset + (row - hits.listTop)
			if opts := mp.filteredModels(); idx >= 0 && idx < len(opts) {
				mp.tier, mp.cursor[TierModel] = TierModel, idx
				return true, mp.choose()
			}
		}
	}
	return true, nil // inside the box but not on a control: consume it
}

// hitChip reports which chip's column range contains col.
func hitChip(ranges [][2]int, col int) (int, bool) {
	for i, r := range ranges {
		if col >= r[0] && col <= r[1] {
			return i, true
		}
	}
	return 0, false
}

// --- rendering --------------------------------------------------------------

// View renders the picker as a self-contained box. The host splices it with
// Center(); the picker computed its origin with the same arithmetic, so mouse
// and render agree.
func (mp *ModelPicker) View() string {
	w, h := mp.boxSize()
	innerW, innerH := w-2, h-2
	if innerW < 10 || innerH < 4 {
		return ""
	}
	rows, _ := mp.layout(innerW, innerH)

	border := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	title := ansi.Truncate(mp.title, max(0, w-5), "…")
	used := 4 + lipgloss.Width(title)
	dash := w - used - 1
	if dash < 0 {
		dash = 0
	}
	var b strings.Builder
	b.WriteString(border.Render("┌─ ") + theme.MenuTitle.Render(title) +
		border.Render(" "+strings.Repeat("─", dash)+"┐"))
	b.WriteString("\n")
	for i := 0; i < innerH; i++ {
		content := ""
		if i < len(rows) {
			content = rows[i]
		}
		b.WriteString(border.Render("│") + theme.ScreenBg.Render(Pad(content, innerW)) + border.Render("│"))
		b.WriteString("\n")
	}
	b.WriteString(border.Render("└" + strings.Repeat("─", w-2) + "┘"))
	return b.String()
}

// layout builds the box interior (one string per row, unpadded) AND the hit map
// for the same geometry — one source of truth for what is drawn and what a
// click addresses.
func (mp *ModelPicker) layout(innerW, innerH int) ([]string, pickerHits) {
	hits := pickerHits{adapterRow: 0, providerRow: 1}
	rows := make([]string, 0, innerH)

	// Tier 1 + 2: adapter and provider chips.
	aLine, ax := chipStrip("ADAPTER", adapterOptions(mp.adapters), mp.cursor[TierAdapter], mp.tier == TierAdapter, innerW)
	hits.adapterX = ax
	pLine, px := chipStrip("PROVIDER", mp.providers, mp.cursor[TierProvider], mp.tier == TierProvider, innerW)
	hits.providerX = px
	rows = append(rows, aLine, pLine)

	// A rule separates the reference tiers from the model list.
	rows = append(rows, theme.HintText.Render(strings.Repeat("─", innerW)))

	// The search box (the model tier's filter).
	rows = append(rows, mp.searchLine(innerW))

	// Reserved bottom rows: status + hints.
	const bottomRows = 2
	listRows := innerH - len(rows) - bottomRows
	if listRows < 1 {
		listRows = 1
	}
	hits.listTop = len(rows)
	hits.listRows = listRows

	opts := mp.filteredModels()
	off := mp.listOffset(len(opts), listRows)
	hits.listOffset = off
	if len(opts) == 0 {
		msg := "(no models)"
		switch {
		case mp.loading:
			msg = "loading models…"
		case mp.err != "":
			msg = "models unavailable — press r to retry"
		case mp.provider == "":
			msg = "choose a provider above to list its models"
		case strings.TrimSpace(mp.query) != "":
			msg = "(no model matches \"" + strings.TrimSpace(mp.query) + "\")"
		}
		rows = append(rows, theme.HintText.Render(Pad("  "+msg, innerW)))
	}
	for i := off; i < len(opts) && i < off+listRows; i++ {
		rows = append(rows, mp.modelRow(opts[i], i == mp.cursor[TierModel], mp.tier == TierModel, innerW))
	}
	// Pad the list to its full height so the status/hint rows stay pinned.
	for len(rows) < hits.listTop+listRows {
		rows = append(rows, "")
	}

	rows = append(rows, mp.statusLine(innerW), hintLine(innerW))
	// Clip to the interior height.
	if len(rows) > innerH {
		rows = rows[:innerH]
	}
	return rows, hits
}

// listOffset is the first visible model index, scrolled to keep the highlight
// in view.
func (mp *ModelPicker) listOffset(n, listRows int) int {
	if n <= listRows {
		return 0
	}
	off := 0
	if mp.cursor[TierModel] >= listRows {
		off = mp.cursor[TierModel] - listRows + 1
	}
	if off > n-listRows {
		off = n - listRows
	}
	if off < 0 {
		off = 0
	}
	return off
}

func (mp *ModelPicker) searchLine(innerW int) string {
	q := mp.query
	caret := ""
	if mp.tier == TierModel {
		caret = "\u258f"
	}
	body := "  search: " + q + caret
	if q == "" && mp.tier != TierModel {
		body = "  search: (tab to the model tier, then type)"
	}
	return theme.DetailValue.Render(Pad(body, innerW))
}

// modelRow renders one model entry: id on the left, dim meta on the right.
func (mp *ModelPicker) modelRow(o PickerOption, cursored, focused bool, innerW int) string {
	label := o.Label
	if label == "" {
		label = o.Value
	}
	mark := "  "
	if cursored && focused {
		mark = "▸ "
	}
	left := mark + label
	right := o.Meta
	maxLabel := innerW - lipgloss.Width(right) - 3
	if maxLabel < 8 {
		maxLabel = 8
	}
	left = ansi.Truncate(left, maxLabel, "…")
	gap := innerW - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	if cursored && focused {
		return theme.ListItemSelected.Render(Pad(line, innerW))
	}
	return theme.ListItem.Render(Pad(line, innerW))
}

// statusLine reports the load state or the current selection summary.
func (mp *ModelPicker) statusLine(innerW int) string {
	var s string
	switch {
	case mp.err != "":
		s = "⚠ " + mp.err
	case mp.loading:
		s = "loading…"
	case mp.tier == TierModel && mp.provider != "":
		n, total := len(mp.filteredModels()), len(mp.models)
		s = strings.Join([]string{
			strconv.Itoa(n) + "/" + strconv.Itoa(total) + " models",
			mp.Ref(),
		}, "  ·  ")
		if mp.degraded {
			s += "  ·  probe degraded"
		}
	case mp.adapter == "":
		s = "select an adapter"
	default:
		s = "select a " + tierName(mp.tier)
	}
	style := theme.HintText
	if mp.err != "" {
		style = theme.ErrorText
	}
	return style.Render(Pad("  "+s, innerW))
}

func hintLine(innerW int) string {
	// The hint stays a SINGLE line so the fixed bottom-row math holds. It picks
	// the LONGEST variant that FITS rather than letting Pad cut one mid-word — a
	// truncated "esc: canc" reads as a broken key contract, not a narrow box.
	variants := []string{
		"tab: tier · ↑/↓/←/→: move · type: search · enter/space: select · esc: cancel",
		"tab: tier · ↑/↓: move · type: search · enter/space: select · esc: cancel",
		"tab: tier · ↑/↓: move · type: search · enter: select · esc: cancel",
		"tab · ↑/↓ · type · enter · esc",
	}
	for _, v := range variants {
		line := "  " + v
		if lipgloss.Width(line) <= innerW {
			return theme.HintText.Render(Pad(line, innerW))
		}
	}
	return theme.HintText.Render(Pad("  "+variants[len(variants)-1], innerW))
}

// chipStrip renders one horizontal chip row and reports each chip's column
// range. The strip WINDOWS around the highlight so a long provider list can
// never push the focused chip off the row (which would make the arrows look
// broken).
func chipStrip(label string, items []PickerOption, cursor int, focused bool, innerW int) (string, [][2]int) {
	// The head and the space available to chips are computed FIRST, because a chip's label may have to
	// be shortened to fit — see the fit check below.
	head := "  " + label + " "
	if focused {
		head = "▸ " + label + " "
	}
	headW := lipgloss.Width(head)
	avail := innerW - headW - 1
	if avail < 4 {
		avail = 4
	}

	texts := make([]string, len(items))
	widths := make([]int, len(items))
	for i, it := range items {
		lbl := it.Label
		if lbl == "" {
			lbl = it.Value
		}
		// THE CHOSEN CHIP MUST FIT, OUTLINE INCLUDED. It is drawn two cells wider than its label (the
		// two rules) and padded one space each side, so a name that alone exceeds the strip loses the
		// closing rule to the box's Pad — leaving an UNCLOSED box that reads as a rendering fault.
		// Truncating the LABEL keeps the outline complete, so the operator can still see which chip is
		// chosen when the name has to be shortened.
		//
		// Measured before this: at screen widths 48 and 40 the closing rule was cut, so the row read
		// "│▸ PROVIDER │ DeepSeekAPIWithAVeryLongProvi│".
		if i == cursor && focused && lipgloss.Width(lbl)+4 > avail {
			lbl = ansi.Truncate(lbl, max(1, avail-4), "…")
		}
		texts[i] = " " + lbl + " "
		// THE LAYOUT COUNTS THE RENDERED WIDTH, not the label's — the outlined chip is two cells wider,
		// and the window is decided here.
		extra := 0
		if i == cursor && focused {
			extra = 2 // the two vertical rules
		}
		widths[i] = lipgloss.Width(texts[i]) + 1 + extra // +1 gap
	}

	lo, hi := 0, 0
	if len(items) == 0 {
		lo, hi = 0, 0
	} else if cursor >= 0 && cursor < len(items) {
		used := widths[cursor]
		lo, hi = cursor, cursor+1
		for hi < len(items) && used+widths[hi] <= avail {
			used += widths[hi]
			hi++
		}
		for lo > 0 && used+widths[lo-1] <= avail {
			lo--
			used += widths[lo]
		}
	} else {
		used := 0
		hi = 0
		for hi < len(items) && used+widths[hi] <= avail {
			used += widths[hi]
			hi++
		}
	}

	var b strings.Builder
	b.WriteString(theme.DetailKey.Render(head))
	pos := headW
	ranges := make([][2]int, 0, hi-lo)
	if len(items) == 0 {
		b.WriteString(theme.HintText.Render("—" + strings.Repeat(" ", max(0, innerW-headW-1))))
		return b.String(), ranges
	}
	if lo > 0 {
		b.WriteString(theme.HintText.Render("…"))
		pos += 1
	}
	for i := lo; i < hi; i++ {
		chosen := i == cursor
		var rendered string
		switch {
		case chosen && focused:
			// An OUTLINE, not a fill: the label keeps the surface's background so the operator reads a
			// word. See theme.PickerChip for the operator's request and why it is unfilled.
			rendered = theme.PickerChip.Render(texts[i])
		case chosen:
			// The chosen chip of an UNFOCUSED tier is already unfilled (bold body text).
			rendered = theme.ListTitle.Render(texts[i])
		default:
			rendered = theme.ListItem.Render(texts[i])
		}
		// The hit range must match what was DRAWN, so it is measured from the rendered string — the
		// outlined chip is two cells wider than its label, and a range computed from the label would
		// put the clickable area one cell inside the box on each side.
		ranges = append(ranges, [2]int{pos + 1, pos + lipgloss.Width(rendered) - 1})
		b.WriteString(rendered)
		pos += lipgloss.Width(rendered)
		if i < hi-1 {
			b.WriteString(" ")
			pos++
		}
	}
	if hi < len(items) {
		b.WriteString(theme.HintText.Render("…"))
	}
	return b.String(), ranges
}

// adapterOptions projects adapter kinds into chips.
func adapterOptions(kinds []string) []PickerOption {
	out := make([]PickerOption, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, PickerOption{Value: k, Label: k})
	}
	return out
}

// orderAdapters puts preferred first (when present) and preserves the rest of
// the reported order. A nil preferred adapter is a no-op — the picker never
// invents an ordering it was not given.
func orderAdapters(kinds []string, preferred string) []string {
	if preferred == "" || len(kinds) == 0 {
		return kinds
	}
	out := make([]string, 0, len(kinds))
	found := false
	for _, k := range kinds {
		if k == preferred {
			found = true
			break
		}
	}
	if !found {
		return kinds
	}
	out = append(out, preferred)
	for _, k := range kinds {
		if k != preferred {
			out = append(out, k)
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func tierName(t int) string {
	switch t {
	case TierAdapter:
		return "adapter"
	case TierProvider:
		return "provider"
	default:
		return "model"
	}
}
