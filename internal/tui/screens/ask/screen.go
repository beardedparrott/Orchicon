// Package ask implements the Ask Orchicon screen: conversations + messages (read-only v1 —
// ChatStream is a mutation surface and is excluded per plan §6).
package ask

import (
	"context"
	"strings"
	"sync"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

	// consent is the adopted card state — the SAME *chat.ConsentState the
	// transcript item holds (see consent.go). consentID is its ask id.
	consent   *chat.ConsentState
	consentID string
	// consentDeferred is the ask id whose claim the operator released with ctrl+g, so the keyboard can
	// go to the composer WITHOUT recording a decision on the card.
	consentDeferred string

	// draft is the adopted FREE-TEXT ROW of a RECORDED ask card — the SAME *chat.ParsedAsk the
	// transcript item holds, so this screen mutates exactly what the renderer draws (see askdraft.go).
	draft    *chat.ParsedAsk
	draftKey string
	// draftDeferred is the item key whose claim ctrl+g released, so the operator can reach the composer
	// WITHOUT losing what they had typed into the row.
	draftDeferred string

	// askCard is the adopted CURSOR CARD — the same *chat.ParsedAsk the transcript item holds, so the
	// keys here move exactly the highlight renderAskCardSpans draws (see askcard.go).
	askCard         *chat.ParsedAsk
	askCardKey      string
	askCardDeferred string

	// ov is the open list overlay (/grants, /permissions, add-rule). The screen
	// owns its keys while it is up (ClaimsKeys).
	ov *askOverlay

	// metaTitle/metaFields are the conversation header this screen fetched (GetConversation).
	metaMu     sync.Mutex
	metaTitle  string
	metaFields []screenkit.Field

	// w/h are the region the shell sized this screen to.
	w, h int
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
	// THE TRANSCRIPT IS THE SHELL'S TO PAINT.
	m.Base.SetDetailBodyHostOwned(true)
	m.AddSource("conversations", "Conversations", m.fetchConversations)
	m.SetDetail(m.detail)
	m.SetOnDetail(m.onDetail)
	// The GUI never auto-opens a conversation at launch: the transcript pane shows the hero until the
	// operator picks one or sends the first message (which creates a new conversation).
	m.Base.SetNoAutoDetail(true)
	m.Base.SetHero(HeroTitle, HeroBody)
	m.Base.SetStatuses(nil) // no live stream in read-only v1
	return m
}

func (m *Model) Name() string { return "ask" }

// Close unsubscribes (no live subs in v1).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.w, m.h = w, h; m.Base.SetSize(w, h) }

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
	// The transcript BODY is deliberately NOT built here.
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
	case tea.KeyMsg:
		// THE CARD OWNS THE KEYS WHILE IT IS PENDING — routed AHEAD of Base so a decision key can never be
		// read as a screen action.
		if cmd, handled := m.handleConsentKey(msg); handled {
			return m, cmd
		}
		if cmd, handled := m.handleAskCardKey(msg); handled {
			return m, cmd
		}
		// The same for a RECORDED card's open free-text row: it is an input the operator is
		// typing into, so what they type is their answer and not a screen action.
		if cmd, handled := m.handleAskDraftKey(msg); handled {
			return m, cmd
		}
		if cmd, handled := m.handleOverlayKey(msg); handled {
			return m, cmd
		}
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
	// The hint NAMES the pane gesture and what the vertical keys do here, because a chord nobody can
	// see is a chord nobody has.
	b.WriteString(theme.HintText.Render("←/→: select the rail or the conversation · ↑/↓, PgUp/PgDn: move the rail's selection, or scroll the conversation — whichever is selected"))
	b.WriteString("\n")
	b.WriteString(theme.HintText.Render("r: refresh · ctrl+g: chat composer — send from any screen; replies stream into the open conversation · ctrl+o: fold the newest reasoning block"))
	b.WriteString("\n")
	// THE SURFACES ARE SLASH COMMANDS, NOT BARE LETTERS.
	b.WriteString(theme.HintText.Render("/grants: the directories allowed for this session (revocable) · /permissions: the persistent allow/deny list"))
	body := b.String()
	if m.w > 0 && m.h > 0 {
		// THE OVERLAY IS SPLICED OVER THE PANE, NOT APPENDED UNDER IT.
		body = kit2.FitLines(body, m.w, m.h)
		if ov := m.overlayView(); ov != "" {
			// A STANDARD MODAL: a bordered panel, centered, over a DARKENED backdrop.
			body = kit2.CenterOnDimmed(body, modalBox(ov, m.w), m.w, m.h)
		}
	}
	return m.Base.Frame(body)
}

