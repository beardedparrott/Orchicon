// Package ask implements the Ask Orchicon screen: conversations +
// messages (read-only v1 — ChatStream is a mutation surface and is
// excluded per plan §6).
package ask

import (
	"context"
	"strings"
	"sync"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Ask screen.
type Model struct {
	kit2.Base
	cl  *client.Clients
	reg *subs.Registry

	// metaTitle/metaFields are the conversation header this screen fetched
	// (GetConversation). The SHELL owns the transcript body — it is the one
	// renderer that sees live chunks and preserves the operator's scroll — so the
	// screen caches the RICH header here for RenderTranscript to serve back,
	// instead of a second renderer painting its own copy of the transcript.
	//
	// metaMu: detail() runs off the update loop (a RequestDetail command) while
	// RenderTranscript runs on it.
	metaMu     sync.Mutex
	metaTitle  string
	metaFields []screenkit.Field
}

// The centered empty state (GUI parity): Ask lands on a fresh
// conversation and shows this until the first message is sent.
const (
	HeroTitle = "Ask Orchicon anything."
	HeroBody  = "Plan, execute, and govern with real-time clarity and thin control."
)

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry) *Model {
	m := &Model{cl: cl, reg: reg}
	m.NameStr = "ask"
	m.Base.HideSources = true
	// THE TRANSCRIPT IS THE SHELL'S TO PAINT. A conversation's detail payload carries title + fields and
	// NO BODY — by design, because the body is a merge of the durable conversation and the live chunk cache
	// that only the shell can see (see detail()). Without this declaration a detail landing ASSIGNS that
	// empty body and erases the transcript: "the conversation pane is completely blank on every chat". It
	// was reachable on every list reload, because Base's fetchedMsg handler ends by loading the detail.
	m.Base.SetDetailBodyHostOwned(true)
	m.AddSource("conversations", "Conversations", m.fetchConversations)
	m.SetDetail(m.detail)
	m.SetOnDetail(m.onDetail)
	// The GUI never auto-opens a conversation at launch: the transcript
	// pane shows the hero until the operator picks one or sends the first
	// message (which creates a new conversation).
	m.Base.SetNoAutoDetail(true)
	m.Base.SetHero(HeroTitle, HeroBody)
	m.Base.SetStatuses(nil) // no live stream in read-only v1
	return m
}

func (m *Model) Name() string { return "ask" }

// Close unsubscribes (no live subs in v1).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd { return m.Load() }

func (m *Model) fetchConversations(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Ask.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Conversations))
	for _, c := range resp.Msg.Conversations {
		meta := screenkit.FmtInt(int(c.GetMessageCount())) + " msgs"
		if c.GetTurnInFlight() {
			meta += " · running"
		}
		items = append(items, screenkit.Item{
			ID:    c.GetId(),
			Title: c.GetTitle(),
			Meta:  meta,
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	if src != "conversations" {
		return "", nil, "", nil
	}
	resp, err := m.cl.Ask.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: id}))
	if err != nil {
		return "", nil, "", err
	}
	c := resp.Msg.GetConversation()
	fields := []screenkit.Field{
		{Key: "id", Value: c.GetId()},
		{Key: "title", Value: c.GetTitle()},
		{Key: "model", Value: c.GetModelRef()},
		{Key: "messages", Value: screenkit.FmtInt(int(c.GetMessageCount()))},
		{Key: "mode", Value: strings.ToLower(c.GetMode().String())},
		{Key: "created", Value: screenkit.FmtTime(c.GetCreatedAt())},
		{Key: "updated", Value: screenkit.FmtTime(c.GetUpdatedAt())},
	}
	// The transcript BODY is deliberately NOT built here. The shell renders it
	// (onChatWake -> chat.RenderItems through the kit2 Stream), because that is the
	// only renderer that sees live chunks and preserves the operator's scroll
	// offset. This function used to build a second, competing copy from
	// ListMessages — and, because the server returns that page NEWEST-FIRST (see
	// db.ListMessages' ORDER BY created_at DESC), that copy was INVERTED: the
	// model's reply printed ABOVE the operator's message, and a long reply pushed
	// the message out of the visible area entirely (the operator's "my initial user
	// message is still not showing up"). Sibling conversationItems had been fixed
	// for exactly this ordering; this path had not. Rather than fix the duplicate,
	// delete it: one transcript, one renderer.
	m.metaMu.Lock()
	m.metaTitle, m.metaFields = "Conversation: "+c.GetTitle(), fields
	m.metaMu.Unlock()
	return "Conversation: " + c.GetTitle(), fields, "", nil
}

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	}
	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.Base.View())
	b.WriteString("\n")
	// The hint NAMES the pane gesture and what the vertical keys do here, because a chord
	// nobody can see is a chord nobody has — the operator's "what am I supposed to hit to
	// scroll a conversation" was exactly that gap. Two lines so it stays readable at the
	// widths this pane gets.
	b.WriteString(theme.HintText.Render("←/→: select the rail or the conversation · ↑/↓, PgUp/PgDn: move the rail's selection, or scroll the conversation — whichever is selected"))
	b.WriteString("\n")
	b.WriteString(theme.HintText.Render("r: refresh · ctrl+g: chat composer — send from any screen; replies stream into the open conversation · ctrl+o: fold the newest reasoning block"))
	return m.Base.Frame(b.String())
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// RefreshView re-reads the rail's LIST and deliberately does NOT re-request the open detail.
//
// THE SHELL'S DEFAULT ERASES THIS PANE. kit2.Base.RefreshView reloads the active source AND re-requests
// the open detail's payload — right for every other screen, and fatal here. ask.detail returns body=""
// BY DESIGN (see its own note: the shell paints the transcript from the live chunk cache, and this pane's
// body is not the server's to send), so a re-request lands a detailMsg whose body is EMPTY, and Base's
// handler SETS it:
//
//	b.detail.SetContent(msg.title, msg.fields, msg.body)
//
// which wipes whatever onChatWake had just painted. The operator saw it at once: "the conversation pane is
// completely blank on every chat." Reproduced before the fix: after onChatWake the pane held the
// transcript, and after one detail landing it held title + fields with a body of length 0.
//
// WHY THE TICK TRIGGERED IT. The rolling refresh window calls this hook every few seconds, so the wipe was
// not a rare race — it was the steady state, and the older code path that only refreshed on an explicit
// `r` made it intermittent enough to look like something else.
//
// Nothing is lost by leaving the body out: onChatWake repaints it on every wake AND on every tick, from
// the cache, with no RPC and no server round trip.
func (m *Model) RefreshView() tea.Cmd {
	// The LIST only. The body belongs to onChatWake.
	return m.Base.Refresh(m.Base.ActiveSourceName())
}

