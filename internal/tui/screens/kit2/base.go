package kit2

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Screen is the contract every area screen implements (alias of the
// screenkit interface — kept so the shell's dispatch stays unchanged).
type Screen = screenkit.Screen

// StatusMsg / SourceMeta are aliased from screenkit so screens keep one
// vocabulary while the SCREEN MODEL moves to kit2.
type (
	StatusMsg      = screenkit.StatusMsg
	SourceMeta     = screenkit.SourceMeta
	Item           = screenkit.Item
	Field          = screenkit.Field
	DetailFn       = screenkit.DetailFn
	Fetch          = func(ctx context.Context, pageToken string) ([]screenkit.Item, string, error)
	StatusReporter = screenkit.StatusReporter
)

// source is one RPC-backed pane, rendered as a kit2 Table inside a Panel.
type source struct {
	name  string
	title string
	fetch Fetch
	table *Table
	err   string
	// filterable draws the search row above this source's list ('/' focuses it).
	filterable bool
	// rowActions are the CLICKABLE controls rendered on that same top row (the
	// tree's collapse/expand-all), right-aligned.
	rowActions []RowAction
	// caption is a SCREEN-SUPPLIED mode label drawn on that same top row, beside the search box.
	caption func() string
	// markable says this source's rows support MULTI-SELECT marks.
	markable bool
}

// topRows is the number of rows this source draws above the table's own rows
// (the search / controls row).
func (s *source) topRows() int {
	if s.filterable || len(s.rowActions) > 0 || s.caption != nil {
		return 1
	}
	return 0
}

// RowAction is one clickable control on a pane's top row.
type RowAction struct {
	Label func() string
	Do    func() tea.Cmd
}

// actionHit records where a RowAction was drawn, so a click can be hit-tested
// against what is actually on screen (recomputed on every render).
type actionHit struct {
	src    string
	i      int
	x0, x1 int // columns INSIDE the panel border
}

type fetchedMsg struct {
	src    string
	items  []Item
	next   string
	append bool
	err    error
}

type detailMsg struct {
	src    string
	id     string
	title  string
	fields []Field
	body   string
}

type detailErrMsg struct{ err error }

// Base is the kit2 screen model: multiple RPC-backed panes (kit2 Tables), one Detail, the shared
// Focus ring, a first-class Stream, an ActionBar, an open Dialog, and the single mutation Executor.
type Base struct {
	NameStr string

	sources []*source
	active  int
	detail  screenkit.Detail
	focusD  bool

	// listSharePct is the operator's WIDTH PREFERENCE for the tree/list pane of the master-detail
	// split, as a percentage of the content width (0 = the default, an even split).
	listSharePct int

	// editPanel HOSTS the open inline form and lives as long as the editing session does.
	editPanel *Panel

	// editForm, when non-nil, replaces the detail pane's body with a typed
	// form: the screen's INLINE EDIT mode. Keys route to it while it is up.
	editForm  *Form
	editTitle string
	// OnEditDone is notified once when the inline editor closes.
	OnEditDone func(submitted bool)

	width, height int
	detailFn      DetailFn
	// detailPaintOwner answers "is the screen painting its own detail?" — see
	// SetDetailPaintOwner. nil means nobody does.
	detailPaintOwner func() bool
	onDetail         func(src, id string) tea.Cmd
	detailID         string
	shell            any
	statuses         []StatusMsg

	// noAutoDetail suppresses the post-fetch auto-detail (the Ask screen's hero stays until the
	// operator picks an item or sends the first message).
	noAutoDetail bool
	heroTitle    string
	heroBody     string

	// filtering is true while the operator is TYPING into the focused source's
	// filter box (the search row above the list).
	filtering bool

	// pendingSelect names a row to focus as soon as it APPEARS in a source.
	pendingMu     sync.Mutex
	pendingSelect map[string]string
	pendingOwn    map[string]string

	// actionHits is recomputed on every render (see filterLine) and consumed by
	// the next click, so a control is hit-tested against what was DRAWN.
	actionHits []actionHit

	// Focus is the ONE rule for key ownership across this screen's regions.
	Focus *Focus
	// Stream is the screen's first-class scrolling transcript (nil = none).
	Stream *Stream
	// Bar is the footer action strip (nil = none).
	Bar *ActionBar
	// Open is the modal dialog currently overlaying the screen (nil = none).
	Open *Dialog
	// Exec is the single write path (nil = direct-RPC fallback, tests).
	Exec *mutate.Executor

	// HideSources renders the detail pane ONLY (no source panes).
	HideSources bool

	// detailBodyHostOwned declares that the detail pane's BODY is painted by the HOST, not by this screen's
	// detail function — so a detail PAYLOAD must not overwrite it.
	//
	// It exists because the two halves of a detail arrive differently, and one of them is empty here. Every
	// other screen's detail() returns the body, so a detail landing legitimately replaces it. The Ask
	// screen's returns body="" BY DESIGN: its transcript is a merge of the durable conversation and the
	// live chunk cache, which only the shell can see, and it is painted by onChatWake. Assigning that
	// empty body — which the base did, unconditionally — ERASES the transcript:
	//
	//	"the conversation pane is completely blank on every chat."
	//
	// It was reachable on every list reload, because Base.Update's fetchedMsg handler ends with
	// loadDetail() — so the rolling refresh's list reload re-requested the detail and wiped the pane a few
	// seconds after every repaint. See the detailMsg case, and ask.New for the declaration.
	detailBodyHostOwned bool

	// OnDialog runs when the open Dialog resolves ("" = dismissed).
	OnDialog func(choice string) tea.Cmd

	// OnActivate, when set, handles Enter/Space on the selected row (list focus).
	OnActivate func() (handled bool, cmd tea.Cmd)

	// OnHighlight, when set, runs after the cursor LANDS ON A NEW ROW while the LIST has focus — an
	// arrow move (up/down/j/k) or a mouse click.
	OnHighlight func() tea.Cmd

	// OnItemsLanded, when set, runs after a source's rows are (re)placed by a fetch or a direct load.
	OnItemsLanded func() tea.Cmd
}

// AddSource registers a fetchable list pane.
func (b *Base) AddSource(name, title string, fetch Fetch) {
	b.sources = append(b.sources, &source{
		name: name, title: title, fetch: fetch,
		table: NewTable(title, Column{Title: ""}),
		// Markable by default: most lists here DO have bulk operations, and opting in would have left the
		// feature off wherever a screen forgot to ask for it.
		markable: true,
	})
	b.rebuildFocus()
}

// SetMarkable says whether a source's rows support multi-select marks (default true).
func (b *Base) SetMarkable(sourceName string, on bool) {
	for _, s := range b.sources {
		if s.name == sourceName {
			s.markable = on
			return
		}
	}
}

// activeMarkable reports whether the ACTIVE source's rows can be marked.
func (b *Base) activeMarkable() bool {
	if b.active < 0 || b.active >= len(b.sources) {
		return false
	}
	return b.sources[b.active].markable
}

func (b *Base) rebuildFocus() {
	r := make([]string, 0, len(b.sources))
	for _, s := range b.sources {
		r = append(r, s.name)
	}
	r = append(r, "detail")
	b.Focus = NewFocus(r...)
}

// FocusedRegion returns the region that currently owns the keyboard.
func (b *Base) FocusedRegion() string {
	if b.Focus == nil || len(b.sources) == 0 {
		return ""
	}
	return b.Focus.Current()
}

// SetStatuses seeds the status reporter.
func (b *Base) SetStatuses(sts []StatusMsg) { b.statuses = sts }

// SetSourceEmpty sets the named source's empty-state message.
func (b *Base) SetSourceEmpty(name, msg string) {
	for _, s := range b.sources {
		if s.name == name {
			s.table.Empty = msg
			return
		}
	}
}

// SetStatus updates one named status.
func (b *Base) SetStatus(name, st string) {
	for i := range b.statuses {
		if b.statuses[i].Name == name {
			b.statuses[i].Status = st
			return
		}
	}
	b.statuses = append(b.statuses, StatusMsg{Name: name, Status: st})
}

// ReportStatus returns the current per-subscription statuses.
func (b *Base) ReportStatus() []StatusMsg { return b.statuses }

// regionWidths splits the content width across the source panes + detail.
func (b *Base) regionWidths() []int {
	n := len(b.sources) + 1
	if n == 2 {
		lw, dw := b.twoPaneWidths(b.width)
		return []int{lw, dw}
	}
	return SplitWidths(b.width, n, 1)
}

// The master-detail split's bounds, as percentages of the content width.
const (
	defaultListSharePct = 50
	minListSharePct     = 25
	maxListSharePct     = 85
)

// ListSharePct is the share in force (the default when the operator has set none).
func (b *Base) ListSharePct() int {
	if b.listSharePct <= 0 {
		return defaultListSharePct
	}
	return b.listSharePct
}

// SetListSharePct sets the share, clamped to the usable range.
func (b *Base) SetListSharePct(pct int) {
	if pct <= 0 {
		// 0 MEANS THE DEFAULT, not the minimum: the App's zero value is "the operator has expressed no
		// preference", and clamping it to minListSharePct would leave every screen on a split nobody chose.
		b.listSharePct = 0
		return
	}
	if pct < minListSharePct {
		pct = minListSharePct
	}
	if pct > maxListSharePct {
		pct = maxListSharePct
	}
	b.listSharePct = pct
}

// SplitAdjustable reports whether this screen HAS a tree/detail split to adjust.
func (b *Base) SplitAdjustable() bool {
	return !b.HideSources && len(b.sources) > 0
}

// NudgeListShare moves the share by delta percent, reporting whether it MOVED (so a caller at a
// bound can tell "nothing happened" from "it changed").
func (b *Base) NudgeListShare(delta int) bool {
	before := b.ListSharePct()
	b.SetListSharePct(before + delta*2)
	return b.ListSharePct() != before
}

// IT IS THE ONLY PLACE THE SPLIT IS DECIDED, and that is the point: the layout (SinglePane), the
// click hit-test (mouseRegion) and the divider a drag grabs must all describe the same boundary.
func (b *Base) twoPaneWidths(w int) (listW, detailW int) {
	total := w - 1 // the gap between the panes
	if total < 2 {
		total = 2
	}
	// ROUNDED UP, so the LIST gets a split's remainder — which is what the even split this replaces did
	// (SplitWidths gives the extra column to the FIRST pane).
	listW = (total*b.ListSharePct() + 99) / 100
	if listW < 1 {
		listW = 1
	}
	if listW > total-1 {
		listW = total - 1
	}
	return listW, total - listW
}

// SetSize lays out the panes (equal split + detail).
func (b *Base) SetSize(w, h int) {
	b.width, b.height = w, h
	// HideSources renders the DETAIL PANE ONLY, at the FULL width (View → detailPaneView(b.width,
	// b.height)).
	if b.HideSources || len(b.sources) == 0 {
		b.detail.Width, b.detail.Height = w, h
		return
	}
	ws := b.regionWidths()
	for i, s := range b.sources {
		s.table.Width, s.table.Height = ws[i], h
		s.table.SetSizeHint(h)
	}
	b.detail.Width = ws[len(ws)-1]
	b.detail.Height = h
	if b.Stream != nil {
		b.Stream.SetSize(w, h)
	}
}

// SetSizeHint stores the pane height on the table (tables size themselves
// from Width/Height).
func (t *Table) SetSizeHint(h int) { t.Height = h }

// Frame normalizes a composed block into the exact content region.
func (b *Base) Frame(content string) string {
	if b.width < 1 || b.height < 1 {
		return content
	}
	return FitLines(content, b.width, b.height)
}

// PaneSize returns the first pane's dimensions.
func (b *Base) PaneSize() (int, int) {
	if len(b.sources) == 0 {
		return 0, b.height
	}
	return b.sources[0].table.Width, b.height
}

// HasAuthRetry reports whether any pane shows the inline re-auth state.
func (b *Base) HasAuthRetry() bool {
	for _, s := range b.sources {
		if isAuthText(s.err) {
			return true
		}
	}
	return false
}

func isAuthText(errtxt string) bool {
	l := strings.ToLower(errtxt)
	return strings.Contains(l, "unauthenticated") || strings.Contains(l, "unauthorized") ||
		strings.Contains(l, "re-authentication") || strings.Contains(l, "re-auth")
}

// Load fetches page 1 of every source.
func (b *Base) Load() tea.Cmd {
	var cmds []tea.Cmd
	for i := range b.sources {
		cmds = append(cmds, b.loadSource(i, ""))
	}
	return tea.Batch(cmds...)
}

// Refresh re-fetches one source (the mutation layer's reconcile hook).
func (b *Base) Refresh(source string) tea.Cmd {
	for i, s := range b.sources {
		if s.name == source {
			return b.loadSource(i, "")
		}
	}
	return nil
}

// RefreshView re-reads what is ON SCREEN: the ACTIVE source's list and, when the detail pane is
// showing an item, that item's detail. WHY ONLY THE VISIBLE SOURCE.
func (b *Base) RefreshView() tea.Cmd {
	// This default re-requests the READ-ONLY detail for the selected row — and a screen with its own
	// detail renderer (the workflow FLOW editor) would have that renderer OVERWRITTEN by it.
	if b.ownsDetailPaint() || b.editingDetail() || b.Open != nil {
		return nil
	}
	cmds := []tea.Cmd{}
	if b.active >= 0 && b.active < len(b.sources) {
		cmds = append(cmds, b.loadSource(b.active, ""))
	}
	// The open detail is refreshed too: a list reload does not re-read the row under the cursor, and
	// the detail is where a follow-up reply appears. It is a no-op when nothing is selected.
	if b.detailID != "" {
		cmds = append(cmds, b.RequestDetail(b.ActiveSourceName(), b.detailID))
	}
	return tea.Batch(cmds...)
}

// editingDetail reports whether the inline detail editor is open. (EditingDetail is the exported
// form; this is the internal read the refresh guard needs.)
func (b *Base) editingDetail() bool { return b.editForm != nil }

// SetDetailPaintOwner installs the predicate that answers "is the screen painting its own detail?".
func (b *Base) SetDetailPaintOwner(fn func() bool) { b.detailPaintOwner = fn }

// ownsDetailPaint reports whether the screen is currently painting its own detail pane.
func (b *Base) ownsDetailPaint() bool {
	return b.detailPaintOwner != nil && b.detailPaintOwner()
}

// loadSource loads a source from the top, fetching the WHOLE set.
func (b *Base) loadSource(i int, pageToken string) tea.Cmd {
	s := b.sources[i]
	if s.fetch == nil {
		return nil
	}
	s.table.Loading = true
	name := s.name
	fetch := s.fetch
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		all := make([]screenkit.Item, 0, 256)
		token := pageToken
		for page := 0; page < maxSourcePages; page++ {
			items, next, err := fetch(ctx, token)
			if err != nil {
				// A FAILED page after a successful one still returns what was fetched: a partial list beats an
				// empty pane, and the operator keeps the rows they can already see.
				if len(all) > 0 {
					return fetchedMsg{src: name, items: all, err: nil}
				}
				return fetchedMsg{src: name, items: items, err: err}
			}
			all = append(all, items...)
			token = next
			if token == "" {
				break
			}
		}
		return fetchedMsg{src: name, items: all}
	}
}

