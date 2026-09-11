// Package control implements the Control screen on the kit2 primitives:
// bordered focus-aware Panels, selectable Tables, typed Forms, entity-bound
// Actions running through the mutation layer, a Confirm dialog, and an
// Activity stream.
//
// It carries the GUI Control surface read-WRITE:
//
//	secrets    list (names/metadata ONLY) · create · rotate value · delete
//	mcp        create · edit · enable/disable · set/clear credential · install
//	providers  create custom · edit · enable/disable · set/clear token · delete
//	webhooks   create · edit · enable/disable · test · delete · deliveries view
//	adapters   list + detail (capabilities) + local enable/disable toggle
//	settings   view all + edit/save (model refs validated before submit)
//	admin      admin-gated surface with an EXPLICIT permission state
//
// Secrets are names/metadata only — a secret VALUE is never fetched,
// rendered, or stored by the TUI (`GetSecret` is never called; the value is
// written once into the create/update request and then forgotten).
//
// Every write is Confirm- or form-gated, applies optimistically, rolls back
// on failure, and runs through the ONE mutation executor (dock feedback +
// source reconcile) — no screen code calls a write RPC directly.
package control

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// adminSurface is one admin-gated surface advertised on the Admin pane.
type adminSurface struct {
	id    string
	title string
	meta  string
}