// ActiveSourceName / ActiveItem expose the Base focus state to the
// shell's context engine.
func (m *Model) ActiveSourceName() string           { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (screenkit.Item, bool) { return m.Base.ActiveItem() }

// NewChat resets the transcript pane to the empty hero (the shell's "new
// chat" affordance, mirroring the GUI's New chat button).
func (m *Model) NewChat() { m.Base.ClearDetail() }

// ScrollDetail scrolls the transcript pane (mouse wheel + the
// empty-composer vertical keys).
func (m *Model) ScrollDetail(delta int) { m.Base.ScrollDetail(delta) }

// DetailWidth exposes the detail pane width (transcript bubble wrap).
func (m *Model) DetailWidth() int { return m.Base.DetailWidth() }

// onDetail fires when the detail pane shows a conversation: the shell
// opens that conversation in the chat controller (transcript + live
// stream follow it).
func (m *Model) onDetail(src, id string) tea.Cmd {
	if src != "conversations" {
		return nil
	}
	type opener interface{ OpenAskConversation(id string) tea.Cmd }
	if shell, ok := m.Shell().(opener); ok {
		return shell.OpenAskConversation(id)
	}
	return nil
}

// RenderTranscript returns the detail title + fields for the shell's repaint.
//
// Only the BODY belongs to the shell (see detail()). The header comes from the
// meta this screen already fetched, so the shell does not downgrade the pane to a
// two-field summary the moment it repaints — with the live conversations-list row
// overlaid on top of it, because that cached fetch can predate the conversation's
// title and its first message (see the staleness comment in the body).
func (m *Model) RenderTranscript(items []chat.ChatItem, live chat.Conversation, liveOK bool) (string, []screenkit.Field) {
	m.metaMu.Lock()
	title, fields := m.metaTitle, append([]screenkit.Field{}, m.metaFields...)
	m.metaMu.Unlock()
	if title == "" {
		// The meta has not landed yet (the pane was opened without a detail round
		// trip): say something honest rather than nothing.
		id := m.Base.DetailID()
		title, fields = "Conversation: "+id, []screenkit.Field{{Key: "id", Value: id}}
	}
	if !liveOK {
		return title, fields
	}
	// The cached header must not go STALE.
	//
	// detail() caches what GetConversation returned at the instant the pane opened,
	// and for a BRAND-NEW conversation that fetch races its own first turn: it reads
	// title='' and count=0 BEFORE the title is assigned and before any message is
	// written, and nothing ever re-reads it — so the pane kept reporting "title —"
	// and "messages 0" for a conversation that plainly had both, while the rail (a
	// fresh ListConversations) showed the real title beside it.
	//
	// The shell reloads the conversations list after every send, and since
	// db.ListConversations now carries message_count that row is authoritative for
	// both values. Overlaying it costs no RPC and cannot lag: the same list the rail
	// renders is the one the header reports.
	if live.Title != "" {
		title = "Conversation: " + live.Title
		fields = setFieldValue(fields, "title", live.Title)
	}
	fields = setFieldValue(fields, "messages", screenkit.FmtInt(int(live.MessageN)))
	return title, fields
}

// setFieldValue replaces an existing field's value, appending when the key is
// absent — a header built without meta (the pre-meta fallback) still reports the
// live values instead of carrying only an id.
func setFieldValue(fields []screenkit.Field, key, value string) []screenkit.Field {
	for i := range fields {
		if fields[i].Key == key {
			fields[i].Value = value
			return fields
		}
	}
	return append(fields, screenkit.Field{Key: key, Value: value})
}
