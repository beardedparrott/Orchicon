package screenkit

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Screen is the contract every area screen implements. It lives here
// (not in package tui) because screens are tui subpackages — package tui
// imports screenkit, never the reverse.
type Screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (Screen, tea.Cmd)
	View() string
	Name() string
	SetSize(w, h int)
	Close()
}

// StatusReporter is the optional hook the shell uses for the footer's
// stream-status strip: the active screen reports its subscriptions'
// current statuses (worst wins in the footer).
type StatusReporter interface {
	ReportStatus() []StatusMsg
}

// StatusMsg is the neutral stream-status carrier the shell and screens
// agree on (subs.StatusMsg maps onto it).
type StatusMsg struct {
	Name   string
	Status string // mirrors stream.Status string values
}

// source is one RPC-backed list pane.
type source struct {
	name  string
	title string
	fetch func(ctx context.Context, pageToken string) ([]Item, string, error)
	list  List
}

// Base is the generic list+detail scaffolding every screen embeds:
// multiple sources (left panes), one detail (right pane), mouse + keys,
// pagination via next_page_token, and a stream-status reporter.
type Base struct {
	NameStr  string
	sources  []*source
	active   int // focused source
	detail   Detail
	focusD   bool // false = list focus, true = detail focus
	width    int
	height   int
	paneW    int
	paneH    int
	detailFn DetailFn
	onDetail func(src, id string) tea.Cmd
	detailID string // id of the item the detail pane currently shows
	shell    any    // the app shell (SetShell); screens type-assert for shell hooks
	statuses []StatusMsg
}

// AddSource registers a fetchable list pane.
func (b *Base) AddSource(name, title string, fetch func(ctx context.Context, pageToken string) ([]Item, string, error)) {
	b.sources = append(b.sources, &source{name: name, title: title, fetch: fetch})
}

// SetStatuses seeds the status reporter (screen wires its stream names).
func (b *Base) SetStatuses(sts []StatusMsg) { b.statuses = sts }

// SetSourceEmpty sets the named source's empty-state message. Every pane
// names WHY it is empty ("no traces in the window — …"); a bare
// "nothing here" is never shown.
func (b *Base) SetSourceEmpty(name, msg string) {
	for _, s := range b.sources {
		if s.name == name {
			s.list.EmptyMsg = msg
			return
		}
	}
}

// SetStatus updates one named status (screen translates subs.StatusMsg).
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

// SetSize lays out: equal-width source panes (left), rest to detail.
func (b *Base) SetSize(w, h int) {
	b.width, b.height = w, h
	n := len(b.sources)
	if n == 0 {
		b.paneW, b.paneH = 0, h
		b.detail.Width, b.detail.Height = w, h
		return
	}
	lw := w / (n + 1)
	if lw < 24 {
		lw = 24
	}
	b.paneW, b.paneH = lw, h
	b.detail.Width = w - lw*n - 3*(n-1)
	if b.detail.Width < 20 {
		b.detail.Width = 20
	}
	b.detail.Height = h
}

// PaneSize returns the per-source pane dimensions.
func (b *Base) PaneSize() (int, int) { return b.paneW, b.paneH }

// Frame normalizes a screen's composed block into the exact content
// region this screen was sized to (SetSize from the shell's
// WindowSizeMsg). Screens call m.Base.Frame(composed) from View so their
// panes fill the region; an unsized screen passes content through.
func (b *Base) Frame(content string) string {
	if b.width < 1 || b.height < 1 {
		return content
	}
	return Frame(content, b.width, b.height)
}

// HasAuthRetry reports whether any list pane is showing the inline
// re-auth retry state (an auth-expired fetch). The shell renders the
// re-auth banner exactly once, so it consults this to avoid duplicating
// the pane's own inline state.
func (b *Base) HasAuthRetry() bool {
	for _, s := range b.sources {
		if isAuthText(s.list.Err) {
			return true
		}
	}
	return false
}

// isAuthText reports whether an error string is the auth-expired shape.
func isAuthText(errtxt string) bool {
	l := strings.ToLower(errtxt)
	return strings.Contains(l, "unauthenticated") || strings.Contains(l, "unauthorized") ||
		strings.Contains(l, "re-authentication") || strings.Contains(l, "re-auth")
}

// Load fetches page 1 of every source (screens call from Init).
func (b *Base) Load() tea.Cmd {
	var cmds []tea.Cmd
	for i := range b.sources {
		cmds = append(cmds, b.loadSource(i, ""))
	}
	return tea.Batch(cmds...)
}