// maxSourcePages bounds the whole-set load.
const maxSourcePages = 200

// loadMore is retired along with the pager: nothing is left unfetched, so there is no next page to
// ask for. Kept as a no-op so the `f` binding can be removed without a call site dangling.
func (b *Base) loadMore() tea.Cmd { return nil }

// friendlyFetchErr maps a raw RPC error to a retryable state string.
func friendlyFetchErr(errText string) string {
	low := strings.ToLower(errText)
	switch {
	case strings.Contains(low, "unauthenticated"):
		return "session needs re-authentication — run /connect (esc cancels; the shell reconnects in place)"
	case strings.Contains(low, "permission_denied") || strings.Contains(low, "forbidden"):
		return "not permitted for this credential — check the API key's scopes or re-authenticate with /connect"
	case strings.Contains(low, "deadline_exceeded") || strings.Contains(low, "timeout"):
		return "the plane took too long to answer — press r to refresh"
	case strings.Contains(low, "unavailable") || strings.Contains(low, "connection refused"):
		return "the plane is unreachable right now — press r to refresh once it is back"
	default:
		return errText
	}
}

// SetDetail installs the detail renderer.
func (b *Base) SetDetail(fn DetailFn) { b.detailFn = fn }

// SetNoAutoDetail suppresses the post-fetch auto-detail: the pane keeps the empty state the screen
// installed until the operator picks an item (the Ask screen's hero).
func (b *Base) SetNoAutoDetail(v bool) { b.noAutoDetail = v }

