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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/providers"
	"github.com/beardedparrott/orchicon/internal/secrets"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

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

	bar  *kit2.ActionBar
	form *kit2.Form

	// modelPicker is the open three-tier MODEL picker (adapter → provider →
	// model, with search). It is the SCREEN's modal, layered ABOVE m.form:
	// the model choice needs its own room (three tiers, a search box and a
	// list) and a form field cannot host mouse-driven tier navigation.
	modelPicker *kit2.ModelPicker
	// modelField names the form field a committed ref is written back to.
	modelField string

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
	// secretNames is the set of secret NAMES the tenant holds (never values — the list API does not
	// return them). The provider forms use it to say whether a token is already stored, which is the
	// one thing a masked field cannot tell the operator.
	secretNames map[string]bool
	settings    *apiv1.TenantSettings

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

	// Model-picker loads (model_picker.go). Thunks for the same reason as the
	// rest: a test asserts the per-adapter branch and the payload without a live
	// plane.
	rpcModelKinds     func(ctx context.Context) ([]string, []string, error)
	rpcModelProviders func(ctx context.Context, adapter string) ([]kit2.PickerOption, error)
	rpcModelModels    func(ctx context.Context, adapter, provider string) ([]kit2.PickerOption, bool, error)
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
		secretNames:     map[string]bool{},
	}
	m.NameStr = "control"
	// Workers live on the Execution tab and Runtime Images on the Work tab
	// (both match the GUI nav-config group placement); Control keeps only the
	// surfaces the GUI files under Control.
	m.AddSource("secrets", "Secrets (names only)", m.fetchSecrets)
	m.AddSource("mcp", "MCP Servers", m.fetchMCP)
	m.AddSource("providers", "Providers", m.fetchProviders)
	m.AddSource("webhooks", "Webhooks", m.fetchWebhooks)
	m.AddSource("adapters", "Adapters", m.fetchAdapters)
	m.AddSource("settings", "Settings", m.fetchSettings)
	// Themes is a Control surface (Settings → Themes in the GUI sense): the TUI
	// owns its palette set, and this is where the operator picks one. Selecting
	// a row and pressing the action key applies + persists it.
	m.AddSource("themes", "Themes", m.fetchThemes)
	// The Themes list has NO bulk operations: the operator's "spacebar does
	// multi-select even on theme lists. That doesn't make any sense as there isn't
	// any bulk operations on themes." Space activates here instead of marking (see
	// Base's space case), which is the gesture it replaced anyway.
	m.Base.SetMarkable("themes", false)
	m.AddSource("admin", "Admin", m.fetchAdmin)
	m.SetDetail(m.detail)
	m.Base.SetStatuses(nil)

	m.bar = kit2.NewActionBar()
	// Every write goes through the ONE mutation executor.
	m.SetExecutor(&mutate.Executor{Sink: m})
	// Enter/Space on a row does the natural thing for the focused pane: on
	// Themes it APPLIES the highlighted palette (the selection IS the intent —
	// matching the GUI, where clicking a theme switches to it), elsewhere it
	// opens the row's detail.
	m.OnActivate = func() (bool, tea.Cmd) {
		if m.ActiveSourceName() != "themes" {
			return false, nil // not ours: fall through to the default detail focus
		}
		item, ok := m.ActiveItem()
		if !ok {
			return false, nil
		}
		if item.ID == "" {
			// A section heading (DARK / LIGHT) is not a theme: activating it must
			// do nothing rather than look like a broken apply.
			return true, nil
		}
		if item.ID == theme.Active().Name {
			m.Notice("theme " + item.ID + " is already active")
			return true, nil
		}
		m.Notice("theme: " + item.ID)
		return true, m.applyTheme(item.ID)
	}

	// MOVING THROUGH THE THEMES APPLIES THEM. This is the hook that makes the themes list a PREVIEW rather
	// than a set of labels to pick from: the operator asked for exactly that — "auto switch to the theme as
	// the user is moving through them with the arrow keys or clicking on them as opposed to having to hit
	// enter to select" — and it mirrors the GUI, where clicking a theme switches to it. Enter still applies
	// (above) and is still the only gesture that also writes the notice.
	m.Base.OnHighlight = func() tea.Cmd { return m.previewTheme() }

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
	// The model picker's per-adapter loads (model_picker.go).
	m.rpcModelKinds = m.defaultModelKinds
	m.rpcModelProviders = m.defaultModelProviders
	m.rpcModelModels = m.defaultModelModels
	return m
}

func errNoClient(what string) error { return fmt.Errorf("no %s client", what) }

func (m *Model) Name() string { return "control" }

// Close unsubscribes (no live subs on this screen in v1).
func (m *Model) Close() { m.reg.CloseAll() }

// SetSize lays out the panes across the whole content region. (The old
// Activity stream panel that stole 7 rows was removed: mutation feedback
// already goes to the always-present dock, so the panel was a second,
// redundant surface that shrank every pane.)
func (m *Model) SetSize(w, h int) {
	m.w, m.h = w, h
	m.Base.SetSize(w, h)
	// The picker derives its centered box from the screen size, so a resize
	// must reach it or its mouse mapping drifts from what is drawn.
	if m.modelPicker != nil {
		m.modelPicker.SetScreen(w, h)
	}
}

func (m *Model) Init() tea.Cmd { return m.Load() }

// bodyHeight is the height of the pane row (the whole content region).
func (m *Model) bodyHeight() int { return m.h }

