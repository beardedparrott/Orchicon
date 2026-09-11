// Package control implements the Control screen on the kit2 primitives:
// bordered focus-aware Panels, selectable Tables, a typed Form, entity-bound
// Actions running through the mutation layer, a Confirm dialog, and an
// Activity stream. Secrets are names/metadata only — values are never
// rendered, fetched, or stored.
package control

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// streamRows is the height of the Activity stream panel pinned to the
// bottom of the screen.
const streamRows = 7

// dockSink is the shell hook Control uses to push mutation feedback into the
// always-present chat dock (the App implements it).
type dockSink interface {
	DockError(string)
	DockNotice(string)
}

// Model is the Control screen.
type Model struct {
	kit2.Base
	cl  *client.Clients
	reg *subs.Registry

	stream *kit2.Stream
	bar    *kit2.ActionBar
	form   *kit2.Form

	w, h int

	// pending carries the action awaiting dialog confirmation.
	pending *kit2.Action

	// rpcCreateWebhook is the write thunk for the New Webhook form. It is a
	// field so tests can assert the RPC fires with the field values.
	rpcCreateWebhook func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error
	// rpcDeleteWebhook / rpcUpdateWebhook are the Action write thunks.
	rpcDeleteWebhook func(ctx context.Context, id string) error
	rpcUpdateWebhook func(ctx context.Context, r *apiv1.UpdateSubscriptionRequest) error
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
	m.Base.SetStatuses(nil)

	m.stream = kit2.NewStream("Activity", 78, streamRows-2)
	m.bar = kit2.NewActionBar()
	// Every write goes through the ONE mutation executor.
	m.Exec = &mutate.Executor{Sink: m}

	m.rpcCreateWebhook = func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return fmt.Errorf("no webhook client")
		}
		_, err := m.cl.Webhooks.CreateSubscription(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcDeleteWebhook = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return fmt.Errorf("no webhook client")
		}
		_, err := m.cl.Webhooks.DeleteSubscription(ctx, connect.NewRequest(&apiv1.DeleteSubscriptionRequest{Id: id}))
		return err
	}
	m.rpcUpdateWebhook = func(ctx context.Context, r *apiv1.UpdateSubscriptionRequest) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return fmt.Errorf("no webhook client")
		}
		_, err := m.cl.Webhooks.UpdateSubscription(ctx, connect.NewRequest(r))
		return err
	}
	return m
}

func (m *Model) Name() string { return "control" }

// Close unsubscribes (no live subs on this screen in v1).
func (m *Model) Close() { m.reg.CloseAll() }

// SetSize lays out the panes plus the Activity stream panel.
func (m *Model) SetSize(w, h int) {
	m.w, m.h = w, h
	body := h - streamRows
	if body < 6 {
		body = 6
	}
	m.Base.SetSize(w, body)
	m.stream.SetSize(w-2, streamRows-2)
}

func (m *Model) Init() tea.Cmd { return m.Load() }

// --- mutation sink (dock feedback + activity stream) --------------------

// Progress reports a running mutation in the dock + activity stream.
func (m *Model) Progress(msg string) {
	m.appendActivity("▸ " + msg)
	if d, ok := m.Shell().(dockSink); ok {
		d.DockNotice(msg)
	}
}

// Fail surfaces a mutation failure in the dock + activity stream.
func (m *Model) Fail(msg string) {
	m.appendActivity("✗ " + msg)
	if d, ok := m.Shell().(dockSink); ok {
		d.DockError(msg)
	}
}

// Notice reports a successful mutation.
func (m *Model) Notice(msg string) {
	m.appendActivity("✓ " + msg)
	if d, ok := m.Shell().(dockSink); ok {
		d.DockNotice(msg)
	}
}

func (m *Model) appendActivity(line string) {
	m.stream.Append(line)
}

// --- sources ------------------------------------------------------------