// adminSurfaces is the admin inventory the TUI advertises. Every row is
// admin-gated on the plane; the pane's FIRST row carries the live permission
// state so a credential without the admin scope sees an explicit "permission
// required" state instead of a silent empty pane.
var adminSurfaces = []adminSurface{
	{"admin-identities", "Identities", "auth · admin"},
	{"admin-roles", "Roles & bindings", "auth · admin"},
	{"admin-tenants", "Tenants", "auth · admin"},
	{"admin-keys", "API keys", "auth · admin"},
	{"admin-audit", "Audit log", "auth · admin"},
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

	// list-response caches: the list RPCs carry the rich rows and v1 has no
	// Get-RPC for providers / webhooks / deliveries, so the list hit is the
	// detail source of truth (same pattern as the enforcement screen).
	webhooks   map[string]*apiv1.WebhookSubscription
	providers  map[string]*apiv1.ProviderEntry
	mcpServers map[string]*apiv1.MCPServer
	adapters   map[string]*apiv1.RuntimeAdapter
	settings   *apiv1.TenantSettings

	// adapterDisabled is the LOCAL dispatch toggle for a registered adapter.
	// The public RuntimeAdapterService is read-only (ListAdapters +
	// GetAdapterCapabilities); adapters register themselves over the sidecar
	// gRPC contract, so the TUI's enable/disable is a client-side dispatch
	// filter, not a plane write.
	adapterDisabled map[string]bool

	// rpc* are the write/read thunks. They are fields so tests can assert the
	// exact RPC payload without a live plane.
	rpcAdminProbe         func(ctx context.Context) error
	rpcListAdapters       func(ctx context.Context, kind string) ([]*apiv1.RuntimeAdapter, error)
	rpcListDeliveries     func(ctx context.Context, subscriptionID string) ([]*apiv1.WebhookDelivery, error)
	rpcTestWebhook        func(ctx context.Context, id string) error
	rpcCreateWebhook      func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error
	rpcUpdateWebhook      func(ctx context.Context, r *apiv1.UpdateSubscriptionRequest) error
	rpcDeleteWebhook      func(ctx context.Context, id string) error
	rpcUpdateSettings     func(ctx context.Context, s *apiv1.TenantSettings) error
	rpcCreateMCP          func(ctx context.Context, r *apiv1.MCPServerCreateRequest) error
	rpcUpdateMCP          func(ctx context.Context, r *apiv1.MCPServerUpdateRequest) error
	rpcDeleteMCP          func(ctx context.Context, id string) error
	rpcInstallMCP         func(ctx context.Context, id string) error
	rpcSetMCPSecret       func(ctx context.Context, id, name, value string) error
	rpcClearMCPSecret     func(ctx context.Context, id, name string) error
	rpcCreateProvider     func(ctx context.Context, r *apiv1.ProviderCreateCustomRequest) error
	rpcUpdateProvider     func(ctx context.Context, r *apiv1.ProviderUpdateCustomRequest) error
	rpcDeleteProvider     func(ctx context.Context, id string) error
	rpcSetProviderToken   func(ctx context.Context, id, token string) error
	rpcClearProviderToken func(ctx context.Context, id string) error
	rpcProviderEnabled    func(ctx context.Context, id string, enabled bool) error
	rpcProviderSettings   func(ctx context.Context, id string, enabled bool, baseURLOverride string) error
	rpcCreateSecret       func(ctx context.Context, r *apiv1.CreateSecretRequest) error
	rpcUpdateSecret       func(ctx context.Context, r *apiv1.UpdateSecretRequest) error
	rpcDeleteSecret       func(ctx context.Context, id string) error
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry) *Model {
	m := &Model{
		cl:              cl,
		reg:             reg,
		webhooks:        map[string]*apiv1.WebhookSubscription{},
		providers:       map[string]*apiv1.ProviderEntry{},
		mcpServers:      map[string]*apiv1.MCPServer{},
		adapters:        map[string]*apiv1.RuntimeAdapter{},
		adapterDisabled: map[string]bool{},
	}
	m.NameStr = "control"
	m.AddSource("workers", "Workers", m.fetchWorkers)
	m.AddSource("images", "Runtime Images", m.fetchImages)
	m.AddSource("secrets", "Secrets (names only)", m.fetchSecrets)
	m.AddSource("mcp", "MCP Servers", m.fetchMCP)
	m.AddSource("providers", "Providers", m.fetchProviders)
	m.AddSource("webhooks", "Webhooks", m.fetchWebhooks)
	m.AddSource("adapters", "Adapters", m.fetchAdapters)
	m.AddSource("settings", "Settings", m.fetchSettings)
	m.AddSource("admin", "Admin", m.fetchAdmin)
	m.SetDetail(m.detail)
	m.Base.SetStatuses(nil)

	m.stream = kit2.NewStream("Activity", 78, streamRows-2)
	m.bar = kit2.NewActionBar()
	// Every write goes through the ONE mutation executor.
	m.SetExecutor(&mutate.Executor{Sink: m})

	m.rpcAdminProbe = func(ctx context.Context) error {
		if m.cl == nil || m.cl.Auth == nil {
			return errNoClient("auth")
		}
		_, err := m.cl.Auth.ListIdentities(ctx, connect.NewRequest(&apiv1.ListIdentitiesRequest{PageSize: 1}))
		return err
	}
	m.rpcListAdapters = func(ctx context.Context, kind string) ([]*apiv1.RuntimeAdapter, error) {
		if m.cl == nil || m.cl.Adapters == nil {
			return nil, errNoClient("adapter")
		}
		req := &apiv1.ListAdaptersRequest{PageSize: 100}
		if kind != "" {
			req.Kind = &kind
		}
		resp, err := m.cl.Adapters.ListAdapters(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetAdapters(), nil
	}
	m.rpcListDeliveries = func(ctx context.Context, subscriptionID string) ([]*apiv1.WebhookDelivery, error) {
		if m.cl == nil || m.cl.Webhooks == nil {
			return nil, errNoClient("webhook")
		}
		resp, err := m.cl.Webhooks.ListDeliveries(ctx, connect.NewRequest(&apiv1.ListDeliveriesRequest{
			SubscriptionId: subscriptionID,
			PageSize:       20,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetDeliveries(), nil
	}
	m.rpcTestWebhook = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return errNoClient("webhook")
		}
		_, err := m.cl.Webhooks.TestSubscription(ctx, connect.NewRequest(&apiv1.TestSubscriptionRequest{Id: id}))
		return err
	}
	m.rpcCreateWebhook = func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return errNoClient("webhook")
		}
		_, err := m.cl.Webhooks.CreateSubscription(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcUpdateWebhook = func(ctx context.Context, r *apiv1.UpdateSubscriptionRequest) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return errNoClient("webhook")
		}
		_, err := m.cl.Webhooks.UpdateSubscription(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcDeleteWebhook = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Webhooks == nil {
			return errNoClient("webhook")
		}
		_, err := m.cl.Webhooks.DeleteSubscription(ctx, connect.NewRequest(&apiv1.DeleteSubscriptionRequest{Id: id}))
		return err
	}
	m.rpcUpdateSettings = func(ctx context.Context, s *apiv1.TenantSettings) error {
		if m.cl == nil || m.cl.Settings == nil {
			return errNoClient("settings")
		}
		_, err := m.cl.Settings.UpdateSettings(ctx, connect.NewRequest(&apiv1.UpdateSettingsRequest{Settings: s}))
		return err
	}
	m.rpcCreateMCP = func(ctx context.Context, r *apiv1.MCPServerCreateRequest) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.CreateMCPServer(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcUpdateMCP = func(ctx context.Context, r *apiv1.MCPServerUpdateRequest) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.UpdateMCPServer(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcDeleteMCP = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.DeleteMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerDeleteRequest{Id: id}))
		return err
	}
	m.rpcInstallMCP = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.InstallMCPRuntime(ctx, connect.NewRequest(&apiv1.MCPServerInstallRequest{Id: id}))
		return err
	}
	m.rpcSetMCPSecret = func(ctx context.Context, id, name, value string) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.SetMCPServerSecret(ctx, connect.NewRequest(&apiv1.MCPServerSetSecretRequest{Id: id, Name: name, Value: value}))
		return err
	}
	m.rpcClearMCPSecret = func(ctx context.Context, id, name string) error {
		if m.cl == nil || m.cl.MCP == nil {
			return errNoClient("mcp")
		}
		_, err := m.cl.MCP.ClearMCPServerSecret(ctx, connect.NewRequest(&apiv1.MCPServerClearSecretRequest{Id: id, Name: name}))
		return err
	}
	m.rpcCreateProvider = func(ctx context.Context, r *apiv1.ProviderCreateCustomRequest) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.CreateCustomProvider(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcUpdateProvider = func(ctx context.Context, r *apiv1.ProviderUpdateCustomRequest) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.UpdateCustomProvider(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcDeleteProvider = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.DeleteCustomProvider(ctx, connect.NewRequest(&apiv1.ProviderDeleteCustomRequest{RefId: id}))
		return err
	}
	m.rpcSetProviderToken = func(ctx context.Context, id, token string) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.SetProviderToken(ctx, connect.NewRequest(&apiv1.ProviderSetTokenRequest{ProviderId: id, Token: token}))
		return err
	}
	m.rpcClearProviderToken = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.ClearProviderToken(ctx, connect.NewRequest(&apiv1.ProviderClearTokenRequest{ProviderId: id}))
		return err
	}
	m.rpcProviderEnabled = func(ctx context.Context, id string, enabled bool) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		_, err := m.cl.Providers.UpdateProviderSettings(ctx, connect.NewRequest(&apiv1.ProviderUpdateSettingsRequest{
			ProviderId: id,
			Enabled:    &enabled,
		}))
		return err
	}
	m.rpcProviderSettings = func(ctx context.Context, id string, enabled bool, baseURLOverride string) error {
		if m.cl == nil || m.cl.Providers == nil {
			return errNoClient("provider")
		}
		req := &apiv1.ProviderUpdateSettingsRequest{ProviderId: id, Enabled: &enabled}
		if baseURLOverride != "" {
			req.BaseUrlOverride = &baseURLOverride
		}
		_, err := m.cl.Providers.UpdateProviderSettings(ctx, connect.NewRequest(req))
		return err
	}
	m.rpcCreateSecret = func(ctx context.Context, r *apiv1.CreateSecretRequest) error {
		if m.cl == nil || m.cl.Secrets == nil {
			return errNoClient("secret")
		}
		_, err := m.cl.Secrets.CreateSecret(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcUpdateSecret = func(ctx context.Context, r *apiv1.UpdateSecretRequest) error {
		if m.cl == nil || m.cl.Secrets == nil {
			return errNoClient("secret")
		}
		_, err := m.cl.Secrets.UpdateSecret(ctx, connect.NewRequest(r))
		return err
	}
	m.rpcDeleteSecret = func(ctx context.Context, id string) error {
		if m.cl == nil || m.cl.Secrets == nil {
			return errNoClient("secret")
		}
		_, err := m.cl.Secrets.DeleteSecret(ctx, connect.NewRequest(&apiv1.DeleteSecretRequest{Id: id}))
		return err
	}
	return m
}

func errNoClient(what string) error { return fmt.Errorf("no %s client", what) }

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

// bodyHeight is the height of the pane row (viewport minus the Activity panel).
func (m *Model) bodyHeight() int {
	body := m.h - streamRows
	if body < 6 {
		body = 6
	}
	return body
}

// ClaimsKeys reports whether the screen owns the keyboard: while a form or a
// Confirm dialog is open the shell hands over every key verbatim, so a secret
// value or a URL containing q / d / / is never eaten by a global chord.
func (m *Model) ClaimsKeys() bool { return m.form != nil || m.Open != nil }

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

func (m *Model) appendActivity(line string) { m.stream.Append(line) }

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

// fetchSecrets lists names/metadata ONLY. Secret values are never fetched,
// rendered, or stored by orch (GetSecret is not called).
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
		m.mcpServers[s.GetId()] = s
		meta := "disabled"
		if s.GetEnabled() {
			meta = "enabled"
		}
		if s.GetHasSecretStored() {
			meta += " · cred"
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
		m.providers[p.GetId()] = p
		meta := "disabled"
		if p.GetEnabled() {
			meta = "enabled"
		}
		if p.GetIsCustom() {
			meta += " · custom"
		}
		if p.GetHasTokenStored() {
			meta += " · token"
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
		m.webhooks[s.GetId()] = s
		meta := strings.ToLower(s.GetStatus())
		if meta == "" {
			meta = "active"
		}
		if s.GetEventFilter() != "" {
			meta += " · " + s.GetEventFilter()
		}
		items = append(items, kit2.Item{ID: s.GetId(), Title: s.GetName(), Meta: meta})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

func (m *Model) fetchAdapters(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	adapters, err := m.rpcListAdapters(ctx, "")
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(adapters))
	for _, a := range adapters {
		m.adapters[a.GetId()] = a
		meta := a.GetKind() + " v" + a.GetVersion() + " · " + a.GetStatus()
		if m.adapterDisabled[a.GetId()] {
			meta += " · disabled (local)"
		}
		items = append(items, kit2.Item{ID: a.GetId(), Title: a.GetKind(), Meta: meta})
	}
	return items, "", nil
}

func (m *Model) fetchSettings(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
	if err != nil {
		return nil, "", err
	}
	s := resp.Msg.GetSettings()
	if s == nil {
		return nil, "", nil
	}
	m.settings = s
	return []kit2.Item{{ID: "tenant-settings", Title: "Tenant Settings", Meta: "e: edit & save"}}, "", nil
}

// fetchAdmin probes an admin-gated read and reports the resulting permission
// state as the pane's FIRST row. A refusal is rendered EXPLICITLY (never a
// silent empty pane); the pane itself carries no error state.
func (m *Model) fetchAdmin(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	// short is the row title (visible even in a narrow pane); full is the
	// Meta + detail state.
	short, full := "granted", "granted — this credential holds the admin scope"
	if m.rpcAdminProbe == nil {
		short, full = "unavailable", "unavailable — no auth client"
	} else if err := m.rpcAdminProbe(ctx); err != nil {
		if isPermissionErr(err) {
			short = "permission required"
			full = "permission required — this credential is not a tenant admin"
		} else {
			short, full = "unavailable", "unavailable — "+err.Error()
		}
	}
	items := []kit2.Item{{ID: "admin-access", Title: short, Meta: full}}
	for _, s := range adminSurfaces {
		items = append(items, kit2.Item{ID: s.id, Title: s.title, Meta: s.meta})
	}
	return items, "", nil
}

// isPermissionErr reports whether an RPC error is an authorization refusal.
func isPermissionErr(err error) bool {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		switch cerr.Code() {
		case connect.CodePermissionDenied, connect.CodeUnauthenticated:
			return true
		}
	}
	l := strings.ToLower(err.Error())
	return strings.Contains(l, "permission_denied") || strings.Contains(l, "permission denied") ||
		strings.Contains(l, "forbidden") || strings.Contains(l, "unauthenticated") ||
		strings.Contains(l, "unauthorized")
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
		// Names only — the value is never fetched or shown.
		return "Secret (value hidden)", []kit2.Field{
			{Key: "id", Value: id},
			{Key: "note", Value: "orch never reads a secret value — write it on create, rotate it blind"},
			{Key: "actions", Value: "e: rotate value/description · x: delete"},
		}, "", nil

	case "mcp":
		s := m.mcpServers[id]
		if s == nil {
			resp, err := m.cl.MCP.GetMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerGetRequest{Id: id}))
			if err != nil {
				return "", nil, "", err
			}
			s = resp.Msg.GetServer()
		}
		return "MCP Server: " + s.GetName(), []kit2.Field{
			{Key: "id", Value: s.GetId()},
			{Key: "name", Value: s.GetName()},
			{Key: "transport", Value: strings.ToLower(s.GetTransport().String())},
			{Key: "command", Value: s.GetCommand()},
			{Key: "args", Value: strings.Join(s.GetArgs(), " ")},
			{Key: "url", Value: s.GetUrl()},
			{Key: "enabled", Value: screenkit.FmtBool(s.GetEnabled())},
			{Key: "install status", Value: strings.ToLower(s.GetInstallStatus().String())},
			{Key: "required secrets", Value: strings.Join(s.GetRequiredSecrets(), ", ")},
			{Key: "credential stored", Value: screenkit.FmtBool(s.GetHasSecretStored())},
			{Key: "catalog", Value: s.GetCatalogSlug()},
			{Key: "actions", Value: "e: edit · t: toggle · s: set credential · c: clear credential · i: install · x: delete"},
		}, "", nil

	case "providers":
		p := m.providers[id]
		if p == nil {
			resp, err := m.cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
			if err != nil {
				return "", nil, "", err
			}
			for _, cand := range resp.Msg.GetProviders() {
				if cand.GetId() == id {
					p = cand
					break
				}
			}
		}
		if p == nil {
			return "Provider", []kit2.Field{{Key: "id", Value: id}}, "", nil
		}
		return "Provider: " + p.GetDisplayName(), []kit2.Field{
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
			{Key: "token secret", Value: p.GetTokenSecretName()},
			{Key: "ctx default", Value: screenkit.FmtInt64(p.GetNumCtxDefault())},
			{Key: "actions", Value: "e: edit · t: toggle · s: set token · c: clear token · x: delete (custom)"},
		}, "", nil

	case "webhooks":
		s := m.webhooks[id]
		if s == nil {
			resp, err := m.cl.Webhooks.ListSubscriptions(ctx, connect.NewRequest(&apiv1.ListSubscriptionsRequest{TenantId: "", PageSize: 100}))
			if err != nil {
				return "", nil, "", err
			}
			for _, cand := range resp.Msg.GetSubscriptions() {
				if cand.GetId() == id {
					s = cand
					break
				}
			}
		}
		if s == nil {
			return "Webhook", []kit2.Field{{Key: "id", Value: id}}, "", nil
		}
		fields := []kit2.Field{
			{Key: "id", Value: s.GetId()},
			{Key: "name", Value: s.GetName()},
			{Key: "target url", Value: s.GetTargetUrl()},
			{Key: "event filter", Value: s.GetEventFilter()},
			{Key: "scope", Value: s.GetScope()},
			{Key: "status", Value: s.GetStatus()},
			{Key: "max retries", Value: screenkit.FmtInt(int(s.GetMaxRetries()))},
			{Key: "secret hint", Value: s.GetSecretHint()},
			{Key: "actions", Value: "e: edit · t: toggle · T: test · x: delete"},
		}
		// Deliveries (the /webhooks GUI tab): the recent delivery log for
		// this subscription, appended to the detail body.
		var body strings.Builder
		if m.rpcListDeliveries != nil {
			if dels, err := m.rpcListDeliveries(ctx, id); err == nil && len(dels) > 0 {
				body.WriteString("deliveries (newest first):\n")
				for _, d := range dels {
					fmt.Fprintf(&body, "  %s  %s  attempt %d  http %d  %s\n",
						d.GetId(), d.GetStatus(), d.GetAttempt(), d.GetStatusCode(), d.GetEventType())
					if d.GetError() != "" {
						body.WriteString("    error: " + d.GetError() + "\n")
					}
				}
			} else if err != nil {
				body.WriteString("deliveries unavailable: " + err.Error() + "\n")
			} else {
				body.WriteString("deliveries: none recorded yet\n")
			}
		}
		return "Webhook: " + s.GetName(), fields, strings.TrimRight(body.String(), "\n"), nil

	case "adapters":
		a := m.adapters[id]
		if a == nil {
			adapters, err := m.rpcListAdapters(ctx, "")
			if err != nil {
				return "", nil, "", err
			}
			for _, cand := range adapters {
				if cand.GetId() == id {
					a = cand
					break
				}
			}
		}
		if a == nil {
			return "Adapter", []kit2.Field{{Key: "id", Value: id}}, "", nil
		}
		state := "enabled"
		if m.adapterDisabled[id] {
			state = "disabled (local dispatch)"
		}
		body := a.GetCapabilities()
		if body == "{}" {
			body = ""
		}
		return "Adapter: " + a.GetKind(), []kit2.Field{
			{Key: "id", Value: a.GetId()},
			{Key: "kind", Value: a.GetKind()},
			{Key: "version", Value: a.GetVersion()},
			{Key: "endpoint", Value: a.GetEndpoint()},
			{Key: "status", Value: a.GetStatus()},
			{Key: "dispatch", Value: state},
			{Key: "actions", Value: "t: enable/disable (local dispatch filter)"},
		}, body, nil

	case "settings":
		s := m.settings
		if s == nil {
			resp, err := m.cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
			if err != nil {
				return "", nil, "", err
			}
			s = resp.Msg.GetSettings()
		}
		if s == nil {
			return "Settings", []kit2.Field{{Key: "note", Value: "no settings returned"}}, "", nil
		}
		return "Tenant Settings", []kit2.Field{
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
			{Key: "default budget overrides", Value: s.GetDefaultBudgetOverrides()},
			{Key: "backup schedule", Value: s.GetBackupSchedule()},
			{Key: "backup retention", Value: screenkit.FmtInt(int(s.GetBackupRetentionDays())) + "d"},
			{Key: "backup dir", Value: s.GetBackupDirectory()},
			{Key: "log dir", Value: s.GetLogDirectory()},
			{Key: "log max size", Value: screenkit.FmtInt64(s.GetLogMaxSizeMb()) + "MB"},
			{Key: "log retention", Value: screenkit.FmtInt(int(s.GetLogRetentionDays())) + "d"},
			{Key: "session token ttl", Value: screenkit.FmtInt64(s.GetSessionAccessTokenTtlSeconds()) + "s"},
			{Key: "session refresh ttl", Value: screenkit.FmtInt64(s.GetSessionRefreshTokenTtlSeconds()) + "s"},
			{Key: "actions", Value: "e: edit & save"},
		}, "", nil

	case "admin":
		if id == "admin-access" {
			return "Admin access", []kit2.Field{
				{Key: "state", Value: m.adminStateString()},
				{Key: "note", Value: "admin-gated surfaces require a tenant-admin credential (or an API key holding the admin scope)"},
			}, "", nil
		}
		for _, s := range adminSurfaces {
			if s.id == id {
				return "Admin: " + s.title, []kit2.Field{
					{Key: "surface", Value: s.title},
					{Key: "gate", Value: s.meta},
					{Key: "access", Value: m.adminStateString()},
				}, "", nil
			}
		}
		return "Admin", []kit2.Field{{Key: "id", Value: id}}, "", nil
	}
	return "", nil, "", nil
}