// ClaimsKeys reports whether the screen owns the keyboard: while a form or a
// Confirm dialog is open the shell hands over every key verbatim, so a secret
// value or a URL containing q / d / / is never eaten by a global chord.
func (m *Model) ClaimsKeys() bool {
	return m.form != nil || m.Open != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// ModalFormOpen reports a form drawn as its own centred WINDOW, which is the one
// state where Tab belongs to the form (field advance) rather than to the shell's
// tab ring. See router.go's tab chord.
// FormOpen reports whether a FORM is open — modal OR inline details-pane editor
// (plus the model picker, which is a modal layered over a form). While one is up,
// Tab moves through the form's FIELDS rather than the tab ring: "when in an edit
// form, tab should move through the fields of the form just like up/down keys ...
// Tabs should only move to the next menu item if you are NOT in edit mode."
func (m *Model) FormOpen() bool {
	return m.form != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// --- mutation sink (dock feedback) --------------------------------------

// Progress reports a running mutation in the always-present dock.
func (m *Model) Progress(msg string) {
	if d, ok := m.Shell().(dockSink); ok {
		d.DockNotice(msg)
	}
}

// Fail surfaces a mutation failure in the dock.
func (m *Model) Fail(msg string) {
	if d, ok := m.Shell().(dockSink); ok {
		d.DockError(msg)
	}
}

// Notice reports a successful mutation.
func (m *Model) Notice(msg string) {
	if d, ok := m.Shell().(dockSink); ok {
		d.DockNotice(msg)
	}
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
	// Remember the NAMES (never the values — the API does not return them). The provider forms ask
	// whether CUSTOM_<REF>_API_KEY already exists so a blank token field can say "stored" instead of
	// leaving the operator to guess; the name is what that question is answered from.
	m.secretNames = make(map[string]bool, len(resp.Msg.Secrets))
	for _, s := range resp.Msg.Secrets {
		m.secretNames[s.GetName()] = true
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

// fetchThemes lists the TUI's OWN palette set (internal/tui/theme), GROUPED into
// dark and light sections — the operator's "Themes should be separated by light
// and dark themes (two sections but same screen)". The palettes are chosen and
// contrast-validated for terminals rather than ported from the GUI's CSS tokens.
//
// A section row carries an EMPTY id, so selecting one is inert: applying a theme
// only ever resolves a real palette name (applyTheme refuses an empty or unknown
// name), so a heading can never be mistaken for a theme.
func (m *Model) fetchThemes(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	active := theme.Active().Name
	names := theme.Names()

	var dark, light []string
	for _, name := range names {
		if theme.IsDark(name) {
			dark = append(dark, name)
		} else {
			light = append(light, name)
		}
	}

	items := make([]kit2.Item, 0, len(names)+2)
	emit := func(heading string, group []string) {
		if len(group) == 0 {
			return
		}
		items = append(items, kit2.Item{ID: "", Title: heading, Meta: screenkit.FmtInt(len(group))})
		for _, name := range group {
			meta := "available"
			if name == active {
				meta = "active"
			}
			items = append(items, kit2.Item{ID: name, Title: name, Meta: meta})
		}
	}
	emit("DARK", dark)
	emit("LIGHT", light)
	return items, "", nil
}

// applyTheme switches the TUI palette through the shell (the shell owns the
// profile and the construction-captured styles), then reconciles this pane so
// the active marker moves.
//
// IT RETURNS THE RECONCILE COMMAND rather than discarding it. `m.Refresh(name)` only STAGES the reload and
// hands back the command that performs it, so the previous `m.Refresh("themes")` — whose return value went
// nowhere — left the pane showing the palette that WAS active until something else happened to refresh it.
// Invisible while a theme switch was a deliberate Enter press the operator followed with a glance; obvious
// now that moving the cursor applies live, where the "active" marker tracking the cursor IS the feedback
// that the preview took.
func (m *Model) applyTheme(name string) tea.Cmd {
	type themer interface{ SetTheme(string) bool }
	if sh, ok := m.Shell().(themer); ok {
		sh.SetTheme(name)
	} else if !theme.Use(name) {
		return nil
	}
	return m.Refresh("themes")
}

// previewTheme applies the palette under the cursor as the operator MOVES through the list.
//
// The operator: "when selecting themes, we should auto switch to the theme as the user is moving through
// them with the arrow keys or clicking on them as opposed to having to hit enter to select." The gesture is
// the PREVIEW, and it mirrors the GUI, where clicking a theme switches to it.
//
// THREE GUARDS, each of which would otherwise make moving through this list unpleasant:
//
//   - A SOURCE CHECK, because this hook is installed on the shared Base and fires for every source on this
//     screen. Applying a palette because the cursor moved over a SECRET would be absurd, and the check is
//     also what keeps this from reacting to a reload re-seating the cursor.
//   - A SECTION HEADING is not a theme. The rows carry DARK / LIGHT with an EMPTY id (see fetchThemes), so
//     arrowing onto a heading must move the cursor WITHOUT changing the palette.
//   - THE ALREADY-ACTIVE THEME IS NOT RE-APPLIED. Without this, parking on the active row and pressing up
//     and back down would re-persist the config and rewrite the notice on every keystroke — a theme
//     "switch" that is really no change, reported as if it were one.
func (m *Model) previewTheme() tea.Cmd {
	if m.ActiveSourceName() != "themes" {
		return nil
	}
	item, ok := m.ActiveItem()
	if !ok || item.ID == "" {
		return nil
	}
	if item.ID == theme.Active().Name {
		return nil
	}
	return m.applyTheme(item.ID)
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

	case "themes":
		// Themes is a TUI-owned surface: no RPC, the palette set lives in
		// internal/tui/theme. A section heading has no detail of its own.
		if id == "" {
			return "Themes", []screenkit.Field{
				{Key: "sections", Value: "DARK then LIGHT — pick a palette under either"},
				{Key: "apply", Value: "enter/a: apply the highlighted palette & save"},
			}, "", nil
		}
		active := theme.Active().Name
		state := "available"
		if id == active {
			state = "ACTIVE"
		}
		fields := []screenkit.Field{
			{Key: "theme", Value: id},
			{Key: "state", Value: state},
			{Key: "palettes", Value: strings.Join(theme.Names(), ", ")},
			{Key: "apply", Value: "a: apply & save (persists to the profile)"},
		}
		body := "TUI palettes are validated for terminal contrast — the borders carry each panel's title, so a browser hairline would render them invisible."
		return "Theme: " + id, fields, body, nil

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
	case "themes":
		id := item.ID
		if id == theme.Active().Name {
			return nil // already active: nothing to apply
		}
		return []kit2.Action{{
			Label: "apply", Key: "a", Source: "themes",
			Apply: func() { m.applyTheme(id) },
			// No RPC: applying a palette is local + a profile write.
			Do: func(ctx context.Context) error { return nil },
		}}
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

// formOpen reports whether ANY form is open — the legacy modal or the inline
// details-pane editor. Most forms now open in the pane, so a check that only
// looked at the modal field would report "not open" for an open editor.
func (m *Model) formOpen() bool { return m.form != nil || m.Base.EditingDetail() }

// activeForm is the form the operator is currently editing, whichever host holds
// it. Tests drive writes through this, so they assert WHAT is being edited rather
// than WHERE it is drawn — which is the point of the change: the host is a
// presentation choice, not part of the contract.
func (m *Model) activeForm() *kit2.Form {
	if m.form != nil {
		return m.form
	}
	return m.Base.DetailForm()
}

// clearForm closes whichever host holds the form (what a screen does after a
// submit or a cancel).
func (m *Model) clearForm() {
	m.form = nil
	m.Base.CloseDetailEdit()
}

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
	// The budget fields are seeded from the transport blob (it is the API's only
	// representation of them).
	bi := func(key, fallback string) string {
		if v := budgetInitials(s.GetDefaultBudgetOverrides())[key]; v != "" {
			return v
		}
		return fallback
	}
	f := kit2.NewForm("Edit tenant settings",
		// Model refs are CHOSEN, not typed: a kit2.KModel field opens the
		// three-tier ModelPicker (adapter → provider → model) and then DISPLAYS
		// the committed ref. The validator stays as the backstop for a stored
		// value the picker did not produce.
		kit2.FieldSpec{Name: "default_worker_model", Label: "Default worker model", Kind: kit2.KModel, Initial: s.GetDefaultWorkerModel(), Validate: validModelRef, Placeholder: "— none — (enter to choose a model)"},
		kit2.FieldSpec{Name: "default_ask_model", Label: "Default ask model", Kind: kit2.KModel, Initial: s.GetDefaultAskOrchiconModel(), Validate: validModelRef, Placeholder: "— none — (enter to choose a model)"},
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
		// --- Execution budget gates ---
		//
		// These used to be ONE json blob. The operator: "Currently default budget
		// overrides is one big JSON blob. This should be expanded out into
		// individual fields with short details on what each field is similar to
		// the stall settings."
		//
		// The blob is a TRANSPORT shape over typed columns (db.BudgetLadder is the
		// source of truth), so each field here is one key of it. An EMPTY gate
		// means "built-in default" and an explicit 0 DISABLES it — which is why
		// they are left blank rather than zero-filled: blank and 0 mean different
		// things, and every placeholder says which is which.
		kit2.FieldSpec{Name: "budget_tokens", Label: "Budget - tokens (0 = off, blank = built-in)", Kind: kit2.KNumber, Initial: bi("budget_tokens", ""), Placeholder: "all tokens at full weight, cache included"},
		kit2.FieldSpec{Name: "budget_cost_usd", Label: "Budget - cost USD", Kind: kit2.KNumber, Initial: bi("budget_cost_usd", ""), Placeholder: "priced cost, a separate gate from tokens"},
		kit2.FieldSpec{Name: "budget_wall_clock_seconds", Label: "Budget - wall clock (s)", Kind: kit2.KNumber, Initial: bi("budget_wall_clock_seconds", ""), Placeholder: "max runtime before the run is aborted"},
		kit2.FieldSpec{Name: "budget_tool_call_count", Label: "Budget - tool calls", Kind: kit2.KNumber, Initial: bi("budget_tool_call_count", ""), Placeholder: "how many tool calls before abort"},
		kit2.FieldSpec{Name: "budget_compact_max_turns", Label: "Budget - compact at turns", Kind: kit2.KNumber, Initial: bi("budget_compact_max_turns", ""), Placeholder: "turns before a context compaction is forced"},
		// The LADDER: each dimension goes warn, escalate, final and then ABORTS.
		// A fraction is OF THE GATE ABOVE (0.5 = half of it).
		kit2.FieldSpec{Name: "warn_frac_tokens", Label: "Ladder - tokens: warn,escalate,final", Kind: kit2.KText, Initial: bi("warn_frac_tokens", ""), Placeholder: "0.25,0.5,0.75", Validate: validFractionTriple},
		kit2.FieldSpec{Name: "warn_frac_cost", Label: "Ladder - cost: warn,escalate,final", Kind: kit2.KText, Initial: bi("warn_frac_cost", ""), Placeholder: "0.25,0.5,0.75", Validate: validFractionTriple},
		kit2.FieldSpec{Name: "warn_frac_tools", Label: "Ladder - tool calls: warn,escalate,final", Kind: kit2.KText, Initial: bi("warn_frac_tools", ""), Placeholder: "0.25,0.5,0.75", Validate: validFractionTriple},
		kit2.FieldSpec{Name: "warn_frac_time", Label: "Ladder - wall clock: warn,escalate,final", Kind: kit2.KText, Initial: bi("warn_frac_time", ""), Placeholder: "0.25,0.5,0.75", Validate: validFractionTriple},
		// --- The warning TEXT (the ladder's other half) ---
		//
		// The fractions above decide WHEN each stage fires; these are the words
		// INJECTED INTO THE SESSION at that stage — the message the worker actually
		// reads when it crosses a threshold. They are the remaining keys of the blob,
		// so without them an operator could tune the thresholds from the TUI but not
		// the instruction the worker obeys. The GUI has shipped these all along
		// (frontend/src/routes/settings.tsx, BudgetWarningsEditor: one message per tier
		// per dimension), which is what made their absence here a parity gap.
		//
		// `{pct}` is substituted with the percentage of that dimension's limit already
		// consumed. They are KTextArea because the real copy is a paragraph, not a
		// label; ctrl+e expands the focused field to a wrapped editor.
		kit2.FieldSpec{Name: "warn_msg_tokens", Label: "Warning text - tokens: WARN", Kind: kit2.KTextArea, Initial: bi("warn_msg_tokens", ""), Placeholder: "sent at the first threshold; {pct} = % of the token limit used"},
		kit2.FieldSpec{Name: "esc_msg_tokens", Label: "Warning text - tokens: ESCALATE", Kind: kit2.KTextArea, Initial: bi("esc_msg_tokens", ""), Placeholder: "a firmer restatement; the worker has not corrected course"},
		kit2.FieldSpec{Name: "final_msg_tokens", Label: "Warning text - tokens: FINAL", Kind: kit2.KTextArea, Initial: bi("final_msg_tokens", ""), Placeholder: "the last message before the limit ABORTS the run"},
		kit2.FieldSpec{Name: "warn_msg_cost", Label: "Warning text - cost: WARN", Kind: kit2.KTextArea, Initial: bi("warn_msg_cost", ""), Placeholder: "sent at the first threshold; {pct} = % of the cost limit used"},
		kit2.FieldSpec{Name: "esc_msg_cost", Label: "Warning text - cost: ESCALATE", Kind: kit2.KTextArea, Initial: bi("esc_msg_cost", ""), Placeholder: "a firmer restatement; the worker has not corrected course"},
		kit2.FieldSpec{Name: "final_msg_cost", Label: "Warning text - cost: FINAL", Kind: kit2.KTextArea, Initial: bi("final_msg_cost", ""), Placeholder: "the last message before the limit ABORTS the run"},
		kit2.FieldSpec{Name: "warn_msg_tools", Label: "Warning text - tool calls: WARN", Kind: kit2.KTextArea, Initial: bi("warn_msg_tools", ""), Placeholder: "sent at the first threshold; {pct} = % of the tool-call limit used"},
		kit2.FieldSpec{Name: "esc_msg_tools", Label: "Warning text - tool calls: ESCALATE", Kind: kit2.KTextArea, Initial: bi("esc_msg_tools", ""), Placeholder: "a firmer restatement; the worker has not corrected course"},
		kit2.FieldSpec{Name: "final_msg_tools", Label: "Warning text - tool calls: FINAL", Kind: kit2.KTextArea, Initial: bi("final_msg_tools", ""), Placeholder: "the last message before the limit ABORTS the run"},
		kit2.FieldSpec{Name: "warn_msg_time", Label: "Warning text - wall clock: WARN", Kind: kit2.KTextArea, Initial: bi("warn_msg_time", ""), Placeholder: "sent at the first threshold; {pct} = % of the time limit used"},
		kit2.FieldSpec{Name: "esc_msg_time", Label: "Warning text - wall clock: ESCALATE", Kind: kit2.KTextArea, Initial: bi("esc_msg_time", ""), Placeholder: "a firmer restatement; the worker has not corrected course"},
		kit2.FieldSpec{Name: "final_msg_time", Label: "Warning text - wall clock: FINAL", Kind: kit2.KTextArea, Initial: bi("final_msg_time", ""), Placeholder: "the last message before the limit ABORTS the run"},
		// Which ladder tiers ALSO compact. The warn tier defaults to OFF: a lossy
		// collapse at the first warning interrupts the worker mid-flight.
		kit2.FieldSpec{Name: "compact_tier_warn", Label: "Compact at WARN tier", Kind: kit2.KCheckbox, Initial: bi("compact_tier_warn", "")},
		kit2.FieldSpec{Name: "compact_tier_escalate", Label: "Compact at ESCALATE tier", Kind: kit2.KCheckbox, Initial: bi("compact_tier_escalate", "")},
		kit2.FieldSpec{Name: "compact_tier_final", Label: "Compact at FINAL tier", Kind: kit2.KCheckbox, Initial: bi("compact_tier_final", "")},
		// Context compaction + memory.
		kit2.FieldSpec{Name: "compact_enabled", Label: "Context compaction enabled", Kind: kit2.KCheckbox, Initial: bi("compact_enabled", "")},
		kit2.FieldSpec{Name: "compact_pressure_frac", Label: "Compaction pressure fraction", Kind: kit2.KNumber, Initial: bi("compact_pressure_frac", ""), Placeholder: "0.9 = compact at 90% of the window"},
		kit2.FieldSpec{Name: "compact_recent_turns", Label: "Compaction keeps recent turns", Kind: kit2.KNumber, Initial: bi("compact_recent_turns", ""), Placeholder: "how many recent turns survive the collapse"},
		kit2.FieldSpec{Name: "memory_enabled", Label: "Memory enabled", Kind: kit2.KCheckbox, Initial: bi("memory_enabled", "")},
		kit2.FieldSpec{Name: "memory_digest_entries", Label: "Memory digest entries", Kind: kit2.KNumber, Initial: bi("memory_digest_entries", ""), Placeholder: "how many digest entries are carried"},
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
	// A model field opens the SCREEN's modal picker (enter/space), seeded from
	// the field's current ref.
	f.OnOpenModelPicker = m.openModelPicker
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
		// The individual fields are composed back into the transport JSON the server
		// merges into its typed columns. Absent keys are OMITTED rather than sent
		// as zero: the server treats an absent gate as "keep the current value" and
		// an explicit 0 as "disable it", so a blank field must not silently turn a
		// gate off.
		out.DefaultBudgetOverrides = buildBudgetJSON(v)
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
		kit2.FieldSpec{Name: "auth_mode", Label: "Auth mode", Kind: kit2.KSelect, Initial: providers.AuthModeNone, Options: authModeOptions()},
		// THE TOKEN IS A FIELD HERE, not a separate `s` chord. The operator: "We should get rid of the 's'
		// to set a token on providers and have that as just another inline field in the edit/new." It
		// was a second form reached by a chord the hint had to explain, for a value that belongs to the
		// same object — and setting it needed the provider to exist FIRST, so the two steps were never
		// really independent.
		//
		// OPTIONAL by design: a provider may legitimately have no token (auth mode `none`), and a
		// blank field must not block the create. It is a KSecret, so the value is masked in the view
		// and never read back.
		kit2.FieldSpec{Name: "token", Label: "API token (optional)", Kind: kit2.KSecret,
			Placeholder: "paste the key — stored as CUSTOM_<REF>_API_KEY, never read back"},
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
		// The token is TWO RPCs on the server's side — create, then set the token against the
		// provider's ref id (the same two calls the GUI makes) — so both ride in ONE mutation, in
		// order, and a failure of either surfaces as one failure of "create provider".
		tok := strings.TrimSpace(v["token"])
		refID := strings.TrimSpace(v["ref_id"])
		return m.Mutate(mutate.Request{
			Name: "create provider " + v["display_name"], Source: "providers",
			Do: func(ctx context.Context) error {
				if err := m.rpcCreateProvider(ctx, req); err != nil {
					return err
				}
				if tok == "" {
					return nil
				}
				// A custom provider's id IS its ref id (providers.CreateCustom: ID: in.RefID), which is
				// what the token's secret name is derived from too.
				return m.rpcSetProviderToken(ctx, refID, tok)
			},
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
		kit2.FieldSpec{Name: "auth_mode", Label: "Auth mode", Kind: kit2.KSelect, Initial: normalizeAuthMode(am), Options: authModeOptions()},
		kit2.FieldSpec{Name: "base_url_override", Label: "Base URL override", Kind: kit2.KText, Initial: ov},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
		// The token is a field HERE too, so "change the provider's auth" is one form rather than a form
		// plus a chord. BLANK MEANS LEAVE IT ALONE: a KSecret is never read back, so an empty field is
		// indistinguishable from "unchanged" and must be treated as such — otherwise merely editing a
		// base URL would wipe the stored token.
		kit2.FieldSpec{Name: "token", Label: "API token (blank = unchanged)", Kind: kit2.KSecret,
			Placeholder: m.tokenPlaceholder(item.ID)},
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
		cmds := make([]tea.Cmd, 0, len(reqs)+1)
		for _, r := range reqs {
			cmds = append(cmds, m.Mutate(r))
		}
		// THE TOKEN, ONLY WHEN TYPED. A KSecret is never read back, so a blank field carries no
		// information — treating it as "clear the token" would silently wipe the credential of anyone
		// who edited only the base URL.
		if tok := strings.TrimSpace(v["token"]); tok != "" {
			cmds = append(cmds, m.Mutate(mutate.Request{
				Name: "store provider token " + item.Title, Source: "providers",
				Do: func(ctx context.Context) error { return m.rpcSetProviderToken(ctx, id, tok) },
			}))
		}
		return tea.Batch(cmds...), nil
	}
	return f
}

// tokenPlaceholder states whether a token is already stored for this provider.
//
// A secret's value is never readable, so without this the operator cannot tell "I already gave this
// provider a token" from "I never did" — and the two call for opposite actions when the field is
// blank. The token's secret NAME is derived from the ref id (`CUSTOM_<REF uppercased, - → _>_API_KEY`,
// providers.CustomSecretName), so asking the secrets list for it answers the question without ever
// touching the value.
func (m *Model) tokenPlaceholder(providerID string) string {
	want := providers.CustomSecretName(providerID)
	if m.secretNames[want] {
		return "stored (" + want + ") — type to replace"
	}
	return "none stored — will be stored as " + want
}

// newSecretForm creates a secret by name. The value is written once and never
// read back.
func (m *Model) newSecretForm() *kit2.Form {
	f := kit2.NewForm("New secret",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Validate: validSecretName, Placeholder: "GITHUB_TOKEN"},
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

	// The MODEL PICKER is a modal layered ABOVE the form that opened it: while it
	// is up it owns every key and the mouse, so a keystroke aimed at the picker
	// can never act on the form underneath it.
	if mp := m.modelPicker; mp != nil {
		switch msg := msg.(type) {
		case modelKindsMsg:
			return m, m.applyModelKinds(msg)
		case modelProvidersMsg:
			return m, m.applyModelProviders(msg)
		case modelModelsMsg:
			return m, m.applyModelModels(msg)
		case tea.KeyMsg:
			_, cmd := mp.HandleKey(msg)
			return m, tea.Batch(cmd, m.finishModelPicker(mp))
		case tea.MouseMsg:
			_, cmd := mp.HandleMouse(msg)
			return m, tea.Batch(cmd, m.finishModelPicker(mp))
		}
	}

	// The INLINE details-pane editor owns every key while it is up — it is the
	// focused surface, so it comes BEFORE the screen's own chords.
	//
	// It has to: the chords below answer 'n' (new), 'e' (edit) and 's' (set
	// credential), so with a form open in the pane a plain letter fired a chord
	// instead of being typed — every 'n', 'e' or 's' in a typed value re-opened a
	// form and discarded the input.
	if m.Base.EditingDetail() {
		if k, ok := msg.(tea.KeyMsg); ok {
			if handled, cmd := m.Base.Update(k); handled {
				return m, cmd
			}
		}
	}

	// The LEGACY modal host owns every key while it is up. Forms open in the
	// details pane now (see the n/e/s chords), so this path is normally inert — it
	// is kept for the hosts that still use it, and checks m.form directly rather
	// than formOpen() so an INLINE editor is not mistaken for it.
	if m.form != nil {
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
	//
	// CREATE and EDIT open in the DETAILS PANE, not a modal. The operator: "Edit
	// Tenant settings is popping up as a separate modal. We should not do that. It
	// should be edited in the details pane just like anything else." A modal covers
	// the list AND the detail it is editing; the pane keeps both visible, and it is
	// the same host the work-item and worker forms already use, so the keys,
	// validation and submit path cannot diverge between them.
	if m.Open == nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "n":
				if f := m.newFormForSource(); f != nil {
					m.Base.BeginDetailEdit(f.Title, f)
					return m, nil
				}
			case "e":
				if f := m.editFormForSource(); f != nil {
					m.Base.BeginDetailEdit(f.Title, f)
					return m, nil
				}
			case "s":
				// set credential / token form openers (per pane).
				if f := m.secretFormForSource(); f != nil {
					m.Base.BeginDetailEdit(f.Title, f)
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
// --- the `s` chord is GONE for providers -------------------------------------------------------
//
// The operator: "We should get rid of the 's' to set a token on providers and have that as just
// another inline field in the edit/new." The token is now a field on both provider forms, so there is
// no separate form to open — and no chord to explain. `secretFormForSource` no longer answers for the
// providers pane; MCP keeps its own `s` (a credential per MCP SERVER is a genuinely separate object,
// not a field of the server's own definition).
func (m *Model) secretFormForSource() *kit2.Form {
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case "mcp":
		return m.mcpSecretForm(item)
	}
	return nil
}

// --- view ---------------------------------------------------------------

func (m *Model) View() string {
	m.refreshActionBar()

	// Nine Control sources render ONE pane at a time (focused source +
	// detail) — a nine-across grid truncates every cell to a few runes.
	out := m.Base.SinglePane(m.w, m.bodyHeight())

	if m.form != nil {
		box := formBox(m.form, m.w, m.h)
		out = kit2.Center(kit2.FitLines(out, m.w, m.h), box, m.w, m.h)
	} else if m.Open != nil {
		box := m.Open.Box(min(60, m.w-4), 9)
		out = kit2.Center(kit2.FitLines(out, m.w, m.h), box, m.w, m.h)
	}
	// The model picker is spliced LAST so it layers ABOVE the form that opened
	// it. It is re-sized here from the same w×h Center() uses, so its mouse
	// hit-testing addresses the box that was actually drawn.
	if m.modelPicker != nil {
		m.modelPicker.SetScreen(m.w, m.h)
		out = kit2.Center(kit2.FitLines(out, m.w, m.h), m.modelPicker.View(), m.w, m.h)
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
	hint := "enter: detail focus · ←/→: pane · r: refresh"
	switch m.ActiveSourceName() {
	case "webhooks":
		hint = "n: new · e: edit · t: enable/disable · T: test · x: delete (deliveries ride the detail)"
	case "mcp":
		hint = "n: new · e: edit · t: enabled · s: set credential · c: clear · i: install · x: delete"
	case "providers":
		hint = "n: new custom · e: edit (the token is a field) · t: enable/disable · c: clear token · x: delete"
	case "secrets":
		hint = "n: new · e: rotate value · x: delete (values are never read back)"
	case "adapters":
		hint = "t: enable/disable (local dispatch filter) · ←/→: pane · r: refresh"
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

// validModelRef validates a model reference against the PINNED grammar
// (internal/adapter.ParseModelRef) — the same parser the server validates with,
// so the TUI can never accept a ref the plane would reject, or reject one it
// would accept.
//
// This replaced a hand-rolled SplitN("/", 2) splitter that disagreed with the
// grammar in BOTH directions: it accepted malformed 4-segment junk ("a/b/c/d"
// split as "a" + "b/c/d" and passed) and rejected a legal 1-segment bare model
// id, which the grammar explicitly allows.
func validModelRef(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil // empty = leave unchanged
	}
	if strings.ContainsAny(v, " \t") {
		return errors.New("model ref must not contain spaces")
	}
	if _, err := adapter.ParseModelRef(v, nil); err != nil {
		return err
	}
	return nil
}

// validSecretName rejects a name the secrets store will refuse, AT THE POINT OF ENTRY.
//
// It calls the SERVER'S OWN validator (secrets.ValidateName) rather than restating the rule, so the
// two can never drift — the same reasoning the model picker uses for a context window. That matters
// because the rule is not obvious: it is `^[A-Z][A-Z0-9_]+$`, i.e. UPPERCASE ONLY, and a lowercase
// name is the natural thing to type.
//
// WHY THIS IS A BUG FIX AND NOT POLISH. Without it the form accepted `my_token`, CLOSED as if it had
// saved, and only then did the server reject it — leaving the operator with a closed form, no row,
// and a transient dock error. That is the operator's report exactly: "I tried adding a secret and a
// provider in the TUI and saved it, yet they were never actually created." A validation error the
// form can see is one the form can SHOW, next to the field, before it closes.
func validSecretName(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil // Required reports emptiness; this reports a malformed NAME
	}
	if err := secrets.ValidateName(v); err != nil {
		// The server's message already names the rule; keep it, and add the human hint the regex
		// alone does not give.
		return errors.New("must be UPPERCASE: letters, digits and _ only, starting with a letter (e.g. GITHUB_TOKEN)")
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

// buildBudgetJSON composes the individual budget fields back into the transport
// JSON the Settings API expects.
//
// An EMPTY field is OMITTED, never sent as zero: the server stores an absent gate
// as NULL ("built-in default") and an explicit 0 as a real value that DISABLES the
// gate, so writing 0 for a blank field would turn gates off whenever the operator
// saved the form without touching them.
// budgetInitials reads the transport blob into the individual field values the
// form edits.
//
// The blob is the API's ONLY representation of the budget (the typed columns are
// the DB's source of truth, but TenantSettings exposes just the JSON), so the form
// has to read it — and write it back through buildBudgetJSON. The two functions
// are exact inverses for the keys the form covers.
func budgetInitials(blob string) map[string]string {
	out := map[string]string{}
	var raw struct {
		Tokens            *float64 `json:"tokens"`
		CostUSD           *float64 `json:"cost_usd"`
		WallClockSecs     *float64 `json:"wall_clock_seconds"`
		ToolCallCount     *float64 `json:"tool_call_count"`
		CompactMaxTurns   *float64 `json:"compact_max_turns"`
		CompactTiers      []bool   `json:"compact_tiers"`
		ContextCompaction *struct {
			Enabled      *bool    `json:"enabled"`
			PressureFrac *float64 `json:"pressure_frac"`
			RecentTurns  *int     `json:"recent_turns"`
		} `json:"context_compaction"`
		Memory *struct {
			Enabled       *bool `json:"enabled"`
			DigestEntries *int  `json:"digest_entries"`
		} `json:"memory"`
		Warnings struct {
			Fractions map[string][3]float64 `json:"fractions"`
			Messages  map[string][3]string  `json:"messages"`
		} `json:"warnings"`
	}
	if strings.TrimSpace(blob) != "" {
		_ = json.Unmarshal([]byte(blob), &raw)
	}
	f := func(v *float64) string {
		if v == nil {
			return ""
		}
		return trimZero(*v)
	}
	out["budget_tokens"] = f(raw.Tokens)
	out["budget_cost_usd"] = f(raw.CostUSD)
	out["budget_wall_clock_seconds"] = f(raw.WallClockSecs)
	out["budget_tool_call_count"] = f(raw.ToolCallCount)
	out["budget_compact_max_turns"] = f(raw.CompactMaxTurns)

	fracFor := func(key string) string {
		t, ok := raw.Warnings.Fractions[key]
		if !ok {
			return ""
		}
		return fracTriple(t[0], t[1], t[2])
	}
	out["warn_frac_tokens"] = fracFor("tokens")
	out["warn_frac_cost"] = fracFor("cost_usd")
	out["warn_frac_tools"] = fracFor("tool_call_count")
	out["warn_frac_time"] = fracFor("wall_clock_seconds")

	// The warning TEXT, one field per tier per dimension. Absent key = "" so a
	// dimension the blob does not carry reads as blank and writes back omitted.
	msgFor := func(key string, i int) string {
		t, ok := raw.Warnings.Messages[key]
		if !ok || i < 0 || i >= len(t) {
			return ""
		}
		return t[i]
	}
	for _, d := range []struct {
		key              string
		warn, esc, final string
	}{
		{"tokens", "warn_msg_tokens", "esc_msg_tokens", "final_msg_tokens"},
		{"cost_usd", "warn_msg_cost", "esc_msg_cost", "final_msg_cost"},
		{"tool_call_count", "warn_msg_tools", "esc_msg_tools", "final_msg_tools"},
		{"wall_clock_seconds", "warn_msg_time", "esc_msg_time", "final_msg_time"},
	} {
		out[d.warn] = msgFor(d.key, 0)
		out[d.esc] = msgFor(d.key, 1)
		out[d.final] = msgFor(d.key, 2)
	}

	// The tier toggles default OFF/ON/ON when absent — the built-in policy, whose
	// WARN tier is off because a lossy collapse at the first warning interrupts the
	// worker mid-flight.
	warn, escal, final := false, true, true
	if len(raw.CompactTiers) == 3 {
		warn, escal, final = raw.CompactTiers[0], raw.CompactTiers[1], raw.CompactTiers[2]
	}
	out["compact_tier_warn"] = boolStr(warn)
	out["compact_tier_escalate"] = boolStr(escal)
	out["compact_tier_final"] = boolStr(final)

	if cc := raw.ContextCompaction; cc != nil {
		if cc.Enabled != nil {
			out["compact_enabled"] = boolStr(*cc.Enabled)
		}
		if cc.PressureFrac != nil {
			out["compact_pressure_frac"] = fmtFrac(*cc.PressureFrac)
		}
		if cc.RecentTurns != nil {
			out["compact_recent_turns"] = strconv.Itoa(*cc.RecentTurns)
		}
	}
	if mem := raw.Memory; mem != nil {
		if mem.Enabled != nil {
			out["memory_enabled"] = boolStr(*mem.Enabled)
		}
		if mem.DigestEntries != nil {
			out["memory_digest_entries"] = strconv.Itoa(*mem.DigestEntries)
		}
	}
	return out
}
func buildBudgetJSON(v map[string]string) string {
	out := map[string]any{}
	gate := func(key, field string) {
		raw := strings.TrimSpace(v[field])
		if raw == "" {
			return
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return
		}
		out[key] = n
	}
	gate("tokens", "budget_tokens")
	gate("cost_usd", "budget_cost_usd")
	gate("wall_clock_seconds", "budget_wall_clock_seconds")
	gate("tool_call_count", "budget_tool_call_count")
	gate("compact_max_turns", "budget_compact_max_turns")

	fractions := map[string]any{}
	for key, field := range map[string]string{
		"tokens": "warn_frac_tokens", "cost_usd": "warn_frac_cost",
		"tool_call_count": "warn_frac_tools", "wall_clock_seconds": "warn_frac_time",
	} {
		if tri, ok := parseFractionTriple(v[field]); ok {
			fractions[key] = tri
		}
	}
	// The warning TEXT. A dimension is written only when at least ONE of its three
	// messages is non-blank, and then all three go together — the rule the GUI's
	// buildBudgetDefaults applies, and the one the server's ApplyBudgetJSON expects,
	// because it reads `warnings.messages.<dim>` as a whole triple. So blanking every
	// tier of a dimension leaves the stored copy untouched (the key is omitted, and
	// an absent key means "keep"), while blanking a single tier in an otherwise
	// populated dimension SILENCES just that tier — the adapter assigns each string
	// verbatim, so "" injects nothing. That is a real edit, not a fallback.
	messages := map[string]any{}
	for key, fields := range map[string][3]string{
		"tokens":             {"warn_msg_tokens", "esc_msg_tokens", "final_msg_tokens"},
		"cost_usd":           {"warn_msg_cost", "esc_msg_cost", "final_msg_cost"},
		"tool_call_count":    {"warn_msg_tools", "esc_msg_tools", "final_msg_tools"},
		"wall_clock_seconds": {"warn_msg_time", "esc_msg_time", "final_msg_time"},
	} {
		tri := [3]string{
			strings.TrimSpace(v[fields[0]]),
			strings.TrimSpace(v[fields[1]]),
			strings.TrimSpace(v[fields[2]]),
		}
		if tri[0] == "" && tri[1] == "" && tri[2] == "" {
			continue
		}
		messages[key] = tri
	}
	warnings := map[string]any{}
	if len(fractions) > 0 {
		warnings["fractions"] = fractions
	}
	if len(messages) > 0 {
		warnings["messages"] = messages
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}

	// The tier toggles are always meaningful (their columns are NOT NULL DEFAULT),
	// so they are always written.
	out["compact_tiers"] = []bool{
		v["compact_tier_warn"] == "true",
		v["compact_tier_escalate"] == "true",
		v["compact_tier_final"] == "true",
	}

	cc := map[string]any{"enabled": v["compact_enabled"] == "true"}
	if raw := strings.TrimSpace(v["compact_pressure_frac"]); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			cc["pressure_frac"] = f
		}
	}
	if raw := strings.TrimSpace(v["compact_recent_turns"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			cc["recent_turns"] = n
		}
	}
	out["context_compaction"] = cc

	mem := map[string]any{"enabled": v["memory_enabled"] == "true"}
	if raw := strings.TrimSpace(v["memory_digest_entries"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			mem["digest_entries"] = n
		}
	}
	out["memory"] = mem

	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseFractionTriple reads "a,b,c" into the [warn, escalate, final] triple an
// adapter expects. An unparseable or short triple yields ok=false, so the key is
// omitted rather than half-written.
func parseFractionTriple(raw string) ([3]float64, bool) {
	var out [3]float64
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return out, false
		}
		out[i] = f
	}
	return out, true
}

// validFractionTriple is the field validator for the ladder rows.
func validFractionTriple(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if _, ok := parseFractionTriple(raw); !ok {
		return errors.New("want three comma-separated fractions, e.g. 0.25,0.5,0.75")
	}
	return nil
}

// fracTriple renders three fractions as the editable "a,b,c" form, blanking the
// row when every value is zero (an unset ladder row reads as empty).
func fracTriple(a, b, c float64) string {
	if a == 0 && b == 0 && c == 0 {
		return ""
	}
	return trimZero(a) + "," + trimZero(b) + "," + trimZero(c)
}

func trimZero(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// fmtFrac renders a single fraction, blank when unset.
func fmtFrac(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// boolStr renders a bool for a KCheckbox field.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
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

// normalizeAuthMode maps a STORED auth mode onto one the form can offer.
//
// WHY THIS IS NOT JUST DEFENSIVE PADDING. A select whose Initial is not one of its Options fails
// validation with "unknown option", so it cannot be submitted at all — the operator would be unable
// to edit that provider by ANY route. A value outside {none, token} is either legacy or invented (the
// form used to offer `bearer` and `api_key`, neither of which the server accepts), so the only
// submit-able repair is a value the server accepts: the stored one is normalised to `none`, and the
// operator sees the choices that actually work.
func normalizeAuthMode(mode string) string {
	if providers.ValidateAuthMode(mode) == nil {
		return mode
	}
	return providers.AuthModeNone
}

// authModeOptions is the provider auth-mode choice, built from the SERVER'S OWN constants.
//
// It offered `none`, `bearer` and `api_key`. The server accepts exactly TWO values —
// providers.AuthModeNone ("none") and providers.AuthModeToken ("token") — and rejects anything else
// with `auth_mode must be "none" or "token"`. So BOTH extra options were unusable, and the one value
// every existing provider actually uses, `token` (the GUI offers only none|token, and the live table
// holds 2×token, 1×none and ZERO bearer/api_key), could not be chosen at all.
//
// That is the second half of the operator's "I tried adding a secret and a provider in the TUI and
// saved it, yet they were never actually created": picking `bearer` produced a provider the server
// refused. The options come from the exported constants rather than from string literals here, so the
// list CANNOT drift from what the server honours — the same reasoning as validSecretName calling the
// secrets store's validator.
func authModeOptions() []kit2.Option {
	return []kit2.Option{
		{Value: providers.AuthModeNone, Label: providers.AuthModeNone},
		// The GUI's own wording: choosing a token is what makes the plane write the tenant secret for
		// this provider, so the label says which secret appears and that it is automatic.
		{Value: providers.AuthModeToken, Label: "token (auto-writes CUSTOM_<REF>_API_KEY)"},
	}
}
