package kit2

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
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

	// OnDialog runs when the open Dialog resolves ("" = dismissed).
	OnDialog func(choice string) tea.Cmd
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
	if len(b.sources) == 0 {
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

func (b *Base) loadSource(i int, pageToken string) tea.Cmd {
	s := b.sources[i]
	if s.fetch == nil {
		return nil
	}
	s.table.Loading = true
	name := s.name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		items, next, err := s.fetch(ctx, pageToken)
		return fetchedMsg{src: name, items: items, next: next, append: pageToken != "", err: err}
	}
}

func (b *Base) loadMore() tea.Cmd {
	if b.active < 0 || b.active >= len(b.sources) {
		return nil
	}
	s := b.sources[b.active]
	if s.table.NextPageToken == "" {
		return nil
	}
	return b.loadSource(b.active, s.table.NextPageToken)
}

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

// SetDetailContent pushes live content into the open detail pane.
func (b *Base) SetDetailContent(title string, fields []Field, body string) {
	b.detail.SetContent(title, fields, body)
}

// SetExecutor installs the single mutation executor (feedback + reconcile).
func (b *Base) SetExecutor(e *mutate.Executor) { b.Exec = e }

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
	return e.Apply(res)
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
					rows = append(rows, Row{ID: it.ID, Cells: []string{it.Title}, Meta: it.Meta})
				}
				s.table.AppendRows(rows, msg.next)
			} else {
				s.table.SetItems(msg.items, msg.next)
			}
			s.table.Loading = false
			if b.noAutoDetail {
				return true, nil
			}
			return true, b.loadDetail()
		}
		return true, nil

	case detailMsg:
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
	case "left", "h":
		b.cycleSource(-1)
		return true, b.loadDetail()
	case "right", "l":
		b.cycleSource(1)
		return true, b.loadDetail()
	case "f":
		return true, b.loadMore()
	case "r":
		return true, b.Refresh(b.ActiveSourceName())
	case "esc":
		b.focusD = false
		b.setFocusForPane()
		return true, nil
	case "enter", "tab":
		b.focusD = !b.focusD
		if b.focusD {
			b.Focus.Set("detail")
		} else {
			b.setFocusForPane()
		}
		return true, nil
	case "shift+tab":
		// Shared focus model: Shift+Tab cycles the region ring in reverse
		// and the detail/pane parity follows — key and mouse agree.
		b.Focus.Prev()
		b.focusD = b.Focus.Current() == "detail"
		return true, nil
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

func (b *Base) cycleSource(delta int) {
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
		row := msg.Y - 2 // line 0 title, line 1 column header
		if row >= 0 && s.table.Click(row) {
			b.active = p
			b.focusD = false
			b.setFocusForPane()
			return b.loadDetail()
		}
	}
	return nil
}

// mouseRegion maps column x to a pane index (or the detail).
func (b *Base) mouseRegion(x int) (int, bool) {
	if len(b.sources) == 0 || b.width < 1 {
		return 0, true
	}
	ws := b.regionWidths()
	col := 0
	for i, w := range ws {
		end := col + w
		if x < end {
			if i == len(ws)-1 {
				return -1, true
			}
			return i, false
		}
		col = end + 1 // gap
	}
	return -1, true
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

// View renders the source panes + detail, all inside kit2 Panels.
func (b *Base) View() string {
	if b.Stream != nil && b.Stream.Title != "" {
		return b.streamView()
	}
	if len(b.sources) == 0 {
		return b.detail.View()
	}
	ws := b.regionWidths()
	panels := make([]string, 0, len(b.sources)+1)
	for i, s := range b.sources {
		s.table.Width, s.table.Height = ws[i], b.paneHeight()
		s.table.Focused = i == b.active && !b.focusD
		p := NewPanel(s.title, ws[i], b.height)
		p.Focused = s.table.Focused
		p.SetContent(s.table.View())
		panels = append(panels, p.View())
	}
	dpanel := NewPanel("Detail", ws[len(ws)-1], b.height)
	dpanel.Focused = b.focusD
	dpanel.SetContent(b.detail.View())
	panels = append(panels, dpanel.View())
	out := JoinRow(panels...)
	if b.Bar != nil {
		out += "\n" + b.Bar.View()
	}
	return out
}

func (b *Base) paneHeight() int { return b.height }

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

// SourceItem returns the named source's item by ID.
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
			items = append(items, screenkit.Item{ID: r.ID, Title: title, Meta: r.Meta})
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