// SetHero installs the detail pane's centered empty state, shown until real
// content replaces it (and restored by ClearDetail).
func (b *Base) SetHero(title, body string) {
	b.heroTitle, b.heroBody = title, body
	b.detail.SetHero(title, body)
}

// ClearDetail returns the detail pane to its empty state (the hero when one
// is installed) and forgets the item it was showing.
func (b *Base) ClearDetail() {
	b.detailID = ""
	if b.heroTitle != "" || b.heroBody != "" {
		b.detail.SetHero(b.heroTitle, b.heroBody)
		return
	}
	b.detail.SetContent("", nil, "")
}

// ScrollDetail scrolls the detail pane by delta lines (mouse wheel + the
// empty-composer vertical keys) — the transcript scroll preservation path.
func (b *Base) ScrollDetail(delta int) { b.detail.Wheel(delta) }

// SetDetailBodyHostOwned declares that the detail pane's BODY is painted by the host, so a detail payload
// must not overwrite it (only its title and fields are taken). See detailBodyHostOwned.
func (b *Base) SetDetailBodyHostOwned(v bool) { b.detailBodyHostOwned = v }

// SetShell installs the app shell reference.
func (b *Base) SetShell(sh any) { b.shell = sh }

// Shell returns the installed shell reference.
func (b *Base) Shell() any { return b.shell }

// SetOnDetail installs the detail-landed hook.
func (b *Base) SetOnDetail(fn func(src, id string) tea.Cmd) { b.onDetail = fn }

// DetailID returns the id the detail pane currently shows.
func (b *Base) DetailID() string { return b.detailID }

// SetDetailID declares that the detail pane is showing the item with this id.
func (b *Base) SetDetailID(id string) { b.detailID = id }

// DetailWidth returns the detail pane's render width.
func (b *Base) DetailWidth() int {
	if w := b.detail.Width; w > 10 {
		return w - 2
	}
	return 60
}

// DetailBodyHeight is the rows the detail pane will give its scrolling body for the given field
// count.
func (b *Base) DetailBodyHeight(fieldRows int, hasBody bool) int {
	return b.detail.BodyHeightFor(fieldRows, hasBody)
}

// SetDetailFooter installs a FIXED band at the bottom of the detail pane, outside the pane's
// scrolling region, so it is always on screen however tall the body.
func (b *Base) SetDetailFooter(s string) { b.detail.SetFooter(s) }

// DetailFooter returns the pane's current fixed band.
func (b *Base) DetailFooter() string { return b.detail.Footer() }

// SetDetailScrollBottom pins the detail pane's viewport to its LAST row.
func (b *Base) SetDetailScrollBottom() { b.detail.SetScrollOffset(1 << 20) }

// SetDetailScrollTop pins the detail pane's viewport to a LINE OFFSET.
func (b *Base) SetDetailScrollTop(offset int) {
	if offset < 0 {
		offset = 0
	}
	b.detail.SetScrollOffset(offset)
}

// SetDetailContent pushes live content into the open detail pane.
func (b *Base) SetDetailContent(title string, fields []Field, body string) {
	b.detail.SetContent(title, fields, body)
}

// SetDetailContentLaidOut is SetDetailContent for a body the caller has ALREADY laid out. The pane returns
// it verbatim. See screenkit.Detail.bodyLaidOut.
func (b *Base) SetDetailContentLaidOut(title string, fields []Field, body string) {
	b.detail.SetContentLaidOut(title, fields, body)
}

// SetDetailBody replaces the detail pane's FIELDS and BODY while keeping its title — the in-place
// repaint a cursor move needs.
func (b *Base) SetDetailBody(body string, fields []Field) {
	b.detail.SetContent(b.detail.Title, fields, body)
}

// SetDetailBodyLaidOut is SetDetailBody for a body the caller has already laid out (the runs step flow
// moving its marker). See screenkit.Detail.bodyLaidOut.
func (b *Base) SetDetailBodyLaidOut(body string, fields []Field) {
	b.detail.SetContentLaidOut(b.detail.Title, fields, body)
}

// DetailFieldCount reports how many field ROWS the pane will draw, which determines its body
// height.
func (b *Base) DetailFieldCount() int { return len(b.detail.Fields) }

// DetailForTest exposes the pane's current contents, so a screen can REPAINT around a change
// without re-deriving what the fetch produced (and so tests can assert what is on screen).
func (b *Base) DetailForTest() (string, []Field, string) {
	return b.detail.Title, b.detail.Fields, b.detail.Body
}

// SetExecutor installs the single mutation executor (feedback + reconcile).
func (b *Base) SetExecutor(e *mutate.Executor) { b.Exec = e }

// SetOnEditDone registers the callback notified once when the inline detail-pane editor closes
// (submitted=true on save, false on cancel).
func (b *Base) SetOnEditDone(fn func(submitted bool)) { b.OnEditDone = fn }

// BeginDetailEdit installs an inline editor in the DETAIL pane and moves focus there, so an entity
// is edited in place rather than in a modal box.
func (b *Base) BeginDetailEdit(title string, f *Form) {
	b.editForm, b.editTitle = f, title
	b.focusD = true
	if b.Focus != nil {
		b.Focus.Set("detail")
	}
}

// CloseDetailEdit closes the inline editor WITHOUT reporting a submit — the
// programmatic equivalent of esc.
func (b *Base) CloseDetailEdit() {
	if b.editForm != nil {
		b.finishDetailEdit(false)
	}
}

// EditingDetail reports whether the detail pane is in inline-edit mode.
func (b *Base) EditingDetail() bool { return b.editForm != nil }

