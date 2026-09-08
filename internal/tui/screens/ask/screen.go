// Package ask implements the Ask Orchicon screen: conversations +
// messages (read-only v1 — ChatStream is a mutation surface and is
// excluded per plan §6).
package ask

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Ask screen.
type Model struct {
	screenkit.Base
	cl *client.Clients
	reg *subs.Registry
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry) *Model {
	m := &Model{cl: cl, reg: reg}
	m.NameStr = "ask"
	m.AddSource("conversations", "Conversations", m.fetchConversations)
	m.SetDetail(m.detail)
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
	// Transcript trails in the body (ListMessages).
	var body strings.Builder
	if mr, err := m.cl.Ask.ListMessages(ctx, connect.NewRequest(&apiv1.ListMessagesRequest{
		ConversationId: id,
		PageSize:       100,
	})); err == nil {
		for _, msg := range mr.Msg.GetMessages() {
			role := strings.ToUpper(msg.GetRole())
			body.WriteString(theme.ListTitle.Render(role) + "\n")
			content := msg.GetContent()
			if len(content) > 2000 {
				content = content[:2000] + " …"
			}
			body.WriteString(content + "\n\n")
		}
	}
	return "Conversation: " + c.GetTitle(), fields, strings.TrimRight(body.String(), "\n"), nil
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
	b.WriteString(theme.HintText.Render("enter: detail focus · f: more pages · r: refresh · chat is read-only in orch v1 (use the GUI to converse)"))
	return b.String()
}
