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
}

// topRows is the number of rows this source draws above the table's own rows
// (the search / controls row).
func (s *source) topRows() int {
	if s.filterable || len(s.rowActions) > 0 {
		return 1
	}
	return 0
}

// RowAction is one clickable control on a pane's top row.
//
// Label is a func so a control can report STATE at render time ("collapse all"
// vs "expand all") without the screen having to re-register it on every
// change. Do RETURNS a tea.Cmd because most controls need to re-fetch: a
// control that only mutated state and dropped its command looked inert — which
// is exactly why cycling the sort changed the label but not the list.
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

// Base is the kit2 screen model: multiple RPC-backed panes (kit2 Tables), one
// Detail, the shared Focus ring, a first-class Stream, an ActionBar, an open
// Dialog, and the single mutation Executor. It replaces screenkit.Base.
type Base struct {
	NameStr string

	sources []*source
	active  int
	detail  screenkit.Detail
	focusD  bool

	// editForm, when non-nil, replaces the detail pane's body with a typed
	// form: the screen's INLINE EDIT mode. Keys route to it while it is up.
	editForm  *Form
	editTitle string
	// OnEditDone is notified once when the inline editor closes.
	OnEditDone func(submitted bool)

	width, height int
	detailFn      DetailFn
	onDetail      func(src, id string) tea.Cmd
	detailID      string
	shell         any
	statuses      []StatusMsg

	// noAutoDetail suppresses the post-fetch auto-detail (the Ask screen's
	// hero stays until the operator picks an item or sends the first
	// message). heroTitle/heroBody are the centered empty state.
	noAutoDetail bool
	heroTitle    string
	heroBody     string

	// filtering is true while the operator is TYPING into the focused source's
	// filter box (the search row above the list).
	filtering bool

	// pendingSelect names a row to focus as soon as it APPEARS in a source —
	// used after a create, so the new entity is selected and visible instead of
	// silently landing below the fold while the cursor stays where it was.
	//
	// pendingOwn is the stronger claim a JUMP makes (ShowEntity): the detail it
	// requested must not be replaced by the row under the cursor when the landing
	// arrives.
	// Guarded because the mutation's Do runs off the update loop.
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

	// HideSources renders the detail pane ONLY (no source panes). Set by
	// screens whose list lives elsewhere — the Ask screen's conversations are
	// the shell's right rail, so the screen shows just the transcript and the
	// tab has exactly ONE conversation list.
	HideSources bool

	// OnDialog runs when the open Dialog resolves ("" = dismissed).
	OnDialog func(choice string) tea.Cmd

	// OnActivate, when set, handles Enter/Space on the selected row (list
	// focus). It reports whether it handled the activation: false falls through
	// to the default (focus the detail), true means the screen took it — which
	// lets a screen make activation do the natural thing for the selection (the
	// Themes pane APPLIES the highlighted palette).
	OnActivate func() (handled bool, cmd tea.Cmd)
}

// AddSource registers a fetchable list pane.
func (b *Base) AddSource(name, title string, fetch Fetch) {
	b.sources = append(b.sources, &source{
		name: name, title: title, fetch: fetch,
		table: NewTable(title, Column{Title: ""}),
	})
	b.rebuildFocus()
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

// SetSourceEmpty sets the named source's empty-state message. Every pane
// names WHY it is empty ("no traces in the window — …"); the bare
// "nothing here" default is never shown for a source that declares one.
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
	return SplitWidths(b.width, n, 1)
}