// adminStateString is the Admin pane's permission state ("granted" until a
// probe says otherwise).
func (m *Model) adminStateString() string {
	for _, r := range m.adminRowsForTest() {
		if r.ID == "admin-access" {
			return r.Meta
		}
	}
	return "unknown"
}

func (m *Model) adminRowsForTest() []kit2.Item {
	items, _, _ := m.fetchAdmin(context.Background(), "")
	return items
}

// --- actions ------------------------------------------------------------

// actionsForSelection builds the entity-bound RPC/toggle actions for the
// currently selected row. Every action carries confirm + optimistic apply +
// rollback and runs through the mutation executor.
func (m *Model) actionsForSelection() []kit2.Action {
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case "webhooks":
		id, name := item.ID, item.Title
		enabled := true
		if s := m.webhooks[id]; s != nil {
			enabled = !strings.EqualFold(s.GetStatus(), "paused") && !strings.EqualFold(s.GetStatus(), "disabled")
		}
		acts := []kit2.Action{}
		if enabled {
			acts = append(acts, kit2.Action{
				Label: "disable", Key: "t", Danger: true, Source: "webhooks",
				Confirm:  "Disable " + name + "?\nDelivery to the target URL stops immediately.",
				Apply:    func() { m.setWebhookStatus(id, "paused") },
				Rollback: func() { m.setWebhookStatus(id, "active") },
				Do: func(ctx context.Context) error {
					paused := "paused"
					return m.rpcUpdateWebhook(ctx, &apiv1.UpdateSubscriptionRequest{Id: id, Status: &paused})
				},
			})
		} else {
			acts = append(acts, kit2.Action{
				Label: "enable", Key: "t", Source: "webhooks",
				Apply:    func() { m.setWebhookStatus(id, "active") },
				Rollback: func() { m.setWebhookStatus(id, "paused") },
				Do: func(ctx context.Context) error {
					active := "active"
					return m.rpcUpdateWebhook(ctx, &apiv1.UpdateSubscriptionRequest{Id: id, Status: &active})
				},
			})
		}
		acts = append(acts,
			kit2.Action{Label: "test", Key: "T", Source: "webhooks",
				Do: func(ctx context.Context) error { return m.rpcTestWebhook(ctx, id) }},
			kit2.Action{
				Label: "delete", Key: "x", Danger: true, Source: "webhooks",
				Confirm:  "Delete " + name + "?\nThis cannot be undone.",
				Apply:    func() { m.RemoveRow("webhooks", id) },
				Rollback: func() { m.Refresh("webhooks") },
				Do:       func(ctx context.Context) error { return m.rpcDeleteWebhook(ctx, id) },
			},
		)
		return acts

	case "mcp":
		id, name := item.ID, item.Title
		enabled, hasCred, installable := false, false, false
		secName := ""
		if s := m.mcpServers[id]; s != nil {
			enabled = s.GetEnabled()
			hasCred = s.GetHasSecretStored()
			installable = s.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO || s.GetCatalogSlug() != ""
			if len(s.GetRequiredSecrets()) > 0 {
				secName = s.GetRequiredSecrets()[0]
			}
		}
		acts := []kit2.Action{}
		if enabled {
			acts = append(acts, kit2.Action{
				Label: "disable", Key: "t", Danger: true, Source: "mcp",
				Confirm:  "Disable " + name + "?\nThe MCP client stops offering its tools at session time.",
				Apply:    func() { m.setMCPEnabledLocally(id, false) },
				Rollback: func() { m.setMCPEnabledLocally(id, true) },
				Do: func(ctx context.Context) error {
					off := false
					return m.rpcUpdateMCP(ctx, &apiv1.MCPServerUpdateRequest{Id: id, Enabled: &off})
				},
			})
		} else {
			acts = append(acts, kit2.Action{
				Label: "enable", Key: "t", Source: "mcp",
				Apply:    func() { m.setMCPEnabledLocally(id, true) },
				Rollback: func() { m.setMCPEnabledLocally(id, false) },
				Do: func(ctx context.Context) error {
					on := true
					return m.rpcUpdateMCP(ctx, &apiv1.MCPServerUpdateRequest{Id: id, Enabled: &on})
				},
			})
		}
		if installable {
			acts = append(acts, kit2.Action{Label: "install", Key: "i", Source: "mcp",
				Do: func(ctx context.Context) error { return m.rpcInstallMCP(ctx, id) }})
		}
		if hasCred || secName != "" {
			acts = append(acts, kit2.Action{
				Label: "clear credential", Key: "c", Danger: true, Source: "mcp",
				Confirm: "Clear the stored credential for " + name + "?\nThe MCP client can no longer authenticate.",
				Do:      func(ctx context.Context) error { return m.rpcClearMCPSecret(ctx, id, secName) },
			})
		}
		acts = append(acts, kit2.Action{
			Label: "delete", Key: "x", Danger: true, Source: "mcp",
			Confirm:  "Delete " + name + "?\nThis cannot be undone.",
			Apply:    func() { m.RemoveRow("mcp", id) },
			Rollback: func() { m.Refresh("mcp") },
			Do:       func(ctx context.Context) error { return m.rpcDeleteMCP(ctx, id) },
		})
		return acts

	case "providers":
		id, name := item.ID, item.Title
		enabled, custom, hasToken := false, false, false
		if p := m.providers[id]; p != nil {
			enabled = p.GetEnabled()
			custom = p.GetIsCustom()
			hasToken = p.GetHasTokenStored()
		}
		acts := []kit2.Action{}
		if enabled {
			acts = append(acts, kit2.Action{
				Label: "disable", Key: "t", Danger: true, Source: "providers",
				Confirm:  "Disable " + name + "?\nWorkers referencing its models fail to dispatch.",
				Apply:    func() { m.setProviderEnabledLocally(id, false) },
				Rollback: func() { m.setProviderEnabledLocally(id, true) },
				Do:       func(ctx context.Context) error { return m.rpcProviderEnabled(ctx, id, false) },
			})
		} else {
			acts = append(acts, kit2.Action{
				Label: "enable", Key: "t", Source: "providers",
				Apply:    func() { m.setProviderEnabledLocally(id, true) },
				Rollback: func() { m.setProviderEnabledLocally(id, false) },
				Do:       func(ctx context.Context) error { return m.rpcProviderEnabled(ctx, id, true) },
			})
		}
		acts = append(acts, kit2.Action{Label: "set token", Key: "s", Source: "providers",
			Do: func(ctx context.Context) error { return nil }}) // form opener handled in Update
		if hasToken {
			acts = append(acts, kit2.Action{
				Label: "clear token", Key: "c", Danger: true, Source: "providers",
				Confirm: "Clear the stored token for " + name + "?\nThe provider stops authenticating.",
				Do:      func(ctx context.Context) error { return m.rpcClearProviderToken(ctx, id) },
			})
		}
		if custom {
			acts = append(acts, kit2.Action{
				Label: "delete", Key: "x", Danger: true, Source: "providers",
				Confirm:  "Delete custom provider " + name + "?\nThis cannot be undone.",
				Apply:    func() { m.RemoveRow("providers", id) },
				Rollback: func() { m.Refresh("providers") },
				Do:       func(ctx context.Context) error { return m.rpcDeleteProvider(ctx, id) },
			})
		}
		return acts

	case "secrets":
		id, name := item.ID, item.Title
		return []kit2.Action{{
			Label: "delete", Key: "x", Danger: true, Source: "secrets",
			Confirm:  "Delete secret " + name + "?\nAny config referencing ${" + name + "} stops resolving.",
			Apply:    func() { m.RemoveRow("secrets", id) },
			Rollback: func() { m.Refresh("secrets") },
			Do:       func(ctx context.Context) error { return m.rpcDeleteSecret(ctx, id) },
		}}

	case "adapters":
		id, name := item.ID, item.Title
		if m.adapterDisabled[id] {
			return []kit2.Action{{
				Label: "enable", Key: "t", Source: "",
				Apply: func() {
					delete(m.adapterDisabled, id)
					m.MutateRow("adapters", id, func(r *kit2.Row) { r.Meta = strings.TrimSuffix(r.Meta, " · disabled (local)") })
				},
				Rollback: func() { m.adapterDisabled[id] = true },
			}}
		}
		return []kit2.Action{{
			Label: "disable", Key: "t", Danger: true, Source: "",
			Confirm: "Disable " + name + " for local dispatch?\nThe scheduler stops offering this adapter (client-side filter; the registry API is read-only).",
			Apply: func() {
				m.adapterDisabled[id] = true
				m.MutateRow("adapters", id, func(r *kit2.Row) { r.Meta += " · disabled (local)" })
			},
			Rollback: func() { delete(m.adapterDisabled, id) },
		}}
	}
	return nil
}