// DetailForm exposes the open inline editor (nil when not editing).
func (b *Base) DetailForm() *Form { return b.editForm }

// PasteIntoForm inserts pasted text into the DETAIL-PANE form's focused field, reporting whether it
// landed.
func (b *Base) PasteIntoForm(text string) bool {
	if f := b.DetailForm(); f != nil {
		return f.PasteText(text)
	}
	return false
}

// finishDetailEdit closes the inline editor and reports the outcome once.
func (b *Base) finishDetailEdit(submitted bool) {
	b.editForm = nil
	// The hosting panel is dropped with the session, so the next form opens at the top with its own
	// content and scroll rather than inheriting the previous one's position.
	b.editPanel = nil
	if b.OnEditDone != nil {
		b.OnEditDone(submitted)
	}
}

// ActiveTableActions exposes the focused source's top-row controls (tests and
// the shell read them).
func (b *Base) ActiveTableActions() []RowAction {
	if b.active < 0 || b.active >= len(b.sources) {
		return nil
	}
	return b.sources[b.active].rowActions
}

// SetRowActions installs the clickable controls drawn on a source's top row.
func (b *Base) SetRowActions(src string, actions []RowAction) {
	for _, s := range b.sources {
		if s.name == src {
			s.rowActions = actions
			return
		}
	}
}

// EnableFilter marks a source as filterable: its pane draws a search row above the list and '/'
// focuses it (the operator's "search box at the top of the work items page").
func (b *Base) EnableFilter(src string) {
	for _, s := range b.sources {
		if s.name == src {
			s.filterable = true
			return
		}
	}
}

// SetCollapsedByDefault makes a source's tree rows start CLOSED the first time they appear (see
// Table.CollapsedByDefault).
func (b *Base) SetCollapsedByDefault(src string, on bool) {
	for _, s := range b.sources {
		if s.name == src {
			s.table.CollapsedByDefault = on
			return
		}
	}
}

// ResetCollapseState re-seats a source's rows to its current collapse default.
func (b *Base) ResetCollapseState(src string) {
	for _, s := range b.sources {
		if s.name == src {
			s.table.ApplyCollapseDefault()
			return
		}
	}
}

// SetCaption installs a mode label drawn on the source's top row, beside the search box.
func (b *Base) SetCaption(src string, label func() string) {
	for _, s := range b.sources {
		if s.name == src {
			s.caption = label
			return
		}
	}
}

// Filtering reports whether the operator is typing into the filter box.
func (b *Base) Filtering() bool { return b.filtering }

// filterTarget is the focused source's table when that source is filterable.
func (b *Base) filterTarget() *Table {
	if b.active < 0 || b.active >= len(b.sources) {
		return nil
	}
	s := b.sources[b.active]
	if !s.filterable {
		return nil
	}
	return s.table
}

// DropKeyClaim releases every reason this screen claims the keyboard, so a focus chord (ctrl+g) can
// hand control back to the composer.
func (b *Base) DropKeyClaim() {
	if b.filtering {
		b.StopFilter()
	}
	if b.editForm != nil {
		b.finishDetailEdit(false)
	}
}

// StartFilter focuses the search box of the focused source (no-op when that
// source has none).
func (b *Base) StartFilter() bool {
	if b.filterTarget() == nil {
		return false
	}
	b.filtering = true
	b.focusD = false
	b.setFocusForPane()
	return true
}

// StopFilter leaves the search box, KEEPING the query (esc clears it instead),
// so the operator can navigate the narrowed list.
func (b *Base) StopFilter() { b.filtering = false }

// ClearFilter drops the query and leaves the search box.
func (b *Base) ClearFilter() {
	if t := b.filterTarget(); t != nil {
		t.SetFilter("")
	}
	b.filtering = false
}

// Mutate runs a mutation through the executor (reconciling this screen's
// affected source). Direct-RPC fallback when no executor is installed.
func (b *Base) Mutate(req mutate.Request) tea.Cmd {
	e := b.Exec
	if e == nil {
		e = &mutate.Executor{}
	}
	if e.Reconcile == nil {
		e.Reconcile = func(src string) tea.Cmd { return b.Refresh(src) }
	}
	return e.Run(req)
}

// HandleMutation handles a mutate.Result (screens forward it here).
func (b *Base) HandleMutation(res mutate.Result) tea.Cmd {
	e := b.Exec
	if e == nil {
		e = &mutate.Executor{}
	}
	if e.Reconcile == nil {
		e.Reconcile = func(src string) tea.Cmd { return b.Refresh(src) }
	}
	cmd := e.Apply(res)
	// A successful mutation leaves the DETAIL pane STALE unless it is re-read.
	if res.Err == nil && b.detailID != "" && b.detailFn != nil {
		cmd = tea.Batch(cmd, b.loadDetail())
	}
	return cmd
}

// Update handles shared behavior: fetch results, keys, mouse.
func (b *Base) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case mutate.Result:
		return true, b.HandleMutationMsg(msg)

	case fetchedMsg:
		for _, s := range b.sources {
			if s.name != msg.src {
				continue
			}
			if msg.err != nil {
				s.err = friendlyFetchErr(msg.err.Error())
				s.table.Err = s.err
				s.table.Loading = false
				return true, nil
			}
			s.err = ""
			s.table.Err = ""
			if msg.append {
				rows := make([]Row, 0, len(msg.items))
				for _, it := range msg.items {
					rows = append(rows, Row{
						ID:     it.ID,
						Cells:  []string{it.Title},
						Meta:   it.Meta,
						Depth:  it.Depth,
						Parent: it.Parent,
						Expand: it.HasChildren,
						Open:   true,
					})
				}
				s.table.AppendRows(rows, msg.next)
			} else {
				s.table.SetItems(msg.items, msg.next)
			}
			// A freshly created entity is focused the moment it appears.
			b.focusPending(s.name, s.table)
			s.table.Loading = false
			// The rows have LANDED: give the screen the chance to reconcile whatever depends
			// on them, before the detail path returns (see OnItemsLanded).
			var landed tea.Cmd
			if b.OnItemsLanded != nil {
				landed = b.OnItemsLanded()
			}
			if b.noAutoDetail {
				return true, landed
			}
			// A JUMP owns the pane until its target has actually landed: the detail it asked for is the one the
			// operator wants, and the row under the cursor is not it when the target is off this page (see.
			if b.ownsPending(s.name) {
				return true, landed
			}
			return true, tea.Batch(landed, b.loadDetail())
		}
		return true, nil

	case detailMsg:
		// A detail payload belongs to the screen that REQUESTED it.
		if !b.ownsSource(msg.src) {
			return true, nil // consumed and dropped: not ours
		}
		b.detailID = msg.id
		// THE BODY BELONGS TO THE HOST when a screen says so.
		body := msg.body
		if b.detailBodyHostOwned {
			body = b.detail.Body
		}
		b.detail.SetContent(msg.title, msg.fields, body)
		if b.onDetail != nil {
			return true, b.onDetail(msg.src, msg.id)
		}
		return true, nil

	case detailErrMsg:
		b.detail.SetContent("couldn't load this item", []Field{{Key: "error", Value: friendlyFetchErr(msg.err.Error())}}, "")
		return true, nil

	case tea.KeyMsg:
		return b.key(msg)

	case tea.MouseMsg:
		return true, b.mouse(msg)
	}
	return false, nil
}

// HandleMutationMsg is the exported form of HandleMutation for msg routing.
func (b *Base) HandleMutationMsg(res mutate.Result) tea.Cmd { return b.HandleMutation(res) }