// SetSize lays out the panes (equal split + detail).
func (b *Base) SetSize(w, h int) {
	b.width, b.height = w, h
	// HideSources renders the DETAIL PANE ONLY, at the FULL width
	// (View → detailPaneView(b.width, b.height)). The stored detail width must
	// agree with that, because DetailWidth() is the WRAP width its callers lay
	// their content out with (the Ask transcript in particular).
	//
	// It used to be assigned the SPLIT width here — a fraction of w — while the
	// very next render silently overwrote it with the full width. Between those
	// two moments DetailWidth() disagreed with the pane being drawn: wider and the
	// right edge is TRUNCATED by the frame (which cuts a right-aligned operator
	// message down to its leading whitespace, and slices long lines mid-sentence);
	// narrower and the content is wrapped short of the pane it sits in.
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

// loadSource loads a source from the top, fetching the WHOLE set.
//
// IT FOLLOWS THE CURSOR INTERNALLY SO NOBODY HAS TO PAGINATE. The operator: "I don't understand
// concept of 'pages'. It's mentioned in the TUI, yet I never page to see more work items. I just hold
// the down arrow and I should be able to see all of them. Same goes for any other item in the TUI.
// Pages are unnecessary imo" — and they are right, from the operator's side: the pane is a scrolling
// list, and holding DOWN must reach the end of it, not the end of a page.
//
// The screen's fetch still speaks the RPC's paging protocol (it has to — that IS the API), so the
// loop lives HERE, once, for every source, instead of in each screen and instead of in the
// operator's head. The bound is a safety net against a server that never clears its token, not a
// display limit: a real tenant's lists are far smaller than the cap.
//
// The returned next-token is deliberately NOT propagated to the table: a token means "there is more
// you have not fetched", and after this loop there is not. That is what removes the "more pages: press
// f" affordance — not a hidden key, but the absence of anything left to fetch.
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
				// A FAILED page after a successful one still returns what was fetched: a partial list
				// beats an empty pane, and the operator keeps the rows they can already see. Only a
				// failure on the FIRST page is an error state.
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

// maxSourcePages bounds the whole-set load. A source that returns a token for ever would otherwise
// spin inside one command; this is a guard on a broken server, never a limit on what is displayed
// (every real list in a tenant is orders of magnitude smaller).
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

// SetNoAutoDetail suppresses the post-fetch auto-detail: the pane keeps the
// empty state the screen installed until the operator picks an item (the Ask
// screen's hero).
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

// SetShell installs the app shell reference.
func (b *Base) SetShell(sh any) { b.shell = sh }

// Shell returns the installed shell reference.
func (b *Base) Shell() any { return b.shell }

// SetOnDetail installs the detail-landed hook.
func (b *Base) SetOnDetail(fn func(src, id string) tea.Cmd) { b.onDetail = fn }

// DetailID returns the id the detail pane currently shows.
func (b *Base) DetailID() string { return b.detailID }

// DetailWidth returns the detail pane's render width.
func (b *Base) DetailWidth() int {
	if w := b.detail.Width; w > 10 {
		return w - 2
	}
	return 60
}

// SetDetailFooter installs a FIXED band at the bottom of the detail pane, outside
// the pane's scrolling region, so it is always on screen however tall the body
// is — the shape an input needs (screenkit.Detail.SetFooter explains why).
func (b *Base) SetDetailFooter(s string) { b.detail.SetFooter(s) }

// DetailFooter returns the pane's current fixed band.
func (b *Base) DetailFooter() string { return b.detail.Footer() }

// SetDetailScrollBottom pins the detail pane's viewport to its LAST row.
//
// It goes through the same pending-offset mechanism an editor's cursor uses, with an offset past
// the end: the viewport clamps to the last page, which is exactly "bottom". Adding a second
// scrolling path would mean two things could disagree about where the pane is, and this pane is
// already written by three producers.
func (b *Base) SetDetailScrollBottom() { b.detail.SetScrollOffset(1 << 20) }

// SetDetailScrollTop pins the detail pane's viewport to a LINE OFFSET.
//
// The pane's normal rule is "keep the operator's scroll" (a live transcript must
// not be yanked to the top). That is wrong for an editor, where the pane must
// follow the CURSOR: selecting step 9 of 11 has to bring step 9 into view. Callers
// that repaint on a selection change use this to place the cursor's step
// deliberately, and it is a no-op before the first paint.
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

// SetDetailBody replaces the detail pane's FIELDS and BODY while keeping its title — the
// in-place repaint a cursor move needs, where the item is unchanged and only what is drawn for it
// differs (the runs step flow moving its marker). Going through SetDetailContent would need the
// title, and every caller re-supplying it is a caller that can get it wrong.
func (b *Base) SetDetailBody(body string, fields []Field) {
	b.detail.SetContent(b.detail.Title, fields, body)
}

// DetailForTest exposes the pane's current contents, so a screen can REPAINT around a change
// without re-deriving what the fetch produced (and so tests can assert what is on screen).
func (b *Base) DetailForTest() (string, []Field, string) {
	return b.detail.Title, b.detail.Fields, b.detail.Body
}

// SetExecutor installs the single mutation executor (feedback + reconcile).
func (b *Base) SetExecutor(e *mutate.Executor) { b.Exec = e }

// SetOnEditDone registers the callback notified once when the inline
// detail-pane editor closes (submitted=true on save, false on cancel). A
// screen uses it for its status notice.
func (b *Base) SetOnEditDone(fn func(submitted bool)) { b.OnEditDone = fn }

// BeginDetailEdit installs an inline editor in the DETAIL pane and moves focus
// there, so an entity is edited in place rather than in a modal box — the
// operator's "edit it in the details pane as opposed to some small work item
// edit modal that pops up. That is more natural and honestly much more
// usable."
//
// The form is the same typed Form the modals use (caret editing, field
// navigation, validation, typed submit); only its HOST changes.
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

// finishDetailEdit closes the inline editor and reports the outcome once.
func (b *Base) finishDetailEdit(submitted bool) {
	b.editForm = nil
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

// EnableFilter marks a source as filterable: its pane draws a search row above
// the list and '/' focuses it (the operator's "search box at the top of the
// work items page").
func (b *Base) EnableFilter(src string) {
	for _, s := range b.sources {
		if s.name == src {
			s.filterable = true
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

// DropKeyClaim releases every reason this screen claims the keyboard, so a
// focus chord (ctrl+g) can hand control back to the composer.
//
// It exists because a claim is a LATCH: the search box stays focused until it is
// explicitly left, and while it is up the screen consumes every key. Without a
// way to drop it, the chord that is supposed to return the operator to the
// composer was itself swallowed — and the letters they typed next ran screen
// actions instead of inserting text.
//
// It deliberately does NOT discard the operator's work: the search QUERY is kept
// (StopFilter, not ClearFilter) so returning to the list finds it narrowed
// exactly as they left it. The inline editor is closed, since an edit in progress
// cannot stay open on an unfocused screen.
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
	//
	// The pane is a snapshot of the selected row taken when it was selected, and a
	// write changes that very row — pausing a recurring item flips the state the
	// pane displays, a rename changes its title, a status change is a field on it.
	// Reconcile only refreshes the LIST, and nothing re-selects the row, so the
	// pane kept showing the pre-mutation values: reported as "hitting 'p' on a
	// recurring item is not changing anything on the details pane".
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
			if b.noAutoDetail {
				return true, nil
			}
			// A JUMP owns the pane until its target has actually landed: the detail it asked for
			// is the one the operator wants, and the row under the cursor is not it when the
			// target is off this page (see ShowEntity).
			//
			// The claim is cleared when the JUMP'S DETAIL ARRIVES (detailMsg), not here — see
			// ownsPending for why consuming it on the first list landing is the bug rather than
			// the fix.
			if b.ownsPending(s.name) {
				return true, nil
			}
			return true, b.loadDetail()
		}
		return true, nil

	case detailMsg:
		// A detail payload belongs to the screen that REQUESTED it. Cmds are
		// asynchronous: Ask's RequestDetail("conversations", id) can resolve
		// after the operator has switched tabs, and the shell routes the
		// message to whatever screen is active — which then painted another
		// screen's conversation into its own detail pane (the operator's
		// "the conversation took over the Details pane of the theme view").
		// A screen only accepts details for a source it actually owns.
		if !b.ownsSource(msg.src) {
			return true, nil // consumed and dropped: not ours
		}
		b.detailID = msg.id
		b.detail.SetContent(msg.title, msg.fields, msg.body)
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

	// The inline detail editor owns EVERY key while it is up: it is the focused
	// surface, so a keystroke can never leak to the list behind it or to the
	// shell's chords ('q' would quit mid-edit).
	if b.editForm != nil {
		f := b.editForm
		switch msg.String() {
		case "esc":
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

	// The filter box owns the keys while the operator is typing into it: it is a
	// text input, so nothing may leak to the list or the shell ('q' would quit
	// mid-search, 'n' would open a create modal).
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
			return true, b.loadDetail()
		}
		return true, nil
	case "down", "j":
		if b.focusD {
			b.detail.Wheel(2)
		} else {
			b.curTable().Move(1)
			return true, b.loadDetail()
		}
		return true, nil
	case "left":
		// LEFT/RIGHT move focus BETWEEN THE TWO PANES below the tab bar: left takes
		// the source list for the submenu the operator selected, right takes the
		// detail. They used to rotate the whole tab bar (a global route, which ran
		// before the screen) — "left+right should not be moving the tab menu at the
		// top nor should it be rotating through the different screens".
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
		// `f` IS RETIRED. It was the pager ("more pages: press f"), which no longer exists: every
		// source is fetched whole, so there is never a next page to ask for (see loadSource).
		//
		// It is NOT claimed as a stub here, because `f` is a letter an operator might reasonably want
		// on a pane that has no other use for it, and a refusal for a concept that no longer exists
		// would be noise. It simply falls through to whatever the screen binds — which is how the
		// execution screen's message-box stub, and any future screen-level `f`, can own it.
		return false, nil
	case "r":
		return true, b.Refresh(b.ActiveSourceName())
	case "esc":
		// ESC CLEARS THE MULTI-SELECTION before it does anything else. That is the operator's
		// rule — "Esc clears the multi-select" — and it is also the safe order: an operator
		// mid-selection reaching for esc means "drop this selection", not "unfocus the pane".
		// With nothing marked, esc keeps its established meaning and falls through.
		if t := b.curTable(); t != nil && t.ClearMarks() {
			return true, nil
		}
		b.focusD = false
		b.setFocusForPane()
		return true, nil
	case " ", "space":
		// SPACE marks the cursor row — the operator's "spacebar can do multi-select" — and
		// ENTER remains the activate gesture (see the case below).
		//
		// Space used to toggle focus into the detail pane, which made it redundant with enter
		// and left no key for selecting more than one row. Acting on the DETAIL (while the
		// detail has focus) is left alone: marking is a list affordance.
		if !b.focusD {
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
		// Activate the selected row. A screen may install OnActivate to make
		// that do something concrete (the Themes pane applies the palette);
		// otherwise activation opens the row's detail.
		//
		// TAB is deliberately NOT here. It used to share this case, which made
		// Tab a PANE-FOCUS TOGGLE (list ↔ detail) on every kit2 screen — so the
		// screen consumed it and the shell's focus ring never saw the key. On the
		// Work tab that read as "the tabbing between the left and right pane
		// breaks the tab path in the top of the tab menu and grabs focus":
		// pressing Tab moved focus into the pane instead of cycling the tab bar.
		// Tab now falls through to the global focus ring (router.go), which is
		// the only thing that should own it.
		if b.OnActivate != nil {
			if handled, cmd := b.OnActivate(); handled {
				return true, cmd
			}
		}
		b.focusD = !b.focusD
		if b.focusD {
			b.Focus.Set("detail")
		} else {
			b.setFocusForPane()
		}
		return true, nil
	// Shift+Tab is deliberately NOT handled here. It used to cycle this screen's
	// region ring in reverse, which CONSUMED the key on every kit2 screen — so
	// walking the tab bar backwards was impossible from any pane, reported as
	// "Projects screen is stealing shift+tab". It now falls through to the
	// shell's reverse focus ring (router.go), the only thing that should own it.
	case "o":
		// expand/collapse a tree node (selectable Table/tree).
		if !b.focusD && b.curTable().Toggle() {
			return true, nil
		}
	}
	return false, nil
}

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
// Safe to call off the update loop (a mutation's Do runs in its own
// goroutine).
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

// ShowEntity brings an entity into view AND makes its own detail the pane's content — the
// "take me to X" gesture (a jump, or a follow-through from another pane).
//
// It is SelectWhenLoaded PLUS one guarantee, and that guarantee is load-bearing: a list landing
// auto-loads the detail of the row UNDER THE CURSOR (see the fetchedMsg case), and a jump's target
// is frequently NOT on the loaded page — the executions list is recent-first and paginated, so an
// older run's step execution is exactly the row that is missing. The landing then loaded whatever
// happened to be at the top, replacing the item the operator had just asked to see: the jump
// looked like it did nothing ("hitting enter on a step is not taking you to the execution page for
// that step").
//
// So a jump CLAIMS the pane: the detail it requested is the authority, and the next landing of
// that source leaves it alone. The claim is CONSUMED by that landing, so ordinary refreshes
// afterwards behave normally.
//
// Callers that only want the CURSOR moved (a freshly created row, whose detail should load the
// normal way) want SelectWhenLoaded instead — a create is not a jump.
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
//
// The claim used to be consumed on the first LIST landing, which read as equivalent and is not: a
// list landing arrives on every refresh (a live execution event pokes one continuously), and once
// the claim was spent the very next refresh auto-loaded the row under the cursor — so a jump to a
// target that is not on the loaded page was honoured for one refresh and then undone. That is the
// operator's "hitting enter on a step is not taking you to the execution page for that step": the
// pane switched, showed the right execution, and then slid back to the top of the executions list a
// beat later.
//
// The claim is therefore held until the OPERATOR takes control — clearPendingOwn is called by
// loadDetail, which is what a cursor move (or a row click) goes through. Releasing it when the
// jump's own detail landed would not work: the jump issues a list refresh AND a detail request, so
// the detail can arrive BEFORE the list, and the claim would be spent by the very event it had to
// survive.
func (b *Base) ownsPending(src string) bool {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	_, ok := b.pendingOwn[src]
	return ok
}

// clearPendingOwn drops a source's claim. It is called when an EXPLICIT load happens (a cursor
// move, a row click) — the operator choosing what to look at ends the jump — and deliberately not
// by RequestDetail, which is how the jump itself asks for its target.
func (b *Base) clearPendingOwn(src string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	delete(b.pendingOwn, src)
}

// focusPending applies (and clears) a pending selection for a source once its
// rows have landed. A row that is not present yet stays pending for the next
// load rather than being dropped.
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
//
// A screen reads this to offer its BULK actions: the operator's rule is that bulk options
// show up "only after you have more than one item selected", so a screen returns its bulk
// action list when len(MarkedIDs()) > 1 and its single-row actions otherwise. The action bar,
// the hint line and the key dispatch all follow from that one decision, because they are all
// built from the same action list.
func (b *Base) MarkedIDs() []string {
	t := b.curTable()
	if t == nil {
		return nil
	}
	return t.MarkedIDs()
}

// BulkThreshold is how many marked rows make a BULK selection.
//
// It lives here, in ONE place, so every screen that offers bulk actions applies the same rule
// rather than each restating "more than one" (the operator's ask was a consistent way to handle
// bulk operations on all forms).
const BulkThreshold = 2

// BulkIDs returns the marked ids when they constitute a BULK SELECTION, and nil otherwise.
//
// One marked row is NOT bulk: it is the row the cursor is on, and offering "archive 1 item"
// beside "archive" would be a slower way to do the same thing.
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
	// Switching sources drops the selection: the marks belong to the list the operator was
	// looking at, and carrying them to another pane would make the next bulk action act on
	// rows they cannot see.
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
		// A click on a tree node's +/- marker toggles that node; any other click
		// on the row selects it. Without this the marker was inert text and the
		// operator had to hunt for a key.
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
			return b.loadDetail()
		}
	}
	return nil
}

// shellChromeRows is the number of terminal rows the SHELL draws above a
// screen's body: the centered tab bar (row 0), its underline rule (row 1) and
// the one-row gap (row 2).
const shellChromeRows = 3

// ShellChromeRows exposes that offset. Mouse events arrive in TERMINAL-absolute
// coordinates while a screen renders from its own row 0, so anything hit-testing
// a click (or a test simulating one) must add this. It is part of the
// coordinate contract, not an implementation detail.
func ShellChromeRows() int { return shellChromeRows }

// tableTopRow returns the number of terminal rows above the FIRST data row of
// the focused source pane: the shell chrome, the panel's top border (the title
// is embedded in it), and the table's own title/header rows when it renders
// them.
//
// This is the correction for the operator's "I have to click above the item to
// select it": the embedded table keeps its title HIDDEN (the panel shows it)
// and does not render an empty header, so those rows no longer exist — and the
// count here describes exactly what View() draws.
func (b *Base) tableTopRow() int {
	head := 0
	if b.active >= 0 && b.active < len(b.sources) {
		t := b.sources[b.active].table
		head = t.TitleRows() + t.HeaderRows() + b.sources[b.active].topRows()
	}
	return shellChromeRows + 1 + head
}

// mouseRegion maps a terminal column to a source index, or to the detail pane.
//
// It MUST describe the layout View() actually renders: two panes (the focused
// source, then the detail) with a one-cell gap. It previously used the widths
// of the old ALL-panes grid, so the X hit-test was wrong on every screen with
// more than one source — clicking the detail pane mapped to a source that was
// not on screen, which is why clicking one thing selected another.
func (b *Base) mouseRegion(x int) (int, bool) {
	// HideSources draws the DETAIL PANE ONLY (see View), so every column belongs
	// to the detail — there is no source pane on screen to click.
	//
	// Without this the hit-test kept the two-pane assumption and a click in the
	// left half resolved to "the focused source pane", so Base.mouse ran
	// table.Click(row) against an INVISIBLE table and selected whatever row sat
	// at that screen Y. On the Ask tab that table is the conversations list, so
	// any click in the content region silently OPENED a conversation — the
	// operator's "clicking into the front page loads a conversation", which
	// picked an arbitrary old conversation rather than doing nothing.
	if b.HideSources {
		return -1, true
	}
	if len(b.sources) == 0 || b.width < 1 {
		return 0, true
	}
	ws := SplitWidths(b.width, 2, 1)
	if x < ws[0] {
		return b.active, false // the focused source pane
	}
	if x < ws[0]+1 {
		return -1, false // the gap between the panes
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
	src := s.name
	// An explicit load ends any jump's claim on this pane: the operator (or the UI on their
	// behalf — a create focusing its new row) is choosing what to look at, and that choice wins
	// over a jump in flight. RequestDetail does NOT clear it, which is how the jump asks for its
	// own target without cancelling itself.
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

// View renders the screen's panes.
//
// TWO PANES ONLY, matching the GUI: the FOCUSED source pane on the left and
// the detail pane on the right. The old behaviour drew EVERY source side by
// side, so a screen with three or four sources (Work carries Projects / Work
// Items / Runtime Images) squeezed them into columns a few runes wide —
// "it is showing everything on the screen at once in small panes" — and left
// no room for a usable detail pane. The remaining sources stay reachable via
// left/right (and the tab dropdown), and the detail pane is where a selected
// entity is inspected and edited (its action bar drives the mutations).
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

// SinglePane is the hub-screen layout: the FOCUSED source pane beside the
// detail pane. A screen with many sources (Control carries nine) renders one
// pane at a time — a nine-across grid truncates every cell to a handful of
// runes, so the operator can read nothing. Left/right (and Shift+Tab) still
// cycle the source ring; the panel title names the focused pane.
func (b *Base) SinglePane(w, h int) string {
	if w < 1 || h < 1 {
		return ""
	}
	ws := SplitWidths(w, 2, 1)
	return JoinRow(b.focusedPaneView(ws[0], h), b.detailPaneView(ws[1], h))
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

// topLine renders the pane's top row: the search box (when the source is
// filterable) on the left, and the clickable row actions on the right. As it
// renders it records where each action landed, so the next click can be
// hit-tested against what was actually drawn.
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

	// Lay the controls out from the right edge, recording each one's columns
	// (relative to the row's first cell inside the panel border).
	b.actionHits = b.actionHits[:0]
	var labels []string
	used := 0
	// A live MULTI-SELECTION is stated on the same row, left of the controls: the operator
	// needs to know how many rows a bulk action will hit, and how to drop the selection.
	if n := b.MarkCount(); n > 0 {
		if n == 1 {
			left += "  1 marked (space adds, esc clears)"
		} else {
			left += fmt.Sprintf("  %d marked (esc clears)", n)
		}
	}
	for i := len(s.rowActions) - 1; i >= 0; i-- {
		label := "[ " + s.rowActions[i].Label() + " ]"
		end := inner - used - 1
		start := end - lipgloss.Width(label) + 1
		b.actionHits = append(b.actionHits, actionHit{src: s.name, i: i, x0: start, x1: end})
		labels = append([]string{label}, labels...)
		used += lipgloss.Width(label) + 1
	}
	right := strings.Join(labels, " ")

	pad := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return theme.HintText.Render(Pad(left+strings.Repeat(" ", pad)+right, inner))
}

// clickRowAction invokes the row control under (absoluteY, x) when the click
// landed on one, returning the control's command so its effect can be
// reconciled.
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

// detailPaneView renders the detail pane sized to exactly w×h. In inline-edit
// mode the pane hosts the typed form in place of the read-only detail, so the
// operator edits the entity where they were already reading it.
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
		content = b.editForm.View() + "\n" + theme.HintText.Render("ctrl+s: save · esc: cancel")
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

// ownsSource reports whether this screen has a source with that name. Used to
// reject detail payloads that belong to another screen (cmds outlive the tab
// that issued them).
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
// SourceItems returns a source's currently loaded rows (a copy). It is what a
// BULK operation acts on: the scope of "accept all" must be the set the operator
// can SEE, and that is exactly the rows the table holds.
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
		// Reconcile the multi-selection with what actually came back. A bulk action DELETES
		// its rows, so without this the marks would linger on items that no longer exist and
		// the next bulk action would report acting on them.
		s.table.PruneMarks()
		// A pending selection (a just-created entity) is applied here too: this
		// is the synchronous load path, so a caller that is not the shell's
		// fetch command still focuses the new row.
		b.focusPending(s.name, s.table)
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
				// MOVING TO ANOTHER SOURCE DROPS THE SELECTION, for the same reason
				// cycleSource does: the marks belong to the list the operator was looking
				// at, and a selection they have stopped looking at is one they will act on
				// by mistake. Re-selecting the source they are already on keeps it.
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
//
// A list landing is where the auto-detail load happens (see the fetchedMsg case), and that step is
// INVISIBLE to a screen test: fetchedMsg is unexported, and the fetch functions go to the plane. A
// screen whose behaviour depends on what happens when the rows arrive — a JUMP that must not be
// clobbered by the row under the cursor — therefore had no way to be tested at all, which is how a
// jump that always lost the race shipped. This is that seam, in the same spirit as the rest of the
// ForTest helpers here.
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

// SourcesForTest exposes the registered sources for screen tests (the
// screenkit test hook, kept for kit2 screens). Items are the rows currently
// loaded (nil before the first fetch lands).
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