// modalRows normalizes an overlay into ONE OPAQUE BLOCK whose rows share a width, so splicing it
// over the pane can neither leave a hole nor break the splice with a ragged row.
func modalRows(content string, maxW int) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	w := 0
	for _, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	if maxW > 4 && w > maxW-2 {
		w = maxW - 2
	}
	for i, l := range lines {
		lines[i] = theme.OpaquePanel(l, w)
	}
	return strings.Join(lines, "\n")
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// OpenAskOverlay opens one of the Ask screen's list surfaces for a slash command: "grants" (the
// session roll-up) or "permissions" (the persistent allow/deny list).
func (m *Model) OpenAskOverlay(kind string) tea.Cmd {
	switch kind {
	case "grants":
		return m.openGrants()
	case "permissions":
		return m.openPermissions()
	}
	return nil
}

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// DetailFieldCount reports how many field rows the pane will actually draw, so the shell can size
// the transcript Stream to the pane's REAL body height. WHY THE SHELL CANNOT GUESS THIS.
func (m *Model) DetailFieldCount() int { return m.Base.DetailFieldCount() }

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

// onDetail fires when the detail pane shows a conversation: the shell opens that conversation in
// the chat controller (transcript + live stream follow it).
func (m *Model) onDetail(src, id string) tea.Cmd {
	if src != "conversations" {
		return nil
	}
	type opener interface{ OpenAskConversation(id string) tea.Cmd }
	var cmds []tea.Cmd
	if shell, ok := m.Shell().(opener); ok {
		cmds = append(cmds, shell.OpenAskConversation(id))
	}
	// RE-SIZE AND REPAINT AFTER A LANDING. A detail landing REPLACES the pane's fields with the fetched
	// set (see kit2.Base's detailMsg case) and the Ask screen keeps the body, so the pane's field count —
	// and therefore its body height — CHANGES as the landing arrives:
	//
	//	DetailBodyHeight(1,true)=23    DetailBodyHeight(2,true)=22
	//
	// The shell sizes the transcript Stream from the fields it holds when IT repaints, which can be the
	// smaller pre-landing set. A stream one row taller than the pane draws is clipped by the pane's own
	// viewport at the BOTTOM — and the row it loses is the NOTICE, which is emitted last. That is why the
	// thinking indicator could be set on the widget and never reach the frame.
	//
	// Repainting through the shell's own path here re-measures against the state that just landed, so the
	// stream and the pane agree. It is a cache merge (no RPC), so it costs a render, not a round trip.
	if shell, ok := m.Shell().(interface{ RepaintTranscript() tea.Cmd }); ok {
		cmds = append(cmds, shell.RepaintTranscript())
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// RenderTranscript returns the detail title + fields for the shell's repaint.
func (m *Model) RenderTranscript(items []chat.ChatItem, live chat.Conversation, liveOK bool) (string, []screenkit.Field) {
	// RECONCILE FIRST, ALWAYS.
	m.SyncTranscriptConsent(items)
	m.SyncAskDraft(items)
	// AND THE RECORDED CARD'S CURSOR, from the same items and on the same wake — so a card that has been
	// answered, or that left the transcript entirely, gives the keyboard back here and nowhere else.
	m.SyncAskCard(items)
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
	if live.Title != "" {
		title = "Conversation: " + live.Title
		fields = setFieldValue(fields, "title", live.Title)
	}
	fields = setFieldValue(fields, "messages", screenkit.FmtInt(int(live.MessageN)))
	// THE SESSION-GRANT ROLL-UP.
	fields = setFieldValue(fields, "grants", m.grantsFieldValue())
	return title, fields
}

// grantsFieldValue is the `grants:` header row: how many directories this conversation has
// allowed for the session, and where to see them.
func (m *Model) grantsFieldValue() string {
	h, ok := m.consentHost()
	if !ok {
		return "(unavailable on this plane)"
	}
	if !h.ConsentGrantsLoaded(m.Base.DetailID()) {
		// NOT YET ASKED: "none" here would be a claim about the operator's permissions that nothing supports.
		return "(checking…) — /grants"
	}
	grants, available := h.ConsentGrants(m.Base.DetailID())
	if !available {
		return "(unavailable on this plane)"
	}
	if len(grants) == 0 {
		return "none — /grants"
	}
	return screenkit.FmtInt(len(grants)) + " dir(s) — /grants"
}

// setFieldValue replaces an existing field's value, appending when the key is absent — a header
// built without meta (the pre-meta fallback) still reports the live values instead of carrying
// only.
func setFieldValue(fields []screenkit.Field, key, value string) []screenkit.Field {
	for i := range fields {
		if fields[i].Key == key {
			fields[i].Value = value
			return fields
		}
	}
	return append(fields, screenkit.Field{Key: key, Value: value})
}

// modalBox wraps an overlay's content in the STANDARD modal panel: the bordered, titled box the
// shell's own modals use (/connect, help, the launch prompt).
func modalBox(content string, maxW int) string {
	inner := modalRows(content, maxW)
	lines := strings.Split(inner, "\n")
	if len(lines) == 0 {
		return ""
	}
	w := lipgloss.Width(lines[0])
	if w < 8 {
		w = 8
	}
	p := kit2.NewPanel("", w+2, len(lines)+2)
	p.SetContent(inner)
	return p.View()
}