// actionForKey returns the action bound to a keybinding.
func (m *Model) actionForKey(key string) (kit2.Action, bool) {
	for _, a := range m.actionsForSelection() {
		if a.Key == key {
			return a, true
		}
	}
	return kit2.Action{}, false
}

// setWebhookStatus locally rewrites the selected webhook's status cell (the
// optimistic edit the mutation layer rolls back on failure).
func (m *Model) setWebhookStatus(id, status string) {
	m.MutateRow("webhooks", id, func(r *kit2.Row) { r.Meta = status })
}

func (m *Model) setMCPEnabledLocally(id string, enabled bool) {
	m.MutateRow("mcp", id, func(r *kit2.Row) {
		if enabled {
			r.Meta = "enabled"
		} else {
			r.Meta = "disabled"
		}
	})
}

func (m *Model) setProviderEnabledLocally(id string, enabled bool) {
	m.MutateRow("providers", id, func(r *kit2.Row) {
		prefix, suffix := r.Meta, ""
		if i := strings.Index(prefix, " · "); i >= 0 {
			suffix = prefix[i:]
			prefix = prefix[:i]
		}
		if enabled {
			r.Meta = prefix + " enabled" + suffix
		} else {
			r.Meta = prefix + " disabled" + suffix
		}
	})
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

// --- forms --------------------------------------------------------------

// newFormForSource opens the typed create form for the focused pane (nil when
// the pane has no creation surface).
func (m *Model) newFormForSource() *kit2.Form {
	switch m.ActiveSourceName() {
	case "webhooks":
		return m.newWebhookForm()
	case "mcp":
		return m.newMCPForm()
	case "providers":
		return m.newProviderForm()
	case "secrets":
		return m.newSecretForm()
	}
	return nil
}

// editFormForSource opens the typed edit form for the selected row (nil when
// the pane is read-only or nothing is selected).
func (m *Model) editFormForSource() *kit2.Form {
	// settings is a singleton pane — it needs no selected row.
	if m.ActiveSourceName() == "settings" {
		return m.settingsForm()
	}
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case "webhooks":
		return m.editWebhookForm(item)
	case "mcp":
		return m.editMCPForm(item)
	case "providers":
		return m.editProviderForm(item)
	case "secrets":
		return m.editSecretForm(item)
	}
	return nil
}