// friendlyFetchErr maps a raw RPC error to a human-readable, retryable
// state string (operator finding #9's auth-cascade half: a bare
// "error: unauthenticated" is never shown — auth expiry names the fix,
// transient failures name the retry).
func friendlyFetchErr(errText string) string {
	low := strings.ToLower(errText)
	switch {
	case strings.Contains(low, "unauthenticated"):
		return "session needs re-authentication — run /connect (esc cancels; the shell reconnects in place)"
	case strings.Contains(low, "permission_denied") || strings.Contains(low, "forbidden"):
		return "not permitted for this credential — check the API key's scopes (Settings → API keys) or re-authenticate with /connect"
	case strings.Contains(low, "deadline_exceeded") || strings.Contains(low, "context deadline") || strings.Contains(low, "timeout"):
		return "the plane took too long to answer — press r to refresh (transient timeouts retry)"
	case strings.Contains(low, "unavailable") || strings.Contains(low, "connection refused") || strings.Contains(low, "no such host") || strings.Contains(low, "connection refused"):
		return "the plane is unreachable right now — press r to refresh once it is back"
	case strings.Contains(low, "unavailable") || strings.Contains(low, "unimplemented"):
		return "this surface is not served by the connected plane — /connect to a plane with this feature"
	default:
		return errText
	}
}

// loadMore fetches the next page of the active source ("f" key).
func (b *Base) loadMore() tea.Cmd {
	s := b.sources[b.active]
	if s.fetch == nil || s.list.NextPageToken == "" {
		return nil
	}
	token := s.list.NextPageToken
	return b.loadSource(b.active, token)
}

func (b *Base) loadSource(i int, pageToken string) tea.Cmd {
	s := b.sources[i]
	if s.fetch == nil {
		return nil
	}
	name := s.name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		items, next, err := s.fetch(ctx, pageToken)
		return fetchedMsg{src: name, items: items, next: next, append: pageToken != "", err: err}
	}
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

// SetDetail installs the detail renderer for the given source name.
func (b *Base) SetDetail(fn DetailFn) { b.detailFn = fn }

// SetShell installs the app shell reference (screens type-assert it
// for shell-side hooks — avoids a screenkit→tui import cycle).
func (b *Base) SetShell(sh any) { b.shell = sh }

// Shell returns the installed shell reference (nil when unset).
func (b *Base) Shell() any { return b.shell }

// SetOnDetail installs a hook fired whenever a detail lands (detailMsg
// handling): the ask transcript / execution live-session views use it
// to (re)attach the live item stream for the entity the pane now shows.
// The returned cmd (if any) is batched by Base.Update's caller.
func (b *Base) SetOnDetail(fn func(src, id string) tea.Cmd) { b.onDetail = fn }

// DetailID returns the id of the item the detail pane currently shows
// ("" = none).
func (b *Base) DetailID() string { return b.detailID }

// DetailWidth returns the detail pane's render width (bubble wrapping).
func (b *Base) DetailWidth() int {
	if w := b.detail.Width; w > 10 {
		return w - 2
	}
	return 60
}

// SetDetailContent pushes live-updated content into the open detail
// pane without touching scroll state fields it owns (title/fields kept
// when body is unchanged by callers passing the same title).
func (b *Base) SetDetailContent(title string, fields []Field, body string) {
	b.detail.SetContent(title, fields, body)
}

// DetailFn renders an Item into detail content.
type DetailFn func(ctx context.Context, src, id string) (title string, fields []Field, body string, err error)

// Update handles shared behavior: fetch results, keys, mouse. Returns
// (handled, cmd) — screens call it first and handle only what remains.
func (b *Base) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case fetchedMsg:
		for _, s := range b.sources {
			if s.name != msg.src {
				continue
			}
			if msg.err != nil {
				s.list.Err = friendlyFetchErr(msg.err.Error())
				s.list.Loading = false
				return true, nil
			}
			if msg.append {
				items := append([]Item{}, s.list.Items...)
				items = append(items, msg.items...)
				s.list.Items = items
				s.list.NextPageToken = msg.next
			} else {
				s.list.SetItems(msg.items, msg.next)
			}
			s.list.Loading = false
			return true, b.loadDetail()
		}
		return true, nil

	case detailMsg:
		b.detail.SetContent(msg.title, msg.fields, msg.body)
		return true, nil

	case detailErrMsg:
		b.detail.SetContent("couldn't load this item", []Field{{Key: "error", Value: friendlyFetchErr(msg.err.Error())}}, "")
		return true, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if b.focusD {
				b.detail.Wheel(-2)
			} else {
				b.sources[b.active].list.Move(-1)
				return true, b.loadDetail()
			}
			return true, nil
		case "down", "j":
			if b.focusD {
				b.detail.Wheel(2)
			} else {
				b.sources[b.active].list.Move(1)
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
		case "esc":
			b.focusD = false
			return true, nil
		case "enter", "tab":
			b.focusD = !b.focusD
			return true, nil
		}

	case tea.MouseMsg:
		return true, b.mouse(msg)
	}
	return false, nil
}