func (b *Base) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	// A modal dialog owns EVERY key while it is open (modality layered on
	// top of the focus ring, never inside it).
	if b.Open != nil {
		choice, done := b.Open.HandleKey(msg)
		if !done {
			return true, nil
		}
		d := b.Open
		b.Open = nil
		_ = d
		if b.OnDialog != nil {
			return true, b.OnDialog(choice)
		}
		return true, nil
	}

	// The inline detail editor owns EVERY key while it is up: it is the focused surface, so a keystroke
	// can never leak to the list behind it or to the shell's chords ('q' would quit mid-edit).
	if b.editForm != nil {
		f := b.editForm
		switch msg.String() {
		case "esc":
			// THE FORM GETS FIRST REFUSAL ON ESC.
			if _, handled := f.HandleKey(msg); handled {
				return true, nil
			}
			b.finishDetailEdit(false)
			return true, nil
		case "ctrl+s":
			cmd, _ := f.Submit()
			if f.Submitted {
				b.finishDetailEdit(true)
			}
			return true, cmd
		}
		cmd, _ := f.HandleKey(msg)
		if f.Submitted {
			b.finishDetailEdit(true)
		}
		return true, cmd
	}

	// The filter box owns the keys while the operator is typing into it: it is a text input, so nothing
	// may leak to the list or the shell ('q' would quit mid-search, 'n' would open a create modal).
	if b.filtering {
		t := b.filterTarget()
		if t == nil {
			b.filtering = false
			return true, nil
		}
		switch msg.String() {
		case "esc":
			b.ClearFilter()
			return true, nil
		case "enter":
			b.StopFilter()
			return true, b.loadDetail()
		case "backspace":
			q := []rune(t.Filter)
			if len(q) > 0 {
				t.SetFilter(string(q[:len(q)-1]))
			}
			return true, nil
		}
		if len(msg.Runes) > 0 {
			t.SetFilter(t.Filter + string(msg.Runes))
		}
		return true, nil
	}

	switch msg.String() {
	case "up", "k":
		if b.focusD {
			b.detail.Wheel(-2)
		} else {
			b.curTable().Move(-1)
			return true, b.afterHighlight()
		}
		return true, nil
	case "down", "j":
		if b.focusD {
			b.detail.Wheel(2)
		} else {
			b.curTable().Move(1)
			return true, b.afterHighlight()
		}
		return true, nil
	case "left":
		// LEFT/RIGHT move focus BETWEEN THE TWO PANES below the tab bar: left takes the source list for the
		// submenu the operator selected, right takes the detail.
		b.focusD = false
		b.setFocusForPane()
		return true, nil
	case "right":
		b.focusD = true
		if b.Focus != nil {
			b.Focus.Set("detail")
		}
		return true, nil
	case "h":
		b.cycleSource(-1)
		return true, b.loadDetail()
	case "l":
		b.cycleSource(1)
		return true, b.loadDetail()
	case "f":
		// `f` IS RETIRED.
		return false, nil
	case "r":
		// `r` REFRESHES WHAT IS ON SCREEN — the list AND the open detail — not just the list.
		return true, b.RefreshView()
	case "esc":
		// ESC CLEARS THE MULTI-SELECTION before it does anything else.
		if t := b.curTable(); t != nil && t.ClearMarks() {
			return true, nil
		}
		b.focusD = false
		b.setFocusForPane()
		return true, nil
	case " ", "space":
		// SPACE marks the cursor row — the operator's "spacebar can do multi-select" — and ENTER remains
		// the activate gesture (see the case below).
		if !b.focusD {
			// mark is only meaningful because a bulk action consumes it; on a list with none (the Themes pane)
			// it is invisible state the operator can neither use nor explain.
			if !b.activeMarkable() {
				return b.activateSelected()
			}
			if t := b.curTable(); t != nil {
				if id := t.SelectedID(); id != "" {
					t.ToggleMark(id)
					// Advance one row, so a run of spaces builds a selection the way every other
					// multi-select list does — without forcing space-then-down each time.
					t.Move(1)
					return true, b.loadDetail()
				}
			}
			return true, nil
		}
		// Detail focused: space keeps its activate meaning.
		b.focusD = false
		b.setFocusForPane()
		return true, nil
	case "enter":
		// Activate the selected row.
		return b.activateSelected()
	// Shift+Tab is deliberately NOT handled here.
	case "o":
		// expand/collapse a tree node (selectable Table/tree).
		if !b.focusD && b.curTable().Toggle() {
			return true, nil
		}
	}
	return false, nil
}