func (m *Model) formOpen() bool { return m.form != nil }

// newWebhookForm builds the typed create form.
func (m *Model) newWebhookForm() *kit2.Form {
	f := kit2.NewForm("New webhook subscription",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "build-finished"},
		kit2.FieldSpec{Name: "target_url", Label: "Target URL", Kind: kit2.KText, Required: true, Placeholder: "https://…", Validate: validURL},
		kit2.FieldSpec{Name: "event_filter", Label: "Event filter", Kind: kit2.KText, Placeholder: "execution.completed"},
		kit2.FieldSpec{Name: "scope", Label: "Scope", Kind: kit2.KSelect, Options: []kit2.Option{
			{Value: "tenant", Label: "tenant"}, {Value: "workflow", Label: "workflow"}, {Value: "execution", Label: "execution"},
		}},
		kit2.FieldSpec{Name: "secret", Label: "Signing secret", Kind: kit2.KSecret},
		kit2.FieldSpec{Name: "max_retries", Label: "Max retries", Kind: kit2.KNumber, Initial: "3"},
	)
	f.Focused = true
	f.Width = 60
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.CreateSubscriptionRequest{
			Name:        v["name"],
			TargetUrl:   v["target_url"],
			EventFilter: v["event_filter"],
			Scope:       v["scope"],
			Secret:      v["secret"],
			MaxRetries:  int32Of(v["max_retries"], 3),
		}
		return m.Mutate(mutate.Request{
			Name: "create webhook " + v["name"], Source: "webhooks",
			Do: func(ctx context.Context) error { return m.rpcCreateWebhook(ctx, req) },
		}), nil
	}
	return f
}

