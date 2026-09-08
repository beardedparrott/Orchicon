// Package control implements the Control screen: workers, runtime
// images, secrets (names/metadata only — values are never rendered),
// MCP entries, and settings (admin-gated reads render an error state
// when the key's scopes are missing).
package control

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

// Model is the Control screen.
type Model struct {
	screenkit.Base
	cl  *client.Clients
	reg *subs.Registry
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry) *Model {
	m := &Model{cl: cl, reg: reg}
	m.NameStr = "control"
	m.AddSource("workers", "Workers", m.fetchWorkers)
	m.AddSource("images", "Runtime Images", m.fetchImages)
	m.AddSource("secrets", "Secrets (names only)", m.fetchSecrets)
	m.AddSource("mcp", "MCP Servers", m.fetchMCP)
	m.SetDetail(m.detail)
	m.Base.SetStatuses(nil) // no live stream on control (v1)
	return m
}

func (m *Model) Name() string { return "control" }

// Close unsubscribes (no live subs on this screen in v1).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd { return m.Load() }

func (m *Model) fetchWorkers(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Workers))
	for _, w := range resp.Msg.Workers {
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetName(),
			Meta:  strings.ToLower(w.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchImages(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.RuntimeImages))
	for _, im := range resp.Msg.RuntimeImages {
		items = append(items, screenkit.Item{
			ID:    im.GetId(),
			Title: im.GetName(),
			Meta:  strings.ToLower(im.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

// fetchSecrets lists names/metadata ONLY. Secret values are never
// fetched, rendered, or stored by orch (GetSecret is not called).
func (m *Model) fetchSecrets(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Secrets.ListSecrets(ctx, connect.NewRequest(&apiv1.ListSecretsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Secrets))
	for _, s := range resp.Msg.Secrets {
		items = append(items, screenkit.Item{
			ID:    s.GetId(),
			Title: s.GetName(),
			Meta:  "value hidden",
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchMCP(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Servers))
	for _, s := range resp.Msg.Servers {
		meta := "disabled"
		if s.GetEnabled() {
			meta = "enabled"
		}
		items = append(items, screenkit.Item{
			ID:    s.GetId(),
			Title: s.GetName(),
			Meta:  meta,
		})
	}
	return items, "", nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case "workers":
		resp, err := m.cl.Workers.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorker()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "name", Value: w.GetName()},
			{Key: "slug", Value: w.GetSlug()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "description", Value: w.GetDescription()},
			{Key: "purpose", Value: w.GetPurpose()},
			{Key: "current ver", Value: screenkit.FmtInt(int(w.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}
		return "Worker: " + w.GetName(), fields, w.GetDescription(), nil

	case "images":
		resp, err := m.cl.Images.GetRuntimeImage(ctx, connect.NewRequest(&apiv1.GetRuntimeImageRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		im := resp.Msg.GetRuntimeImage()
		fields := []screenkit.Field{
			{Key: "id", Value: im.GetId()},
			{Key: "name", Value: im.GetName()},
			{Key: "status", Value: strings.ToLower(im.GetStatus().String())},
			{Key: "base image", Value: im.GetBaseImageRef()},
			{Key: "tag", Value: im.GetTag()},
			{Key: "apt packages", Value: im.GetAptPackages()},
			{Key: "toolchains", Value: im.GetToolchains()},
			{Key: "created", Value: screenkit.FmtTime(im.GetCreatedAt())},
		}
		body := im.GetError()
		if body == "" {
			body = ""
		}
		return "Runtime Image: " + im.GetName(), fields, body, nil

	case "secrets":
		// Names only — the value is never fetched or shown.
		return "Secret (value hidden)", []screenkit.Field{
			{Key: "id", Value: id},
			{Key: "note", Value: "values are managed in the GUI; orch never reads them"},
		}, "", nil

	case "mcp":
		resp, err := m.cl.MCP.GetMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerGetRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		s := resp.Msg.GetServer()
		fields := []screenkit.Field{
			{Key: "id", Value: s.GetId()},
			{Key: "name", Value: s.GetName()},
			{Key: "transport", Value: strings.ToLower(s.GetTransport().String())},
			{Key: "command", Value: s.GetCommand()},
			{Key: "url", Value: s.GetUrl()},
			{Key: "enabled", Value: screenkit.FmtBool(s.GetEnabled())},
			{Key: "install status", Value: strings.ToLower(s.GetInstallStatus().String())},
			{Key: "required secrets", Value: strings.Join(s.GetRequiredSecrets(), ", ")},
		}
		return "MCP Server: " + s.GetName(), fields, "", nil
	}
	return "", nil, "", nil
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
	b.WriteString(theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh"))
	return b.String()
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// ActiveSourceName / ActiveItem expose the Base focus state to the
// shell's context engine.
func (m *Model) ActiveSourceName() string           { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (screenkit.Item, bool) { return m.Base.ActiveItem() }
