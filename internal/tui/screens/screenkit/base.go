package screenkit

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
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
	stripH   int    // 1 when the source strip renders (>1 source), else 0
	// noAutoDetail suppresses the automatic detail load after a fetch:
	// the Ask screen keeps its hero until the operator deliberately opens
	// a conversation (the GUI never auto-opens one at launch).
	noAutoDetail bool
	heroTitle    string
	heroBody     string
	shell        any // the app shell (SetShell); screens type-assert for shell hooks
	statuses     []StatusMsg
}

// AddSource registers a fetchable list pane.
func (b *Base) AddSource(name, title string, fetch func(ctx context.Context, pageToken string) ([]Item, string, error)) {
	b.sources = append(b.sources, &source{name: name, title: title, fetch: fetch})
}

// SetStatuses seeds the status reporter (screen wires its stream names).
func (b *Base) SetStatuses(sts []StatusMsg) { b.statuses = sts }

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

// SetSize lays out: the ACTIVE source pane (left) + detail (right), with
// a one-row source strip on top when the screen has >1 source. The old
// all-panes-side-by-side math needed w/(n+1) per pane — at 7 sources
// (Control) that wanted ~186+ columns and pushed the detail off-screen,
// so selecting "settings" focused a pane you could not see (operator
// Phase-3.5 finding 4). One list pane has a fixed, bounded width; the
// detail always gets the remainder.
func (b *Base) SetSize(w, h int) {
	b.width, b.height = w, h
	n := len(b.sources)
	if n == 0 {
		b.stripH = 0
		b.paneW, b.paneH = 0, h
		b.detail.Width, b.detail.Height = w, h
		return
	}
	b.stripH = 0
	contentH := h
	if n > 1 && h > 1 {
		b.stripH = 1
		contentH = h - 1
	}
	lw := w / 3
	if lw < 24 {
		lw = 24
	}
	if lw > 48 {
		lw = 48
	}
	if w-lw-3 < 20 {
		lw = w - 3 - 20
		if lw < 12 {
			lw = 12
		}
	}
	if lw < 1 {
		lw = 1
	}
	b.paneW, b.paneH = lw, contentH
	b.detail.Width = w - lw - 3
	if b.detail.Width < 1 {
		b.detail.Width = 1
	}
	b.detail.Height = contentH
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

// SetNoAutoDetail suppresses the post-fetch auto-detail: the pane keeps
// the empty state the screen installed until the operator picks an item
// (the Ask screen's hero).
func (b *Base) SetNoAutoDetail(v bool) { b.noAutoDetail = v }

// SetHero installs the detail pane's centered empty state, shown until
// real content replaces it (and restored by ClearDetail).
func (b *Base) SetHero(title, body string) {
	b.heroTitle, b.heroBody = title, body
	b.detail.SetHero(title, body)
}

// ClearDetail returns the detail pane to its empty state (the hero when
// one is installed) and forgets the item it was showing.
func (b *Base) ClearDetail() {
	b.detailID = ""
	if b.heroTitle != "" || b.heroBody != "" {
		b.detail.SetHero(b.heroTitle, b.heroBody)
		return
	}
	b.detail.SetContent("", nil, "")
}

// ScrollDetail scrolls the detail pane by delta lines (mouse wheel + the
// empty-composer vertical keys).
func (b *Base) ScrollDetail(delta int) { b.detail.Wheel(delta) }

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
			if b.noAutoDetail {
				// Stay on the empty state: nothing is auto-selected (the
				// Ask screen shows its hero until a conversation is picked).
				return true, nil
			}
			return true, b.loadDetail()
		}
		return true, nil

	case detailMsg:
		b.detail.SetContent(msg.title, msg.fields, msg.body)
		// Record WHICH item the detail pane now shows. The shell's
		// onChatWake repaints the open conversation's transcript keyed on
		// DetailID() — a detailID that is never written makes that guard
		// always fail and live chat chunks silently never render
		// (operator Phase-3.5 finding 5: clicking a rail conversation
		// appeared to do nothing).
		b.detailID = msg.id
		// Fire the on-detail hook (ask transcript / execution session
		// follow). The hook's cmd is returned so Base.Update's caller
		// (the screen) batches it — without this the hook is stored but
		// never runs and chat targets never follow navigation.
		if b.onDetail != nil {
			return true, b.onDetail(msg.src, msg.id)
		}
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
		case "enter":
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
		// Route by WHERE the wheel is, not by keyboard focus: a wheel
		// over the detail pane scrolls the transcript even when the
		// list holds focus (operator finding: mouse scroll in chat
		// appeared dead — the wheel scrolled the list instead).
		if b.ClickDetail(msg.X) {
			b.detail.Wheel(-3)
		} else {
			b.sources[b.active].list.Wheel(-3)
		}
	case tea.MouseButtonWheelDown:
		if b.ClickDetail(msg.X) {
			b.detail.Wheel(3)
		} else {
			b.sources[b.active].list.Wheel(3)
		}
	case tea.MouseButtonLeft:
		// Strip row first: clicking a source title switches the list
		// pane (the mouse path for the >1-source strip).
		if b.stripH == 1 && msg.Y == 0 {
			if idx := b.stripSourceAt(msg.X); idx >= 0 && idx < len(b.sources) {
				b.active = idx
				b.focusD = false
				return b.loadDetail()
			}
			return nil
		}
		if len(b.sources) == 0 || b.ClickDetail(msg.X) {
			b.focusD = true
			return nil
		}
		s := b.sources[b.active]
		row := msg.Y - b.stripH - 1 // strip row + pane title line
		if row >= 0 && s.list.Click(row) {
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

// View renders the source strip (when >1 source) over the active source
// pane + detail side by side. Only the ACTIVE source's list renders as a
// pane — every source stays reachable via the strip (keyboard h/l,
// slash jumps, mouse) without needing 186+ columns.
func (b *Base) View() string {
	if len(b.sources) == 0 {
		return b.detail.View()
	}
	listBlock := b.renderPane(b.sources[b.active], true)
	detailBlock := b.detail.View()
	body := lipgloss.JoinHorizontal(lipgloss.Top, listBlock, PaneGap(), detailBlock)
	if b.stripH == 0 {
		return body
	}
	return b.sourceStripView() + "\n" + body
}

// sourceStripView renders the one-row source switcher: every source title
// in order, the active one highlighted. Titles truncate to fit; the strip
// never wraps (a wrapping strip would steal a budget row).
func (b *Base) sourceStripView() string {
	parts := make([]string, 0, len(b.sources))
	for i, s := range b.sources {
		if i == b.active {
			parts = append(parts, theme.ListItemSelected.Render(" "+s.title+" "))
		} else {
			parts = append(parts, theme.ListTitle.Render(" "+s.title+" "))
		}
	}
	strip := strings.Join(parts, theme.HintText.Render(" │ "))
	w := b.width
	if w < 1 {
		return strip
	}
	if lipgloss.Width(strip) > w {
		strip = ansi.Truncate(strip, w, "")
	}
	return strip
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