// editWebhookForm edits the mutable subscription fields (target url, event
// filter, status, retries — the UpdateSubscriptionRequest surface).
func (m *Model) editWebhookForm(item kit2.Item) *kit2.Form {
	s := m.webhooks[item.ID]
	status := "active"
	target, filter, retries := "", "", "3"
	if s != nil {
		status = s.GetStatus()
		if status == "" {
			status = "active"
		}
		target = s.GetTargetUrl()
		filter = s.GetEventFilter()
		retries = strconv.Itoa(int(s.GetMaxRetries()))
	}
	f := kit2.NewForm("Edit webhook: "+item.Title,
		kit2.FieldSpec{Name: "target_url", Label: "Target URL", Kind: kit2.KText, Required: true, Initial: target, Validate: validURL},
		kit2.FieldSpec{Name: "event_filter", Label: "Event filter", Kind: kit2.KText, Initial: filter},
		kit2.FieldSpec{Name: "status", Label: "Status", Kind: kit2.KSelect, Initial: status, Options: []kit2.Option{
			{Value: "active", Label: "active"}, {Value: "paused", Label: "paused"},
		}},
		kit2.FieldSpec{Name: "max_retries", Label: "Max retries", Kind: kit2.KNumber, Initial: retries},
	)
	f.Focused = true
	f.Width = 60
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		turl, filt, st := v["target_url"], v["event_filter"], v["status"]
		mr := int32Of(v["max_retries"], 3)
		req := &apiv1.UpdateSubscriptionRequest{Id: id, TargetUrl: &turl, EventFilter: &filt, Status: &st, MaxRetries: &mr}
		return m.Mutate(mutate.Request{
			Name: "edit webhook " + item.Title, Source: "webhooks",
			Do: func(ctx context.Context) error { return m.rpcUpdateWebhook(ctx, req) },
		}), nil
	}
	return f
}

// settingsForm edits and saves the tenant settings. Model refs are validated
// inline (before submit); only non-empty / non-zero fields are sent, matching
// the merge semantics of UpdateSettings.
func (m *Model) settingsForm() *kit2.Form {
	s := m.settings
	if s == nil {
		s = &apiv1.TenantSettings{}
	}
	num := func(v int64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatInt(v, 10)
	}
	i32 := func(v int32) string {
		if v == 0 {
			return ""
		}
		return strconv.Itoa(int(v))
	}
	f := kit2.NewForm("Edit tenant settings",
		kit2.FieldSpec{Name: "default_worker_model", Label: "Default worker model", Kind: kit2.KText, Initial: s.GetDefaultWorkerModel(), Validate: validModelRef, Placeholder: "provider/model"},
		kit2.FieldSpec{Name: "default_ask_model", Label: "Default ask model", Kind: kit2.KText, Initial: s.GetDefaultAskOrchiconModel(), Validate: validModelRef, Placeholder: "provider/model"},
		kit2.FieldSpec{Name: "max_concurrent_runs", Label: "Max concurrent runs", Kind: kit2.KNumber, Initial: i32(s.GetMaxConcurrentRuns())},
		kit2.FieldSpec{Name: "stall_no_progress_window_seconds", Label: "Stall no-progress window (s)", Kind: kit2.KNumber, Initial: num(s.GetStallNoProgressWindowSeconds())},
		kit2.FieldSpec{Name: "stall_no_file_diff_window_seconds", Label: "Stall no-diff window (s)", Kind: kit2.KNumber, Initial: num(s.GetStallNoFileDiffWindowSeconds())},
		kit2.FieldSpec{Name: "stall_text_loop_window_seconds", Label: "Stall text-loop window (s)", Kind: kit2.KNumber, Initial: num(s.GetStallTextLoopWindowSeconds())},
		kit2.FieldSpec{Name: "stall_repetition_count", Label: "Stall repetition count", Kind: kit2.KNumber, Initial: i32(s.GetStallRepetitionCount())},
		kit2.FieldSpec{Name: "stall_repetition_window_seconds", Label: "Stall repetition window (s)", Kind: kit2.KNumber, Initial: num(s.GetStallRepetitionWindowSeconds())},
		kit2.FieldSpec{Name: "stall_nudge_max", Label: "Stall nudge max", Kind: kit2.KNumber, Initial: i32(s.GetStallNudgeMax())},
		kit2.FieldSpec{Name: "stall_tool_hang_seconds", Label: "Stall tool-hang window (s)", Kind: kit2.KNumber, Initial: num(s.GetStallToolHangSeconds())},
		kit2.FieldSpec{Name: "execution_reap_grace_seconds", Label: "Exec reap grace (s)", Kind: kit2.KNumber, Initial: num(s.GetExecutionReapGraceSeconds())},
		kit2.FieldSpec{Name: "execution_reap_consecutive_failures", Label: "Exec reap consecutive failures", Kind: kit2.KNumber, Initial: i32(s.GetExecutionReapConsecutiveFailures())},
		kit2.FieldSpec{Name: "default_budget_overrides", Label: "Default budget overrides (JSON)", Kind: kit2.KJSON, Initial: s.GetDefaultBudgetOverrides()},
		kit2.FieldSpec{Name: "backup_schedule", Label: "Backup schedule (cron)", Kind: kit2.KText, Initial: s.GetBackupSchedule()},
		kit2.FieldSpec{Name: "backup_retention_days", Label: "Backup retention (days)", Kind: kit2.KNumber, Initial: i32(s.GetBackupRetentionDays())},
		kit2.FieldSpec{Name: "backup_directory", Label: "Backup directory", Kind: kit2.KText, Initial: s.GetBackupDirectory()},
		kit2.FieldSpec{Name: "log_directory", Label: "Log directory", Kind: kit2.KText, Initial: s.GetLogDirectory()},
		kit2.FieldSpec{Name: "log_max_size_mb", Label: "Log max size (MB)", Kind: kit2.KNumber, Initial: num(s.GetLogMaxSizeMb())},
		kit2.FieldSpec{Name: "log_retention_days", Label: "Log retention (days)", Kind: kit2.KNumber, Initial: i32(s.GetLogRetentionDays())},
		kit2.FieldSpec{Name: "session_access_token_ttl_seconds", Label: "Session access TTL (s)", Kind: kit2.KNumber, Initial: num(s.GetSessionAccessTokenTtlSeconds())},
		kit2.FieldSpec{Name: "session_refresh_token_ttl_seconds", Label: "Session refresh TTL (s)", Kind: kit2.KNumber, Initial: num(s.GetSessionRefreshTokenTtlSeconds())},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		out := &apiv1.TenantSettings{}
		out.DefaultWorkerModel = v["default_worker_model"]
		out.DefaultAskOrchiconModel = v["default_ask_model"]
		if n, ok := i64Of(v["max_concurrent_runs"]); ok {
			n32 := int32(n)
			out.MaxConcurrentRuns = &n32
		}
		out.StallNoProgressWindowSeconds = i64Of0(v["stall_no_progress_window_seconds"])
		out.StallNoFileDiffWindowSeconds = i64Of0(v["stall_no_file_diff_window_seconds"])
		out.StallTextLoopWindowSeconds = i64Of0(v["stall_text_loop_window_seconds"])
		out.StallRepetitionCount = int32(i64Of0(v["stall_repetition_count"]))
		out.StallRepetitionWindowSeconds = i64Of0(v["stall_repetition_window_seconds"])
		out.StallNudgeMax = int32(i64Of0(v["stall_nudge_max"]))
		out.StallToolHangSeconds = i64Of0(v["stall_tool_hang_seconds"])
		out.ExecutionReapGraceSeconds = i64Of0(v["execution_reap_grace_seconds"])
		out.ExecutionReapConsecutiveFailures = int32(i64Of0(v["execution_reap_consecutive_failures"]))
		out.DefaultBudgetOverrides = strings.TrimSpace(v["default_budget_overrides"])
		out.BackupSchedule = v["backup_schedule"]
		out.BackupRetentionDays = int32(i64Of0(v["backup_retention_days"]))
		out.BackupDirectory = v["backup_directory"]
		out.LogDirectory = v["log_directory"]
		out.LogMaxSizeMb = i64Of0(v["log_max_size_mb"])
		out.LogRetentionDays = int32(i64Of0(v["log_retention_days"]))
		out.SessionAccessTokenTtlSeconds = i64Of0(v["session_access_token_ttl_seconds"])
		out.SessionRefreshTokenTtlSeconds = i64Of0(v["session_refresh_token_ttl_seconds"])
		return m.Mutate(mutate.Request{
			Name: "save settings", Source: "settings",
			Do: func(ctx context.Context) error { return m.rpcUpdateSettings(ctx, out) },
		}), nil
	}
	return f
}