func (b *Base) cycleSource(delta int) {
	n := len(b.sources)
	if n == 0 {
		return
	}
	b.active = ((b.active+delta)%n + n) % n
	b.focusD = false
}

func (b *Base) mouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Action == tea.MouseActionMotion || msg.Action == tea.MouseActionRelease {
		return nil // drag/release never consumed: Shift+drag stays native
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if b.focusD {
			b.detail.Wheel(-3)
		} else {
			b.sources[b.active].list.Wheel(-3)
		}
	case tea.MouseButtonWheelDown:
		if b.focusD {
			b.detail.Wheel(3)
		} else {
			b.sources[b.active].list.Wheel(3)
		}
	case tea.MouseButtonLeft:
		if len(b.sources) == 0 || b.ClickDetail(msg.X) {
			b.focusD = true
			return nil
		}
		p := b.mousePane(msg.X)
		if p < 0 || p >= len(b.sources) {
			return nil
		}
		s := b.sources[p]
		row := msg.Y - 1 // line 0 is the pane title
		if row >= 0 && s.list.Click(row) {
			b.active = p
			b.focusD = false
			return b.loadDetail()
		}
	}
	return nil
}

// loadDetail renders the selected item via the screen's DetailFn.
func (b *Base) loadDetail() tea.Cmd {
	if b.detailFn == nil {
		return nil
	}
	s := b.sources[b.active]
	it := s.list.Selected()
	if it == nil {
		return nil
	}
	src, id := s.name, it.ID
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

// View renders the source panes + detail side by side.
func (b *Base) View() string {
	if len(b.sources) == 0 {
		return b.detail.View()
	}
	panes := make([]string, 0, len(b.sources)+1)
	for i, s := range b.sources {
		panes = append(panes, b.renderPane(s, i == b.active))
	}
	panes = append(panes, b.detail.View())
	return strings.Join(panes, PaneGap())
}

func (b *Base) renderPane(s *source, focused bool) string {
	s.list.Title = s.title
	s.list.Width, s.list.Height = b.paneW, b.paneH
	var out strings.Builder
	out.WriteString(s.list.View(focused))
	if s.list.Loading {
		out.WriteString("\n" + Hint("loading…"))
	}
	return out.String()
}

// SourceItem returns the named source's item with the given ID (screens
// render detail-from-list when no Get-RPC exists).
func (b *Base) SourceItem(name, id string) (Item, bool) {
	for _, s := range b.sources {
		if s.name != name {
			continue
		}
		for _, it := range s.list.Items {
			if it.ID == id {
				return it, true
			}
		}
	}
	return Item{}, false
}

// SourceMeta is one list pane's identity, exposed for the shell's
// nav/slash generation (the no-drift source of truth: commands are
// generated from what screens actually have).
type SourceMeta struct {
	Name  string
	Title string
}

// Sources returns the screen's list panes (nav command generation).
func (b *Base) Sources() []SourceMeta {
	out := make([]SourceMeta, 0, len(b.sources))
	for _, s := range b.sources {
		out = append(out, SourceMeta{Name: s.name, Title: s.title})
	}
	return out
}

// ActiveSourceName returns the focused source's name ("" when none).
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
	it := b.sources[b.active].list.Selected()
	if it == nil {
		return Item{}, false
	}
	return *it, true
}

// SelectSource focuses the named source (slash nav); false when the
// screen has no such source.
func (b *Base) SelectSource(name string) bool {
	for i, s := range b.sources {
		if s.name == name {
			b.active = i
			b.focusD = false
			return true
		}
	}
	return false
}

// SelectItem selects the item with the given ID in the named source and
// triggers its detail load (slash arg jumps). Works without the item
// being visible (cursor move is by ID, not position). Returns false
// when the source has no such item (not fetched yet).
func (b *Base) SelectItem(src, id string) bool {
	for i, s := range b.sources {
		if s.name != src {
			continue
		}
		b.active = i
		b.focusD = false
		for row, it := range s.list.Items {
			if it.ID == id {
				s.list.Cursor = row
				s.list.clampOffset()
				return true
			}
		}
		// not on the loaded page — still focus the source; the caller
		// can request the detail directly.
		return false
	}
	return false
}

// RequestDetail loads the detail for (src, id) directly via the
// screen's DetailFn — detail works without the item being on the
// loaded page (plan §2). The result flows through the same detailMsg.
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

// SelectSourceDelegator / SelectItemDelegator / RequestDetailDelegator
// are optional screen interfaces the shell's slash navigation uses;
// Base implements them so every list+detail screen gets nav jumps.
