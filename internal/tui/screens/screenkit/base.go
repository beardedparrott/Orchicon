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
	statuses []StatusMsg
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

// Load fetches page 1 of every source (screens call from Init).
func (b *Base) Load() tea.Cmd {
	var cmds []tea.Cmd
	for i := range b.sources {
		cmds = append(cmds, b.loadSource(i, ""))
	}
	return tea.Batch(cmds...)
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
	title  string
	fields []Field
	body   string
}

type detailErrMsg struct{ err error }

// SetDetail installs the detail renderer for the given source name.
func (b *Base) SetDetail(fn DetailFn) { b.detailFn = fn }

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
				s.list.Err = msg.err.Error()
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
		b.detail.SetContent("detail error", []Field{{Key: "error", Value: msg.err.Error()}}, "")
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
		return detailMsg{title: title, fields: fields, body: body}
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