// newMCPForm builds the typed MCP server create form.
func (m *Model) newMCPForm() *kit2.Form {
	f := kit2.NewForm("New MCP server",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "github-mcp"},
		kit2.FieldSpec{Name: "transport", Label: "Transport", Kind: kit2.KSelect, Initial: "stdio", Options: []kit2.Option{
			{Value: "stdio", Label: "stdio"}, {Value: "streamable-http", Label: "streamable-http"},
		}},
		kit2.FieldSpec{Name: "command", Label: "Command (stdio)", Kind: kit2.KText, Placeholder: "npx"},
		kit2.FieldSpec{Name: "args", Label: "Args (space separated)", Kind: kit2.KText},
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: "true"},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.MCPServerCreateRequest{
			Name:      v["name"],
			Transport: mcpTransport(v["transport"]),
			Command:   v["command"],
			Args:      strings.Fields(v["args"]),
			Url:       v["url"],
			Enabled:   v["enabled"] == "true",
		}
		return m.Mutate(mutate.Request{
			Name: "create MCP server " + v["name"], Source: "mcp",
			Do: func(ctx context.Context) error { return m.rpcCreateMCP(ctx, req) },
		}), nil
	}
	return f
}

// editMCPForm edits the mutable MCP server fields (command/args, url, enabled).
func (m *Model) editMCPForm(item kit2.Item) *kit2.Form {
	s := m.mcpServers[item.ID]
	command, args, url, enabled := "", "", "", "true"
	if s != nil {
		command = s.GetCommand()
		args = strings.Join(s.GetArgs(), " ")
		url = s.GetUrl()
		if !s.GetEnabled() {
			enabled = "false"
		}
	}
	f := kit2.NewForm("Edit MCP server: "+item.Title,
		kit2.FieldSpec{Name: "command", Label: "Command (stdio)", Kind: kit2.KText, Initial: command},
		kit2.FieldSpec{Name: "args", Label: "Args (space separated)", Kind: kit2.KText, Initial: args},
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: url},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
	)
	f.Focused = true
	f.Width = 64
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.MCPServerUpdateRequest{Id: id}
		if cmd := strings.TrimSpace(v["command"]); cmd != "" {
			req.Command = &cmd
		}
		if argv := strings.Fields(v["args"]); len(argv) > 0 {
			req.Args = argv
			replace := true
			req.ReplaceArgs = &replace
		}
		if u := strings.TrimSpace(v["url"]); u != "" {
			req.Url = &u
		}
		en := v["enabled"] == "true"
		req.Enabled = &en
		return m.Mutate(mutate.Request{
			Name: "edit MCP server " + item.Title, Source: "mcp",
			Do: func(ctx context.Context) error { return m.rpcUpdateMCP(ctx, req) },
		}), nil
	}
	return f
}

// mcpSecretForm stores a credential for an MCP server via the secret store.
// The value field is a KSecret — masked in the view, written once, never read
// back.
func (m *Model) mcpSecretForm(item kit2.Item) *kit2.Form {
	name := ""
	if s := m.mcpServers[item.ID]; s != nil && len(s.GetRequiredSecrets()) > 0 {
		name = s.GetRequiredSecrets()[0]
	}
	f := kit2.NewForm("Store MCP credential: "+item.Title,
		kit2.FieldSpec{Name: "name", Label: "Credential key", Kind: kit2.KText, Required: true, Initial: name, Placeholder: "GITHUB_TOKEN"},
		kit2.FieldSpec{Name: "value", Label: "Value", Kind: kit2.KSecret, Required: true},
	)
	f.Focused = true
	f.Width = 64
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		key, val := v["name"], v["value"]
		return m.Mutate(mutate.Request{
			Name: "store MCP credential " + key, Source: "mcp",
			Do: func(ctx context.Context) error { return m.rpcSetMCPSecret(ctx, id, key, val) },
		}), nil
	}
	return f
}

// newProviderForm builds the custom-provider create form.
func (m *Model) newProviderForm() *kit2.Form {
	f := kit2.NewForm("New custom provider",
		kit2.FieldSpec{Name: "display_name", Label: "Display name", Kind: kit2.KText, Required: true, Placeholder: "Local Ollama"},
		kit2.FieldSpec{Name: "ref_id", Label: "Ref id", Kind: kit2.KText, Required: true, Placeholder: "local-ollama"},
		kit2.FieldSpec{Name: "base_url", Label: "Base URL", Kind: kit2.KText, Required: true, Validate: validURL, Placeholder: "http://127.0.0.1:11434"},
		kit2.FieldSpec{Name: "auth_mode", Label: "Auth mode", Kind: kit2.KSelect, Initial: "none", Options: []kit2.Option{
			{Value: "none", Label: "none"}, {Value: "bearer", Label: "bearer"}, {Value: "api_key", Label: "api_key"},
		}},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.ProviderCreateCustomRequest{
			DisplayName: v["display_name"],
			RefId:       v["ref_id"],
			BaseUrl:     v["base_url"],
			AuthMode:    v["auth_mode"],
		}
		return m.Mutate(mutate.Request{
			Name: "create provider " + v["display_name"], Source: "providers",
			Do: func(ctx context.Context) error { return m.rpcCreateProvider(ctx, req) },
		}), nil
	}
	return f
}

// editProviderForm edits a provider. Built-ins take the settings-update path
// (enabled + base_url_override); custom providers additionally take the
// custom-update path (display name / base url / auth mode).
func (m *Model) editProviderForm(item kit2.Item) *kit2.Form {
	p := m.providers[item.ID]
	dn, bu, am, ov := "", "", "none", ""
	enabled := "true"
	custom := false
	if p != nil {
		dn = p.GetDisplayName()
		bu = p.GetBaseUrl()
		am = p.GetAuthMode()
		if am == "" {
			am = "none"
		}
		ov = p.GetBaseUrlOverride()
		custom = p.GetIsCustom()
		if !p.GetEnabled() {
			enabled = "false"
		}
	}
	f := kit2.NewForm("Edit provider: "+item.Title,
		kit2.FieldSpec{Name: "display_name", Label: "Display name (custom)", Kind: kit2.KText, Initial: dn},
		kit2.FieldSpec{Name: "base_url", Label: "Base URL (custom)", Kind: kit2.KText, Initial: bu},
		kit2.FieldSpec{Name: "auth_mode", Label: "Auth mode", Kind: kit2.KSelect, Initial: am, Options: []kit2.Option{
			{Value: "none", Label: "none"}, {Value: "bearer", Label: "bearer"}, {Value: "api_key", Label: "api_key"},
		}},
		kit2.FieldSpec{Name: "base_url_override", Label: "Base URL override", Kind: kit2.KText, Initial: ov},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
	)
	f.Focused = true
	f.Width = 64
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		on := v["enabled"] == "true"
		override := strings.TrimSpace(v["base_url_override"])
		reqs := []mutate.Request{{
			Name: "save provider settings " + item.Title, Source: "providers",
			Do: func(ctx context.Context) error {
				return m.rpcProviderSettings(ctx, id, on, override)
			},
		}}
		if custom {
			dnv, buv, amv := v["display_name"], v["base_url"], v["auth_mode"]
			cust := &apiv1.ProviderUpdateCustomRequest{RefId: id}
			if strings.TrimSpace(dnv) != "" {
				cust.DisplayName = &dnv
			}
			if strings.TrimSpace(buv) != "" {
				cust.BaseUrl = &buv
			}
			if amv != "" {
				cust.AuthMode = &amv
			}
			reqs = append(reqs, mutate.Request{
				Name: "edit provider " + item.Title, Source: "providers",
				Do: func(ctx context.Context) error { return m.rpcUpdateProvider(ctx, cust) },
			})
		}
		cmds := make([]tea.Cmd, 0, len(reqs))
		for _, r := range reqs {
			cmds = append(cmds, m.Mutate(r))
		}
		return tea.Batch(cmds...), nil
	}
	return f
}