// afterHighlight is the landing path for a cursor MOVE: the screen's OnHighlight hook first (the
// Themes pane previews the palette live), then the row's own detail load.
func (b *Base) afterHighlight() tea.Cmd {
	var cmds []tea.Cmd
	if b.OnHighlight != nil {
		if c := b.OnHighlight(); c != nil {
			cmds = append(cmds, c)
		}
	}
	if c := b.loadDetail(); c != nil {
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// activateSelected runs the ACTIVATE gesture on the selected row: a screen's OnActivate first (the
// Themes pane applies the palette), then a row's detail load.
func (b *Base) activateSelected() (bool, tea.Cmd) {
	if b.OnActivate != nil {
		if handled, cmd := b.OnActivate(); handled {
			return true, cmd
		}
	}
	// ACTIVATION LOADS THE SELECTED ROW when the detail is not already showing it.
	if t := b.curTable(); t != nil {
		if id := t.SelectedID(); id != "" && id != b.detailID {
			b.focusD = true
			if b.Focus != nil {
				b.Focus.Set("detail")
			}
			return true, b.loadDetail()
		}
	}
	b.focusD = !b.focusD
	if b.focusD {
		b.Focus.Set("detail")
	} else {
		b.setFocusForPane()
	}
	return true, nil
}

// SetPaneFocus moves the keyboard between this screen's list and its detail pane, so the pane
// BORDERS show where the keyboard is.
func (b *Base) SetPaneFocus(detail bool) {
	b.focusD = detail
	if detail {
		if b.Focus != nil {
			b.Focus.Set("detail")
		}
		return
	}
	b.setFocusForPane()
}

// DetailFocused reports whether the detail pane holds the keyboard.
func (b *Base) DetailFocused() bool { return b.focusD }

func (b *Base) setFocusForPane() {
	if b.Focus != nil && b.active >= 0 && b.active < len(b.sources) {
		b.Focus.Set(b.sources[b.active].name)
	}
}

func (b *Base) curTable() *Table {
	if b.active < 0 || b.active >= len(b.sources) {
		return &Table{}
	}
	return b.sources[b.active].table
}

// SelectWhenLoaded asks the next load of a source to focus the row with id.
func (b *Base) SelectWhenLoaded(src, id string) {
	if id == "" {
		return
	}
	b.pendingMu.Lock()
	if b.pendingSelect == nil {
		b.pendingSelect = map[string]string{}
	}
	b.pendingSelect[src] = id
	b.pendingMu.Unlock()
}

// ShowEntity brings an entity into view AND makes its own detail the pane's content — the "take me
// to X" gesture (a jump, or a follow-through from another pane).
func (b *Base) ShowEntity(src, id string) {
	b.SelectWhenLoaded(src, id)
	if id == "" {
		return
	}
	b.pendingMu.Lock()
	if b.pendingOwn == nil {
		b.pendingOwn = map[string]string{}
	}
	b.pendingOwn[src] = id
	b.pendingMu.Unlock()
}

// ownsPending reports whether a jump currently CLAIMS this source's detail pane, leaving the claim
// in place.
func (b *Base) ownsPending(src string) bool {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	_, ok := b.pendingOwn[src]
	return ok
}

// clearPendingOwn drops a source's claim.
func (b *Base) clearPendingOwn(src string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	delete(b.pendingOwn, src)
}

// focusPending applies (and clears) a pending selection for a source once its rows have landed.
func (b *Base) focusPending(src string, t *Table) {
	b.pendingMu.Lock()
	id := b.pendingSelect[src]
	b.pendingMu.Unlock()
	if id == "" {
		return
	}
	if t.visIndexOf(id) < 0 { // hidden or absent: keep waiting
		return
	}
	t.setCursorToID(id)
	t.clampOffset()
	b.pendingMu.Lock()
	delete(b.pendingSelect, src)
	b.pendingMu.Unlock()
}

// ActiveTable exposes the focused source's table to the owning screen (the
// Work screen's collapse/expand-all gesture reaches the tree through it).
func (b *Base) ActiveTable() *Table {
	if b.active < 0 || b.active >= len(b.sources) {
		return nil
	}
	return b.sources[b.active].table
}

// MarkedIDs returns the focused source's multi-selection in DRAW ORDER (nil = none).
func (b *Base) MarkedIDs() []string {
	t := b.curTable()
	if t == nil {
		return nil
	}
	return t.MarkedIDs()
}

// BulkThreshold is how many marked rows make a BULK selection.
const BulkThreshold = 2

// BulkIDs returns the marked ids when they constitute a BULK SELECTION, and nil otherwise.
func (b *Base) BulkIDs() []string {
	ids := b.MarkedIDs()
	if len(ids) < BulkThreshold {
		return nil
	}
	return ids
}

// MarkCount is how many rows are marked in the focused source.
func (b *Base) MarkCount() int {
	t := b.curTable()
	if t == nil {
		return 0
	}
	return t.MarkCount()
}

// ClearMarks drops the focused source's multi-selection, reporting whether there was one.
func (b *Base) ClearMarks() bool {
	t := b.curTable()
	if t == nil {
		return false
	}
	return t.ClearMarks()
}

// Marked reports whether a row id is in the focused source's multi-selection.
func (b *Base) Marked(id string) bool {
	t := b.curTable()
	return t != nil && t.IsMarked(id)
}

func (b *Base) cycleSource(delta int) {
	// Switching sources drops the selection: the marks belong to the list the operator was looking at,
	// and carrying them to another pane would make the next bulk action act on rows they cannot see.
	b.ClearMarks()
	n := len(b.sources)
	if n == 0 {
		return
	}
	b.active = ((b.active+delta)%n + n) % n
	b.focusD = false
	b.setFocusForPane()
}

func (b *Base) mouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Action == tea.MouseActionMotion || msg.Action == tea.MouseActionRelease {
		return nil
	}
	// AN OPEN INLINE EDITOR OWNS THE WHEEL.
	if b.editForm != nil {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if b.editForm.Wheel(-3) {
				return nil
			}
		case tea.MouseButtonWheelDown:
			if b.editForm.Wheel(3) {
				return nil
			}
		}
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if b.focusD {
			b.detail.Wheel(-3)
		} else {
			b.curTable().Wheel(-3)
		}
	case tea.MouseButtonWheelDown:
		if b.focusD {
			b.detail.Wheel(3)
		} else {
			b.curTable().Wheel(3)
		}
	case tea.MouseButtonLeft:
		p, isDetail := b.mouseRegion(msg.X)
		if isDetail {
			b.focusD = true
			b.Focus.Set("detail")
			return nil
		}
		if p < 0 || p >= len(b.sources) {
			return nil
		}
		s := b.sources[p]
		// A click on a top-row control (the tree's collapse/expand-all, the sort
		// control) fires it and forwards its command.
		if hit, cmd := b.clickRowAction(p, msg.Y, msg.X); hit {
			b.active = p
			b.focusD = false
			b.setFocusForPane()
			return cmd
		}
		row := msg.Y - b.tableTopRow()
		// A click on a tree node's +/- marker toggles that node; any other click on the row selects it.
		if row >= 0 && s.table.ToggleAt(row, msg.X) {
			b.active = p
			b.focusD = false
			b.setFocusForPane()
			return nil
		}
		if row >= 0 && s.table.Click(row) {
			b.active = p
			b.focusD = false
			b.setFocusForPane()
			// A CLICK MOVES THE CURSOR TOO, so it fires the same hook an arrow does — the operator's
			// "moving through them with the arrow keys or clicking on them" is one gesture here.
			return b.afterHighlight()
		}
	}
	return nil
}

// shellChromeRows is the number of terminal rows the SHELL draws above a screen's body: the
// centered tab bar (row 0), its underline rule (row 1) and the one-row gap (row 2).
const shellChromeRows = 3

// ShellChromeRows exposes that offset.
func ShellChromeRows() int { return shellChromeRows }

// tableTopRow returns the number of terminal rows above the FIRST data row of the focused source
// pane: the shell chrome, the panel's top border (the title is embedded in it).
func (b *Base) tableTopRow() int {
	head := 0
	if b.active >= 0 && b.active < len(b.sources) {
		t := b.sources[b.active].table
		head = t.TitleRows() + t.HeaderRows() + b.sources[b.active].topRows()
	}
	return shellChromeRows + 1 + head
}

// It MUST describe the layout View() actually renders: two panes (the focused source, then the
// detail) with a one-cell gap.
func (b *Base) mouseRegion(x int) (int, bool) {
	// HideSources draws the DETAIL PANE ONLY (see View), so every column belongs to the detail — there
	// is no source pane on screen to click.
	if b.HideSources {
		return -1, true
	}
	if len(b.sources) == 0 || b.width < 1 {
		return 0, true
	}
	lw, _ := b.twoPaneWidths(b.width)
	if x < lw {
		return b.active, false // the focused source pane
	}
	if x < lw+1 {
		return -1, false // the divider between the panes
	}
	return -1, true // the detail pane
}

func (b *Base) loadDetail() tea.Cmd {
	if b.detailFn == nil || b.active < 0 || b.active >= len(b.sources) {
		return nil
	}
	s := b.sources[b.active]
	id := s.table.SelectedID()
	if id == "" {
		return nil
	}
	// A CATEGORY ROW IS NOT AN ITEM, so there is nothing to load for it. Its id is a SYNTHETIC
	// sentinel that starts with a NUL byte (screenkit.groupRowIDPrefix), which is exactly why it
	// can never collide with a real ULID — and exactly why it must never be sent to the server.
	//
	// Without this guard, landing the cursor on a folder asked the server about the sentinel. For
	// most sources the detail function ignores an unknown id; for WORKFLOWS it reached Postgres,
	// which rejects a NUL in a parameter outright:
	//
	//	db: get workflow: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
	//
	// so the Workflows pane rendered "couldn't load this item" the moment it opened, on the first
	// row, before the operator had selected anything. The guard belongs HERE rather than in the
	// workflow detail function, because a synthesised row is a property of the LIST and every
	// grouped source has the same folder rows.
	if screenkit.IsGroupRow(id) {
		return nil
	}
	src := s.name
	// An explicit load ends any jump's claim on this pane: the operator (or the UI on their behalf — a
	// create focusing its new row) is choosing what to look.
	b.clearPendingOwn(src)
	fn := b.detailFn
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		title, fields, body, err := fn(ctx, src, id)
		if err != nil {
			return detailErrMsg{err: err}
		}
		return detailMsg{src: src, id: id, title: title, fields: fields, body: body}
	}
}