func (m *Model) fetchWorkers(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.Workers))
	for _, w := range resp.Msg.Workers {
		items = append(items, kit2.Item{ID: w.GetId(), Title: w.GetName(), Meta: strings.ToLower(w.GetStatus().String())})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchImages(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.RuntimeImages))
	for _, im := range resp.Msg.RuntimeImages {
		items = append(items, kit2.Item{ID: im.GetId(), Title: im.GetName(), Meta: strings.ToLower(im.GetStatus().String())})
	}
	return items, resp.Msg.NextPageToken, nil
}

// fetchSecrets lists names/metadata ONLY.
func (m *Model) fetchSecrets(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Secrets.ListSecrets(ctx, connect.NewRequest(&apiv1.ListSecretsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.Secrets))
	for _, s := range resp.Msg.Secrets {
		items = append(items, kit2.Item{ID: s.GetId(), Title: s.GetName(), Meta: "value hidden"})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchMCP(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.Servers))
	for _, s := range resp.Msg.Servers {
		meta := "disabled"
		if s.GetEnabled() {
			meta = "enabled"
		}
		items = append(items, kit2.Item{ID: s.GetId(), Title: s.GetName(), Meta: meta})
	}
	return items, "", nil
}

func (m *Model) fetchProviders(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.GetProviders()))
	for _, p := range resp.Msg.GetProviders() {
		meta := "disabled"
		if p.GetEnabled() {
			meta = "enabled"
		}
		items = append(items, kit2.Item{
			ID: p.GetId(), Title: p.GetDisplayName(),
			Meta: strings.ToLower(p.GetKind()) + " " + meta,
		})
	}
	return items, "", nil
}

