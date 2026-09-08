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
	m.AddSource("providers", "Providers", m.fetchProviders)
	m.AddSource("webhooks", "Webhooks", m.fetchWebhooks)
	m.AddSource("settings", "Settings", m.fetchSettings)
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

func (m *Model) fetchProviders(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.GetProviders()))
	for _, p := range resp.Msg.GetProviders() {
		meta := "disabled"
		if p.GetEnabled() {
			meta = "enabled"
		}
		if p.GetIsCustom() {
			meta += " · custom"
		}
		items = append(items, screenkit.Item{
			ID:    p.GetId(),
			Title: p.GetDisplayName(),
			Meta:  strings.ToLower(p.GetKind()) + " " + meta,
		})
	}
	return items, "", nil
}

func (m *Model) fetchWebhooks(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Webhooks.ListSubscriptions(ctx, connect.NewRequest(&apiv1.ListSubscriptionsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.GetSubscriptions()))
	for _, s := range resp.Msg.GetSubscriptions() {
		meta := strings.ToLower(s.GetStatus())
		if s.GetEventFilter() != "" {
			meta += " · " + s.GetEventFilter()
		}
		items = append(items, screenkit.Item{
			ID:    s.GetId(),
			Title: s.GetName(),
			Meta:  meta,
		})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

func (m *Model) fetchSettings(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
	if err != nil {
		return nil, "", err
	}
	s := resp.Msg.GetSettings()
	if s == nil {
		return nil, "", nil
	}
	items := []screenkit.Item{
		{ID: "tenant-settings", Title: "Tenant Settings", Meta: "admin-gated"},
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

	case "providers":
		resp, err := m.cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
		if err != nil {
			return "", nil, "", err
		}
		for _, p := range resp.Msg.GetProviders() {
			if p.GetId() != id {
				continue
			}
			fields := []screenkit.Field{
				{Key: "id", Value: p.GetId()},
				{Key: "name", Value: p.GetDisplayName()},
				{Key: "kind", Value: p.GetKind()},
				{Key: "base url", Value: p.GetBaseUrl()},
				{Key: "base url override", Value: p.GetBaseUrlOverride()},
				{Key: "enabled", Value: screenkit.FmtBool(p.GetEnabled())},
				{Key: "auth mode", Value: p.GetAuthMode()},
				{Key: "is custom", Value: screenkit.FmtBool(p.GetIsCustom())},
				{Key: "read only", Value: screenkit.FmtBool(p.GetReadOnly())},
				{Key: "token stored", Value: screenkit.FmtBool(p.GetHasTokenStored())},
				{Key: "ctx default", Value: screenkit.FmtInt64(p.GetNumCtxDefault())},
			}
			return "Provider: " + p.GetDisplayName(), fields, "", nil
		}
		return "Provider", []screenkit.Field{{Key: "id", Value: id}}, "", nil

	case "webhooks":
		resp, err := m.cl.Webhooks.ListSubscriptions(ctx, connect.NewRequest(&apiv1.ListSubscriptionsRequest{TenantId: ""}))
		if err != nil {
			return "", nil, "", err
		}
		for _, s := range resp.Msg.GetSubscriptions() {
			if s.GetId() != id {
				continue
			}
			fields := []screenkit.Field{
				{Key: "id", Value: s.GetId()},
				{Key: "name", Value: s.GetName()},
				{Key: "target url", Value: s.GetTargetUrl()},
				{Key: "event filter", Value: s.GetEventFilter()},
				{Key: "scope", Value: s.GetScope()},
				{Key: "status", Value: s.GetStatus()},
				{Key: "max retries", Value: screenkit.FmtInt(int(s.GetMaxRetries()))},
				{Key: "secret hint", Value: s.GetSecretHint()},
			}
			return "Webhook: " + s.GetName(), fields, "", nil
		}
		return "Webhook", []screenkit.Field{{Key: "id", Value: id}}, "", nil

	case "settings":
		resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
		if err != nil {
			return "", nil, "", err
		}
		s := resp.Msg.GetSettings()
		if s == nil {
			return "Settings", []screenkit.Field{{Key: "note", Value: "no settings returned"}}, "", nil
		}
		fields := []screenkit.Field{
			{Key: "default worker model", Value: s.GetDefaultWorkerModel()},
			{Key: "default ask model", Value: s.GetDefaultAskOrchiconModel()},
			{Key: "max concurrent runs", Value: screenkit.FmtInt(int(s.GetMaxConcurrentRuns()))},
			{Key: "stall no-progress window", Value: screenkit.FmtInt64(s.GetStallNoProgressWindowSeconds()) + "s"},
			{Key: "stall no-diff window", Value: screenkit.FmtInt64(s.GetStallNoFileDiffWindowSeconds()) + "s"},
			{Key: "stall text-loop window", Value: screenkit.FmtInt64(s.GetStallTextLoopWindowSeconds()) + "s"},
			{Key: "stall repetition count", Value: screenkit.FmtInt(int(s.GetStallRepetitionCount()))},
			{Key: "stall repetition window", Value: screenkit.FmtInt64(s.GetStallRepetitionWindowSeconds()) + "s"},
			{Key: "stall nudge max", Value: screenkit.FmtInt(int(s.GetStallNudgeMax()))},
			{Key: "stall tool-hang", Value: screenkit.FmtInt64(s.GetStallToolHangSeconds()) + "s"},
			{Key: "exec reap grace", Value: screenkit.FmtInt64(s.GetExecutionReapGraceSeconds()) + "s"},
			{Key: "exec reap failures", Value: screenkit.FmtInt(int(s.GetExecutionReapConsecutiveFailures()))},
			{Key: "backup schedule", Value: s.GetBackupSchedule()},
			{Key: "backup retention", Value: screenkit.FmtInt(int(s.GetBackupRetentionDays())) + "d"},
			{Key: "backup dir", Value: s.GetBackupDirectory()},
			{Key: "log dir", Value: s.GetLogDirectory()},
			{Key: "log max size", Value: screenkit.FmtInt64(s.GetLogMaxSizeMb()) + "MB"},
			{Key: "log retention", Value: screenkit.FmtInt(int(s.GetLogRetentionDays())) + "d"},
			{Key: "session token ttl", Value: screenkit.FmtInt64(s.GetSessionAccessTokenTtlSeconds()) + "s"},
		}
		return "Tenant Settings", fields, "", nil
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