// RequestDetail loads the detail for (src, id) directly.
func (b *Base) RequestDetail(src, id string) tea.Cmd {
	if b.detailFn == nil {
		return nil
	}
	fn := b.detailFn
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		title, fields, body, err := fn(ctx, src, id)
		if err != nil {
			return detailErrMsg{err: err}
		}
		return detailMsg{src: src, id: id, title: title, fields: fields, body: body}
	}
}

// View renders the screen's panes. TWO PANES ONLY, matching the GUI: the FOCUSED source pane on the
// left and the detail pane on the right.
func (b *Base) View() string {
	if b.Stream != nil && b.Stream.Title != "" {
		return b.streamView()
	}
	if b.HideSources {
		return b.detailPaneView(b.width, b.height)
	}
	if len(b.sources) == 0 {
		return b.detail.View()
	}
	return b.SinglePane(b.width, b.height)
}

func (b *Base) paneHeight() int { return b.height }

// SinglePane is the hub-screen layout: the FOCUSED source pane beside the detail pane.
func (b *Base) SinglePane(w, h int) string {
	if w < 1 || h < 1 {
		return ""
	}
	lw, dw := b.twoPaneWidths(w)
	return JoinRow(b.focusedPaneView(lw, h), b.detailPaneView(dw, h))
}

// focusedPaneView renders the focused source pane sized to exactly w×h.
func (b *Base) focusedPaneView(w, h int) string {
	if b.active < 0 || b.active >= len(b.sources) {
		return NewPanel("", w, h).View()
	}
	s := b.sources[b.active]
	s.table.Width, s.table.Height = w, h-s.topRows()
	s.table.Focused = !b.focusD
	// The panel border carries the title; the embedded table must not repeat it.
	s.table.HideTitle = true
	p := NewPanel(s.title, w, h)
	p.Focused = s.table.Focused
	content := s.table.View()
	if s.topRows() > 0 {
		content = b.topLine(s, w) + "\n" + content
	}
	p.SetContent(content)
	return p.View()
}

// topLine renders the pane's top row: the search box (when the source is filterable) on the left,
// and the clickable row actions on the right.
func (b *Base) topLine(s *source, w int) string {
	inner := max(0, w-2)
	var left string
	if s.filterable {
		q := s.table.Filter
		if q == "" && !b.filtering {
			left = "/ search"
		} else {
			caret := ""
			if b.filtering {
				caret = "\u258f"
			}
			left = "/ " + q + caret
		}
		if q != "" {
			n, total := s.table.MatchCount()
			left += "  " + strconv.Itoa(n) + "/" + strconv.Itoa(total)
		}
	}
	// The MODE caption the screen asked for, if any (see SetCaption).
	caption := ""
	if s.caption != nil {
		caption = s.caption()
	}

	// A CONTROL IS DROPPED, NEVER CLIPPED: the recorded hit-box comes from where a control was DRAWN,
	// so a clipped control still holds a hit-box the operator cannot read.
	baseLeft := left
	b.actionHits = b.actionHits[:0]
	var labels []string
	// A live MULTI-SELECTION is stated on the same row, left of the controls: the operator
	// needs to know how many rows a bulk action will hit, and how to drop the selection.
	if n := b.MarkCount(); n > 0 {
		if n == 1 {
			baseLeft += "  1 marked (space adds, esc clears)"
		} else {
			baseLeft += fmt.Sprintf("  %d marked (esc clears)", n)
		}
	}
	// WHICH CONTROLS SURVIVE IS DECIDED IN DECLARATION ORDER, and a screen declares them
	// most-important-first.
	minLeft := lipgloss.Width(baseLeft) + 2
	room := inner - minLeft - 1
	kept := 0
	need := 0
	for i := 0; i < len(s.rowActions); i++ {
		w := lipgloss.Width("[ "+s.rowActions[i].Label()+" ]") + 1 // +1 for the join space
		if need+w > room {
			break
		}
		need += w
		kept++
	}
	// Lay the survivors out hard against the right edge, recording where each landed.
	end := inner - 1
	for i := kept - 1; i >= 0; i-- {
		label := "[ " + s.rowActions[i].Label() + " ]"
		labelW := lipgloss.Width(label)
		start := end - labelW + 1
		b.actionHits = append(b.actionHits, actionHit{src: s.name, i: i, x0: start, x1: end})
		labels = append([]string{label}, labels...)
		end = start - 2 // one space, then the next control
	}
	right := strings.Join(labels, " ")

	// The MODE caption, in whatever the search box and the reserved controls left over.
	left = baseLeft
	if caption != "" {
		const gap = 3
		avail := inner - lipgloss.Width(baseLeft) - lipgloss.Width(right) - gap - 1
		if avail >= lipgloss.Width(caption)+2 {
			if left != "" {
				left += strings.Repeat(" ", gap)
			}
			left += caption
		}
	}

	pad := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return theme.HintText.Render(Pad(left+strings.Repeat(" ", pad)+right, inner))
}

// clickRowAction invokes the row control under (absoluteY, x) when the click landed on one,
// returning the control's command so its effect can be reconciled.
func (b *Base) clickRowAction(p int, absoluteY, x int) (bool, tea.Cmd) {
	head := 0
	if b.active >= 0 && b.active < len(b.sources) {
		t := b.sources[b.active].table
		head = t.TitleRows() + t.HeaderRows()
	}
	topY := b.tableTopRow() - b.activePaneTopRows() - head
	if absoluteY != topY {
		return false, nil
	}
	// The recorded x is inside the border, so shift by the border + pane offset.
	for _, h := range b.actionHits {
		if h.src != b.sources[p].name {
			continue
		}
		if x-1 >= h.x0 && x-1 <= h.x1 {
			if h.i >= 0 && h.i < len(b.sources[p].rowActions) {
				return true, b.sources[p].rowActions[h.i].Do()
			}
		}
	}
	return false, nil
}

// activePaneTopRows is the focused source's top-row count.
func (b *Base) activePaneTopRows() int {
	if b.active < 0 || b.active >= len(b.sources) {
		return 0
	}
	return b.sources[b.active].topRows()
}

// detailPaneView renders the detail pane sized to exactly w×h.
func (b *Base) detailPaneView(w, h int) string {
	b.detail.Width, b.detail.Height = w, h
	title, content := "Detail", b.detail.View()
	if b.editForm != nil {
		title = b.editTitle
		if title == "" {
			title = "Edit"
		}
		// One border cell each side plus a little slack; the form windows its
		// values to this width so the caret is always on screen.
		b.editForm.Width = w - 4
		// The form also gets the height it may draw in, so a focused multi-line field can grow to fit the
		// pane instead of clamping to a fixed 6 rows.
		b.editForm.Height = h - 3
		form := b.editForm.View()
		content = form + "\n" + theme.HintText.Render("ctrl+s: save · esc: cancel")
		// The form is SCROLLED to follow its cursor, through a panel kept for the whole editing session.
		if b.editPanel == nil {
			b.editPanel = NewPanel(title, w, h)
		}
		p := b.editPanel
		p.Title, p.Width, p.Height = title, w, h
		p.Focused = b.focusD
		p.SetContent(content)
		// Two rows of context above the field, matching the step editor's rule.
		p.Scroll = b.editForm.FocusedRow(form) - 2
		p.clampScroll()
		return p.View()
	}
	p := NewPanel(title, w, h)
	p.Focused = b.focusD
	p.SetContent(content)
	return p.View()
}