func (m *Model) fetchWebhooks(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Webhooks.ListSubscriptions(ctx, connect.NewRequest(&apiv1.ListSubscriptionsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.GetSubscriptions()))
	for _, s := range resp.Msg.GetSubscriptions() {
		meta := strings.ToLower(s.GetStatus())
		if s.GetEventFilter() != "" {
			meta += " · " + s.GetEventFilter()
		}
		items = append(items, kit2.Item{ID: s.GetId(), Title: s.GetName(), Meta: meta})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

func (m *Model) fetchSettings(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
	if err != nil {
		return nil, "", err
	}
	if resp.Msg.GetSettings() == nil {
		return nil, "", nil
	}
	return []kit2.Item{{ID: "tenant-settings", Title: "Tenant Settings", Meta: "admin-gated"}}, "", nil
}

// --- detail -------------------------------------------------------------

func (m *Model) detail(ctx context.Context, src, id string) (string, []kit2.Field, string, error) {
	switch src {
	case "workers":
		resp, err := m.cl.Workers.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorker()
		return "Worker: " + w.GetName(), []kit2.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "name", Value: w.GetName()},
			{Key: "slug", Value: w.GetSlug()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "description", Value: w.GetDescription()},
			{Key: "purpose", Value: w.GetPurpose()},
			{Key: "current ver", Value: screenkit.FmtInt(int(w.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}, w.GetDescription(), nil

	case "images":
		resp, err := m.cl.Images.GetRuntimeImage(ctx, connect.NewRequest(&apiv1.GetRuntimeImageRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		im := resp.Msg.GetRuntimeImage()
		return "Runtime Image: " + im.GetName(), []kit2.Field{
			{Key: "id", Value: im.GetId()},
			{Key: "name", Value: im.GetName()},
			{Key: "status", Value: strings.ToLower(im.GetStatus().String())},
			{Key: "base image", Value: im.GetBaseImageRef()},
			{Key: "tag", Value: im.GetTag()},
			{Key: "apt packages", Value: im.GetAptPackages()},
			{Key: "toolchains", Value: im.GetToolchains()},
			{Key: "created", Value: screenkit.FmtTime(im.GetCreatedAt())},
		}, im.GetError(), nil

	case "secrets":
		return "Secret (value hidden)", []kit2.Field{
			{Key: "id", Value: id},
			{Key: "note", Value: "values are managed in the GUI; orch never reads them"},
		}, "", nil

	case "mcp":
		resp, err := m.cl.MCP.GetMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerGetRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		s := resp.Msg.GetServer()
		return "MCP Server: " + s.GetName(), []kit2.Field{
			{Key: "id", Value: s.GetId()},
			{Key: "name", Value: s.GetName()},
			{Key: "transport", Value: strings.ToLower(s.GetTransport().String())},
			{Key: "command", Value: s.GetCommand()},
			{Key: "url", Value: s.GetUrl()},
			{Key: "enabled", Value: screenkit.FmtBool(s.GetEnabled())},
			{Key: "install status", Value: strings.ToLower(s.GetInstallStatus().String())},
			{Key: "required secrets", Value: strings.Join(s.GetRequiredSecrets(), ", ")},
		}, "", nil

	case "providers":
		resp, err := m.cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
		if err != nil {
			return "", nil, "", err
		}
		for _, p := range resp.Msg.GetProviders() {
			if p.GetId() != id {
				continue
			}
			return "Provider: " + p.GetDisplayName(), []kit2.Field{
				{Key: "id", Value: p.GetId()},
				{Key: "name", Value: p.GetDisplayName()},
				{Key: "kind", Value: p.GetKind()},
				{Key: "base url", Value: p.GetBaseUrl()},
				{Key: "enabled", Value: screenkit.FmtBool(p.GetEnabled())},
				{Key: "auth mode", Value: p.GetAuthMode()},
				{Key: "is custom", Value: screenkit.FmtBool(p.GetIsCustom())},
				{Key: "read only", Value: screenkit.FmtBool(p.GetReadOnly())},
				{Key: "token stored", Value: screenkit.FmtBool(p.GetHasTokenStored())},
				{Key: "ctx default", Value: screenkit.FmtInt64(p.GetNumCtxDefault())},
			}, "", nil
		}
		return "Provider", []kit2.Field{{Key: "id", Value: id}}, "", nil

	case "webhooks":
		resp, err := m.cl.Webhooks.ListSubscriptions(ctx, connect.NewRequest(&apiv1.ListSubscriptionsRequest{TenantId: ""}))
		if err != nil {
			return "", nil, "", err
		}
		for _, s := range resp.Msg.GetSubscriptions() {
			if s.GetId() != id {
				continue
			}
			return "Webhook: " + s.GetName(), []kit2.Field{
				{Key: "id", Value: s.GetId()},
				{Key: "name", Value: s.GetName()},
				{Key: "target url", Value: s.GetTargetUrl()},
				{Key: "event filter", Value: s.GetEventFilter()},
				{Key: "scope", Value: s.GetScope()},
				{Key: "status", Value: s.GetStatus()},
				{Key: "max retries", Value: screenkit.FmtInt(int(s.GetMaxRetries()))},
				{Key: "secret hint", Value: s.GetSecretHint()},
			}, "", nil
		}
		return "Webhook", []kit2.Field{{Key: "id", Value: id}}, "", nil

	case "settings":
		resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
		if err != nil {
			return "", nil, "", err
		}
		s := resp.Msg.GetSettings()
		if s == nil {
			return "Settings", []kit2.Field{{Key: "note", Value: "no settings returned"}}, "", nil
		}
		return "Tenant Settings", []kit2.Field{
			{Key: "default worker model", Value: s.GetDefaultWorkerModel()},
			{Key: "default ask model", Value: s.GetDefaultAskOrchiconModel()},
			{Key: "max concurrent runs", Value: screenkit.FmtInt(int(s.GetMaxConcurrentRuns()))},
			{Key: "backup schedule", Value: s.GetBackupSchedule()},
			{Key: "backup retention", Value: screenkit.FmtInt(int(s.GetBackupRetentionDays())) + "d"},
			{Key: "log dir", Value: s.GetLogDirectory()},
			{Key: "session token ttl", Value: screenkit.FmtInt64(s.GetSessionAccessTokenTtlSeconds()) + "s"},
		}, "", nil
	}
	return "", nil, "", nil
}

// --- actions ------------------------------------------------------------

// actionsForSelection builds the entity-bound actions for the currently
// selected row. Every action carries confirm + optimistic apply + rollback
// and runs through the mutation executor.
func (m *Model) actionsForSelection() []kit2.Action {
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case "webhooks":
		id, name := item.ID, item.Title
		return []kit2.Action{
			{
				Label: "disable", Key: "d", Danger: true, Source: "webhooks",
				Confirm:  "Disable " + name + "?\nDelivery to the target URL stops immediately.",
				Apply:    func() { m.setWebhookStatusLocally(id, "disabled") },
				Rollback: func() { m.setWebhookStatusLocally(id, "active") },
				Do: func(ctx context.Context) error {
					status := "disabled"
					return m.rpcUpdateWebhook(ctx, &apiv1.UpdateSubscriptionRequest{Id: id, Status: &status})
				},
			},
			{
				Label: "delete", Key: "x", Danger: true, Source: "webhooks",
				Confirm:  "Delete " + name + "?\nThis cannot be undone.",
				Apply:    func() { m.removeWebhookLocally(id) },
				Rollback: func() { m.Refresh("webhooks") },
				Do: func(ctx context.Context) error {
					return m.rpcDeleteWebhook(ctx, id)
				},
			},
		}
	case "secrets":
		return []kit2.Action{
			{Label: "rotate", Key: "r", Source: "secrets", Confirm: "Rotate " + item.Title + "?", Do: func(ctx context.Context) error { return nil }},
		}
	}
	return nil
}

// setWebhookStatusLocally optimistically rewrites the selected webhook's
// status cell (rollback restores the previous cell).
func (m *Model) setWebhookStatusLocally(id, status string) {
	m.MutateRow("webhooks", id, func(r *kit2.Row) { r.Meta = status })
}

func (m *Model) removeWebhookLocally(id string) {
	m.RemoveRow("webhooks", id)
}

// openActionsDialog opens the Confirm dialog for an action that needs
// confirmation, or runs it immediately when it does not.
func (m *Model) openActionsDialog(a kit2.Action) tea.Cmd {
	if a.NeedsConfirm() {
		d := kit2.Confirm(a.Label, a.Confirm, a.Label)
		d.Danger = a.Danger
		m.Open = d
		pending := a
		m.pending = &pending
		m.OnDialog = func(choice string) tea.Cmd {
			pa := m.pending
			m.pending = nil
			m.OnDialog = nil
			if pa == nil || choice == "" {
				return nil // dismissed
			}
			return m.runAction(*pa)
		}
		return nil
	}
	return m.runAction(a)
}

// runAction hands the action to the mutation executor.
func (m *Model) runAction(a kit2.Action) tea.Cmd {
	return m.Mutate(mutate.Request{
		Name: a.Label, Source: a.Source,
		Apply: a.Apply, Rollback: a.Rollback, Do: a.Do,
	})
}

// --- form ---------------------------------------------------------------

// newWebhookForm builds the typed create form. Submission goes through the
// mutation layer (never a direct RPC).
func (m *Model) newWebhookForm() *kit2.Form {
	f := kit2.NewForm("New webhook subscription",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "build-finished"},
		kit2.FieldSpec{Name: "target_url", Label: "Target URL", Kind: kit2.KText, Required: true, Placeholder: "https://…"},
		kit2.FieldSpec{Name: "event_filter", Label: "Event filter", Kind: kit2.KText, Placeholder: "execution.completed"},
		kit2.FieldSpec{Name: "scope", Label: "Scope", Kind: kit2.KSelect, Options: []kit2.Option{
			{Value: "tenant", Label: "tenant"}, {Value: "workflow", Label: "workflow"}, {Value: "execution", Label: "execution"},
		}},
		kit2.FieldSpec{Name: "secret", Label: "Signing secret", Kind: kit2.KSecret},
		kit2.FieldSpec{Name: "max_retries", Label: "Max retries", Kind: kit2.KNumber, Initial: "3"},
	)
	f.Focused = true
	f.Width = 60
	f.OnSubmit = func(v map[string]string, multi map[string][]string) (tea.Cmd, error) {
		maxRetries := int32(3)
		if n := strings.TrimSpace(v["max_retries"]); n != "" {
			var parsed int
			if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil {
				maxRetries = int32(parsed)
			}
		}
		req := &apiv1.CreateSubscriptionRequest{
			Name:        v["name"],
			TargetUrl:   v["target_url"],
			EventFilter: v["event_filter"],
			Scope:       v["scope"],
			Secret:      v["secret"],
			MaxRetries:  maxRetries,
		}
		return m.Mutate(mutate.Request{
			Name: "create webhook " + v["name"], Source: "webhooks",
			Do: func(ctx context.Context) error { return m.rpcCreateWebhook(ctx, req) },
		}), nil
	}
	return f
}

func (m *Model) formOpen() bool { return m.form != nil }

// --- update -------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	case mutate.Result:
		return m, m.HandleMutation(msg)
	}

	// The open form owns every key (modal, layered over the focus ring).
	if m.formOpen() {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.String() == "esc" {
				m.form = nil
				return m, nil
			}
			cmd, _ := m.form.HandleKey(k)
			if m.form.Submitted {
				m.form = nil
			}
			return m, cmd
		}
	}

	if k, ok := msg.(tea.KeyMsg); ok && m.Open == nil && !m.formOpen() {
		switch k.String() {
		case "n":
			// New entity: the webhooks pane opens the typed create form.
			if m.ActiveSourceName() == "webhooks" {
				m.form = m.newWebhookForm()
				return m, nil
			}
		case "x":
			actions := m.actionsForSelection()
			if len(actions) > 0 {
				return m, m.openActionsDialog(actions[0])
			}
		case "D":
			actions := m.actionsForSelection()
			if len(actions) > 1 {
				return m, m.openActionsDialog(actions[1])
			}
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

// --- view ---------------------------------------------------------------

func (m *Model) View() string {
	m.refreshActionBar()

	body := m.Base.View()
	sp := kit2.NewPanel("Activity", m.w, streamRows)
	sp.SetContent(m.stream.View())
	sp.Focused = false
	out := body + "\n" + sp.View()

	if m.formOpen() {
		box := formBox(m.form, m.w)
		out = kit2.Center(kit2.FitLines(out, m.w, m.h), box, m.w, m.h)
	} else if m.Open != nil {
		box := m.Open.Box(min(60, m.w-4), 9)
		out = kit2.Center(kit2.FitLines(out, m.w, m.h), box, m.w, m.h)
	}
	if m.w > 0 && m.h > 0 {
		return kit2.FitLines(out, m.w, m.h)
	}
	return out
}

// formBox wraps the form in a titled dialog-sized box.
func formBox(f *kit2.Form, w int) string {
	bw := min(66, w-4)
	if bw < 20 {
		bw = 20
	}
	d := &kit2.Dialog{Title: f.Title, Body: f.View(), Buttons: []string{"submit", "cancel"}}
	return d.Box(bw, min(16, len(f.Specs)*2+4))
}

func (m *Model) refreshActionBar() {
	actions := m.actionsForSelection()
	m.bar.Actions = actions
	if m.bar.Sel >= len(actions) {
		m.bar.Sel = 0
	}
}

// Hint returns the screen's key cheat-sheet (also rendered by the shell).
func (m *Model) HintLine() string {
	return theme.HintText.Render("enter: detail focus · ←/→: pane · n: new · x: action · f: more pages · r: refresh")
}

// --- shell hooks --------------------------------------------------------

func (m *Model) SelectSource(name string) bool        { return m.Base.SelectSource(name) }
func (m *Model) SelectItem(src, id string) bool       { return m.Base.SelectItem(src, id) }
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