// providerTokenForm stores a provider API token via the secret store. The
// token field is a KSecret — masked in the view, written once, never read
// back.
func (m *Model) providerTokenForm(item kit2.Item) *kit2.Form {
	f := kit2.NewForm("Store provider token: "+item.Title,
		kit2.FieldSpec{Name: "token", Label: "Token", Kind: kit2.KSecret, Required: true},
	)
	f.Focused = true
	f.Width = 64
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		tok := v["token"]
		return m.Mutate(mutate.Request{
			Name: "store provider token " + item.Title, Source: "providers",
			Do: func(ctx context.Context) error { return m.rpcSetProviderToken(ctx, id, tok) },
		}), nil
	}
	return f
}

// newSecretForm creates a secret by name. The value is written once and never
// read back.
func (m *Model) newSecretForm() *kit2.Form {
	f := kit2.NewForm("New secret",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "GITHUB_TOKEN"},
		kit2.FieldSpec{Name: "value", Label: "Value", Kind: kit2.KSecret, Required: true},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KText},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.CreateSecretRequest{Name: v["name"], Value: v["value"], Description: v["description"]}
		return m.Mutate(mutate.Request{
			Name: "create secret " + v["name"], Source: "secrets",
			Do: func(ctx context.Context) error { return m.rpcCreateSecret(ctx, req) },
		}), nil
	}
	return f
}

// editSecretForm rotates a secret's value (write-only) or edits its
// description.
func (m *Model) editSecretForm(item kit2.Item) *kit2.Form {
	f := kit2.NewForm("Rotate secret: "+item.Title,
		kit2.FieldSpec{Name: "value", Label: "New value", Kind: kit2.KSecret},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KText},
	)
	f.Focused = true
	f.Width = 64
	id := item.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.UpdateSecretRequest{Id: id}
		if val := v["value"]; val != "" {
			req.Value = &val
		}
		if desc := v["description"]; desc != "" {
			req.Description = &desc
		}
		return m.Mutate(mutate.Request{
			Name: "rotate secret " + item.Title, Source: "secrets",
			Do: func(ctx context.Context) error { return m.rpcUpdateSecret(ctx, req) },
		}), nil
	}
	return f
}

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
		return m, nil
	}

	// Screen chords run only when no modal dialog is open (the dialog owns
	// every key through the kit2 base, layered over the focus ring).
	if m.Open == nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "n":
				if f := m.newFormForSource(); f != nil {
					m.form = f
					return m, nil
				}
			case "e":
				if f := m.editFormForSource(); f != nil {
					m.form = f
					return m, nil
				}
			case "s":
				// set credential / token form openers (per pane).
				if f := m.secretFormForSource(); f != nil {
					m.form = f
					return m, nil
				}
			default:
				if a, ok := m.actionForKey(k.String()); ok {
					return m, m.openActionsDialog(a)
				}
			}
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

// secretFormForSource opens the credential/token entry form for panes that
// store secrets (MCP "s", Providers "s").
func (m *Model) secretFormForSource() *kit2.Form {
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case "mcp":
		return m.mcpSecretForm(item)
	case "providers":
		return m.providerTokenForm(item)
	}
	return nil
}

// --- view ---------------------------------------------------------------

func (m *Model) View() string {
	m.refreshActionBar()

	// Nine Control sources render ONE pane at a time (focused source +
	// detail) — a nine-across grid truncates every cell to a few runes.
	body := m.Base.SinglePane(m.w, m.bodyHeight())
	sp := kit2.NewPanel("Activity", m.w, streamRows)
	sp.SetContent(m.stream.View())
	sp.Focused = false
	out := body + "\n" + sp.View()

	if m.formOpen() {
		box := formBox(m.form, m.w, m.h)
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

// formBox wraps the form in a titled dialog-sized box (tall enough for the
// whole field list, capped by the viewport).
func formBox(f *kit2.Form, w, h int) string {
	bw := min(72, w-4)
	if bw < 24 {
		bw = 24
	}
	bh := len(f.Specs)*2 + 5
	if h > 0 && bh > h {
		bh = h
	}
	if bh < 8 {
		bh = 8
	}
	d := &kit2.Dialog{Title: f.Title, Body: f.View(), Buttons: []string{"submit", "cancel"}}
	return d.Box(bw, bh)
}

func (m *Model) refreshActionBar() {
	actions := m.actionsForSelection()
	m.bar.Actions = actions
	if m.bar.Sel >= len(actions) {
		m.bar.Sel = 0
	}
}

// HintLine returns the screen's key cheat-sheet (pane-aware).
func (m *Model) HintLine() string {
	hint := "enter: detail focus · ←/→: pane · f: more pages · r: refresh"
	switch m.ActiveSourceName() {
	case "webhooks":
		hint = "n: new · e: edit · t: enable/disable · T: test · x: delete (deliveries ride the detail)"
	case "mcp":
		hint = "n: new · e: edit · t: enabled · s: set credential · c: clear · i: install · x: delete"
	case "providers":
		hint = "n: new custom · e: edit · t: enable/disable · s: set token · c: clear · x: delete"
	case "secrets":
		hint = "n: new · e: rotate value · x: delete (values are never read back)"
	case "adapters":
		hint = "t: enable/disable (local dispatch filter) · ←/→: pane · r: refresh"
	case "settings":
		hint = "e: edit & save (model refs validated before submit)"
	case "admin":
		hint = "admin-gated — the first row reports the live permission state"
	}
	return theme.HintText.Render(hint)
}

// --- shell hooks --------------------------------------------------------

func (m *Model) SelectSource(name string) bool        { return m.Base.SelectSource(name) }
func (m *Model) SelectItem(src, id string) bool       { return m.Base.SelectItem(src, id) }
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// --- helpers ------------------------------------------------------------

// validModelRef validates a model reference ("provider/model") before submit.
func validModelRef(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil // empty = leave unchanged
	}
	if strings.ContainsAny(v, " \t") {
		return errors.New("model ref must not contain spaces")
	}
	parts := strings.SplitN(v, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("expected provider/model (e.g. ollama/llama3)")
	}
	return nil
}

// validURL validates an http(s) URL before submit.
func validURL(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return errors.New("must start with http:// or https://")
	}
	return nil
}

func mcpTransport(v string) apiv1.MCPServerTransport {
	if v == "streamable-http" {
		return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP
	}
	return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO
}

func int32Of(s string, def int32) int32 {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32); err == nil {
		return int32(n)
	}
	return def
}

func i64Of(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}

func i64Of0(s string) int64 {
	n, _ := i64Of(s)
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