func (b *Base) streamView() string {
	p := NewPanel(b.Stream.Title, b.width, b.height-1)
	p.Focused = true
	p.SetContent(b.Stream.View())
	out := p.View()
	if b.Bar != nil {
		out += "\n" + b.Bar.View()
	}
	return out
}

// ownsSource reports whether this screen has a source with that name.
func (b *Base) ownsSource(name string) bool {
	if name == "" {
		return false
	}
	for _, s := range b.sources {
		if s.name == name {
			return true
		}
	}
	return false
}

// SourceItem returns the named source's item by ID.
func (b *Base) SourceItems(name string) []Item {
	for _, s := range b.sources {
		if s.name != name {
			continue
		}
		out := make([]Item, 0, len(s.table.Rows))
		for _, r := range s.table.Rows {
			title := ""
			if len(r.Cells) > 0 {
				title = r.Cells[0]
			}
			out = append(out, Item{ID: r.ID, Title: title, Meta: r.Meta})
		}
		return out
	}
	return nil
}

func (b *Base) SourceItem(name, id string) (Item, bool) {
	for _, s := range b.sources {
		if s.name != name {
			continue
		}
		for _, r := range s.table.Rows {
			if r.ID == id {
				title := ""
				if len(r.Cells) > 0 {
					title = r.Cells[0]
				}
				return Item{ID: r.ID, Title: title, Meta: r.Meta}, true
			}
		}
	}
	return Item{}, false
}

// LoadItems seeds a source's rows directly (tests and optimistic refreshes
// that already hold the new page).
func (b *Base) LoadItems(source string, items []Item, next string) bool {
	for _, s := range b.sources {
		if s.name != source {
			continue
		}
		s.table.SetItems(items, next)
		s.table.Err = ""
		s.err = ""
		// Reconcile the multi-selection with what actually came back.
		s.table.PruneMarks()
		// A pending selection (a just-created entity) is applied here too: this is the synchronous load
		// path, so a caller that is not the shell's fetch command still focuses the new row.
		b.focusPending(s.name, s.table)
		// A direct load is a LANDING too, so the same reconciliation the fetch path gets applies (see
		// OnItemsLanded) — a test or an optimistic refresh that seeds rows must leave the pane in the same.
		if b.OnItemsLanded != nil {
			b.OnItemsLanded()
		}
		return true
	}
	return false
}

// MutateRow applies fn to the row with id in the named source (the
// optimistic local edit the mutation layer rolls back on failure).
func (b *Base) MutateRow(source, id string, fn func(*Row)) bool {
	for _, s := range b.sources {
		if s.name != source {
			continue
		}
		for i := range s.table.Rows {
			if s.table.Rows[i].ID == id {
				fn(&s.table.Rows[i])
				return true
			}
		}
	}
	return false
}

// RemoveRow drops a row locally (optimistic delete).
func (b *Base) RemoveRow(source, id string) bool {
	for _, s := range b.sources {
		if s.name != source {
			continue
		}
		for i := range s.table.Rows {
			if s.table.Rows[i].ID == id {
				s.table.Rows = append(s.table.Rows[:i], s.table.Rows[i+1:]...)
				if s.table.Cursor >= len(s.table.Rows) {
					s.table.Cursor = max(0, len(s.table.Rows)-1)
				}
				return true
			}
		}
	}
	return false
}

// Sources returns the screen's list panes (nav command generation).
func (b *Base) Sources() []SourceMeta {
	out := make([]SourceMeta, 0, len(b.sources))
	for _, s := range b.sources {
		out = append(out, SourceMeta{Name: s.name, Title: s.title})
	}
	return out
}

// ActiveSourceName returns the focused source's name.
func (b *Base) ActiveSourceName() string {
	if b.active < 0 || b.active >= len(b.sources) {
		return ""
	}
	return b.sources[b.active].name
}

// ActiveItem returns the selected item of the focused source.
func (b *Base) ActiveItem() (Item, bool) {
	if b.active < 0 || b.active >= len(b.sources) {
		return Item{}, false
	}
	r := b.sources[b.active].table.Selected()
	if r == nil {
		return Item{}, false
	}
	title := ""
	if len(r.Cells) > 0 {
		title = r.Cells[0]
	}
	return Item{ID: r.ID, Title: title, Meta: r.Meta}, true
}

// SelectSource focuses the named source.
func (b *Base) SelectSource(name string) bool {
	for i, s := range b.sources {
		if s.name == name {
			if b.active != i {
				// MOVING TO ANOTHER SOURCE DROPS THE SELECTION, for the same reason cycleSource does: the marks
				// belong to the list the operator was looking.
				b.ClearMarks()
			}
			b.active = i
			b.focusD = false
			b.setFocusForPane()
			return true
		}
	}
	return false
}

// SelectItem selects the item with the given ID in the named source.
func (b *Base) SelectItem(src, id string) bool {
	for i, s := range b.sources {
		if s.name != src {
			continue
		}
		b.active = i
		b.focusD = false
		b.setFocusForPane()
		for row, r := range s.table.Rows {
			if r.ID == id {
				s.table.Cursor = row
				return true
			}
		}
		return false
	}
	return false
}

// DetailFocusedForTest reports whether the DETAIL pane holds the keyboard (the base's own
// focus flag), so a test can assert where a key landed rather than inferring it.
func (b *Base) DetailFocusedForTest() bool { return b.focusD }

// DeliverFetchForTest hands the base a fetch result as if a list load had landed, and returns the
// follow-up command it produced.
func (b *Base) DeliverFetchForTest(src string, items []screenkit.Item, next string) tea.Cmd {
	_, cmd := b.Update(fetchedMsg{src: src, items: items, next: next})
	return cmd
}

// DeliverDetailForTest hands the base a detail payload as if its load had resolved, returning the
// follow-up command (the onDetail hook's work).
func (b *Base) DeliverDetailForTest(src, id, title, body string, fields []Field) tea.Cmd {
	_, cmd := b.Update(detailMsg{src: src, id: id, title: title, fields: fields, body: body})
	return cmd
}

// SetFocusForTest moves the keyboard to "detail" or "list" by name.
func (b *Base) SetFocusForTest(what string) {
	b.focusD = what == "detail"
	if b.focusD && b.Focus != nil {
		b.Focus.Set("detail")
		return
	}
	b.setFocusForPane()
}

// SourcesForTest exposes the registered sources for screen tests (the screenkit test hook, kept for
// kit2 screens).
func (b *Base) SourcesForTest() []screenkit.TestSource {
	out := make([]screenkit.TestSource, 0, len(b.sources))
	for _, s := range b.sources {
		var items []screenkit.Item
		for _, r := range s.table.Rows {
			title := ""
			if len(r.Cells) > 0 {
				title = r.Cells[0]
			}
			// Carry the tree metadata back too, so a test reading the source
			// sees the same tree shape the pane draws.
			items = append(items, screenkit.Item{
				ID:          r.ID,
				Title:       title,
				Meta:        r.Meta,
				Depth:       r.Depth,
				Parent:      r.Parent,
				HasChildren: r.Expand,
			})
		}
		out = append(out, screenkit.TestSource{Name: s.name, Fetch: s.fetch, Items: items})
	}
	return out
}

// Loading reports whether any pane is fetching.
func (b *Base) Loading() bool {
	for _, s := range b.sources {
		if s.table.Loading {
			return true
		}
	}
	return false
}
