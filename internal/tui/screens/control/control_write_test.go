package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// newWriteModel builds a sized Control screen with a capturing dock and no
// live plane; every rpc* thunk is replaced per test.
func newWriteModel(t *testing.T) (*Model, *fakeDock) {
	t.Helper()
	m := New(nil, nil)
	dock := &fakeDock{}
	m.SetShell(dock)
	m.SetSize(140, 40)
	return m, dock
}

// confirmAndRun resolves an action the way the shell's dialog path does: it
// opens the Confirm dialog when the action needs one, resolves it with the
// given choice, and returns the dispatched cmd.
func (m *Model) confirmAndRun(a kit2.Action, choice string) tea.Cmd {
	cmd := m.openActionsDialog(a)
	if !a.NeedsConfirm() {
		return cmd
	}
	m.Open = nil
	od := m.OnDialog
	m.OnDialog = nil
	if od == nil {
		return nil
	}
	return od(choice)
}

// runCmd runs a mutation cmd and returns its Result.
func runCmd(t *testing.T, cmd tea.Cmd) mutateResult {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil mutation cmd")
	}
	res, ok := cmd().(mutateResult)
	if !ok {
		t.Fatalf("cmd produced %T, want mutate.Result", cmd())
	}
	return res
}

// ---------------------------------------------------------------- sources

// Every GUI Control surface is registered as a real source (no silent
// read-only gap, /adapters and /admin included).
func TestControlRegistersEverySource(t *testing.T) {
	m := New(nil, nil)
	want := map[string]bool{
		"workers": false, "images": false, "secrets": false, "mcp": false,
		"providers": false, "webhooks": false, "adapters": false,
		"settings": false, "admin": false,
	}
	for _, s := range m.Base.SourcesForTest() {
		if _, ok := want[s.Name]; !ok {
			t.Fatalf("unexpected source %q", s.Name)
		}
		want[s.Name] = true
		if s.Fetch == nil {
			t.Fatalf("source %q has nil fetch", s.Name)
		}
	}
	for name, ok := range want {
		if !ok {
			t.Fatalf("missing source %q", name)
		}
	}
	for _, name := range []string{"adapters", "admin", "settings"} {
		if !m.SelectSource(name) {
			t.Fatalf("source %q is not selectable", name)
		}
	}
}

// ------------------------------------------------------------- settings

// Settings are editable + savable from the TUI, and invalid model refs are
// rejected INLINE before submit (the RPC never fires).
func TestSettingsEditSaveValidatesModelRefs(t *testing.T) {
	m, dock := newWriteModel(t)

	var got *apiv1.TenantSettings
	m.rpcUpdateSettings = func(ctx context.Context, s *apiv1.TenantSettings) error {
		got = s
		return nil
	}
	m.settings = &apiv1.TenantSettings{DefaultWorkerModel: "ollama/llama3", StallNudgeMax: 2}

	if !m.SelectSource("settings") {
		t.Fatal("no settings source")
	}
	// "e" opens the settings edit form.
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		t.Fatal("opening the settings form should not dispatch a cmd")
	}
	if !m.formOpen() {
		t.Fatal("e did not open the settings form")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open form must claim keys")
	}

	// Invalid model ref → rejected inline, no RPC.
	m.form.Set("default_worker_model", "notamodel")
	if _, err := m.form.Submit(); err == nil {
		t.Fatal("invalid model ref must be rejected before submit")
	}
	if m.form.Errors["default_worker_model"] == "" {
		t.Fatal("the invalid model ref must carry an inline field error")
	}
	if !strings.Contains(m.View(), "✗") {
		t.Fatal("the inline error is not rendered")
	}
	if got != nil {
		t.Fatal("the RPC fired despite the validation error")
	}

	// Valid values → the RPC fires with the edited fields.
	m.form.Set("default_worker_model", "ollama/llama3")
	m.form.Set("default_ask_model", "anthropic/claude-3")
	m.form.Set("stall_nudge_max", "4")
	m.form.Set("stall_no_progress_window_seconds", "120")
	m.form.Set("backup_schedule", "0 3 * * *")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("submit: %v (%v)", err, m.form.Errors)
	}
	res := runCmd(t, cmd)
	if res.Err != nil {
		t.Fatalf("settings save failed: %v", res.Err)
	}
	if got == nil {
		t.Fatal("UpdateSettings never fired")
	}
	if got.GetDefaultWorkerModel() != "ollama/llama3" || got.GetDefaultAskOrchiconModel() != "anthropic/claude-3" {
		t.Fatalf("model refs not sent: %+v", got)
	}
	if got.GetStallNudgeMax() != 4 || got.GetStallNoProgressWindowSeconds() != 120 {
		t.Fatalf("stall parameters not sent: %+v", got)
	}
	if got.GetBackupSchedule() != "0 3 * * *" {
		t.Fatalf("backup schedule not sent: %+v", got)
	}
	m.HandleMutation(res)
	if len(dock.notices) == 0 {
		t.Fatal("no success notice in the dock")
	}
}

// A malformed model ref (no provider prefix) is rejected too, while an empty
// value (leave unchanged) is accepted.
func TestModelRefValidation(t *testing.T) {
	if err := validModelRef("ollama/llama3"); err != nil {
		t.Fatalf("valid ref rejected: %v", err)
	}
	if err := validModelRef(""); err != nil {
		t.Fatalf("empty ref (unchanged) rejected: %v", err)
	}
	for _, bad := range []string{"llama3", "ollama/", "/llama3", "ollama llama3"} {
		if err := validModelRef(bad); err == nil {
			t.Fatalf("bad ref %q accepted", bad)
		}
	}
}

// ------------------------------------------------------------- webhooks

func TestWebhookCreateEditDeleteAndDeliveries(t *testing.T) {
	m, _ := newWriteModel(t)

	var created *apiv1.CreateSubscriptionRequest
	var edited *apiv1.UpdateSubscriptionRequest
	var deleted string
	m.rpcCreateWebhook = func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error {
		created = r
		return nil
	}
	m.rpcUpdateWebhook = func(ctx context.Context, r *apiv1.UpdateSubscriptionRequest) error {
		edited = r
		return nil
	}
	m.rpcDeleteWebhook = func(ctx context.Context, id string) error {
		deleted = id
		return nil
	}

	if !m.SelectSource("webhooks") {
		t.Fatal("no webhooks source")
	}

	// --- create ---
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("n did not open the create form")
	}
	m.form.Set("name", "ci-events")
	m.form.Set("target_url", "https://example.test/hook")
	m.form.Set("event_filter", "execution.completed")
	m.form.Set("scope", "tenant")
	m.form.Set("secret", "shhh")
	m.form.Set("max_retries", "5")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("create submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("create failed: %v", res.Err)
	}
	if created == nil || created.GetName() != "ci-events" || created.GetTargetUrl() != "https://example.test/hook" ||
		created.GetMaxRetries() != 5 || created.GetSecret() != "shhh" {
		t.Fatalf("create RPC payload wrong: %+v", created)
	}
	if strings.Contains(m.View(), "shhh") {
		t.Fatal("the signing secret was rendered")
	}

	m.form = nil // the screen clears the form on submit (mirrored here)

	// --- edit (prefilled from the cached subscription) ---
	m.LoadItems("webhooks", []kit2.Item{{ID: "w1", Title: "deploy", Meta: "active"}}, "")
	m.SelectSource("webhooks")
	m.webhooks["w1"] = &apiv1.WebhookSubscription{
		Id: "w1", Name: "deploy", TargetUrl: "https://old.test/hook", Status: "active", MaxRetries: 3,
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.formOpen() {
		t.Fatal("e did not open the edit form")
	}
	if m.form.Values["target_url"] != "https://old.test/hook" {
		t.Fatalf("edit form not prefilled: %q", m.form.Values["target_url"])
	}
	m.form.Set("target_url", "https://new.test/hook")
	m.form.Set("event_filter", "project.*")
	m.form.Set("status", "paused")
	m.form.Set("max_retries", "7")
	cmd, err = m.form.Submit()
	if err != nil {
		t.Fatalf("edit submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("edit failed: %v", res.Err)
	}
	if edited == nil || edited.GetId() != "w1" || edited.GetTargetUrl() != "https://new.test/hook" ||
		edited.GetStatus() != "paused" || edited.GetMaxRetries() != 7 {
		t.Fatalf("edit RPC payload wrong: %+v", edited)
	}

	// --- delete (Confirm-gated) ---
	del, ok := m.actionForKey("x")
	if !ok {
		t.Fatal("no delete action on webhooks")
	}
	if del.Label != "delete" || !del.NeedsConfirm() {
		t.Fatalf("delete action must be Confirm-gated: %+v", del)
	}
	m.openActionsDialog(del)
	if m.Open == nil {
		t.Fatal("confirm dialog did not open")
	}
	if res := runCmd(t, m.confirmAndRun(del, "delete")); res.Err != nil {
		t.Fatalf("delete failed: %v", res.Err)
	}
	if deleted != "w1" {
		t.Fatalf("delete RPC did not fire for w1: %q", deleted)
	}

	// --- deliveries view (rides the webhook detail body) ---
	m.rpcListDeliveries = func(ctx context.Context, subID string) ([]*apiv1.WebhookDelivery, error) {
		if subID != "w2" {
			t.Fatalf("deliveries queried for %q, want w2", subID)
		}
		return []*apiv1.WebhookDelivery{
			{Id: "d1", Status: "delivered", Attempt: 1, StatusCode: 200, EventType: "execution.completed"},
			{Id: "d2", Status: "dead_letter", Attempt: 5, StatusCode: 500, EventType: "execution.failed", Error: "boom"},
		}, nil
	}
	m.webhooks["w2"] = &apiv1.WebhookSubscription{Id: "w2", Name: "hook2", TargetUrl: "https://x.test"}
	_, _, body, err := m.detail(context.Background(), "webhooks", "w2")
	if err != nil {
		t.Fatalf("webhook detail: %v", err)
	}
	if !strings.Contains(body, "d1") || !strings.Contains(body, "delivered") || !strings.Contains(body, "d2") ||
		!strings.Contains(body, "dead_letter") {
		t.Fatalf("deliveries not rendered in the detail: %q", body)
	}
}

// ------------------------------------------------------------- adapters

func TestAdaptersSourceDetailAndToggle(t *testing.T) {
	m, _ := newWriteModel(t)
	m.rpcListAdapters = func(ctx context.Context, kind string) ([]*apiv1.RuntimeAdapter, error) {
		return []*apiv1.RuntimeAdapter{{
			Id: "a1", Kind: "opencode", Version: "1.2.3", Endpoint: "grpc://x",
			Status: "ready", Capabilities: `{"exec":true}`,
		}}, nil
	}
	items, _, err := m.fetchAdapters(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch adapters: %v", err)
	}
	if len(items) != 1 || items[0].ID != "a1" {
		t.Fatalf("adapter source not populated: %+v", items)
	}
	m.LoadItems("adapters", items, "")
	if !m.SelectSource("adapters") {
		t.Fatal("no adapters source")
	}

	// detail exposes the adapter fields + the capability manifest body.
	title, fields, body, err := m.detail(context.Background(), "adapters", "a1")
	if err != nil {
		t.Fatalf("adapter detail: %v", err)
	}
	if !strings.Contains(title, "opencode") || !strings.Contains(body, "exec") {
		t.Fatalf("adapter detail = %q / %q", title, body)
	}
	if !hasField(fields, "endpoint", "grpc://x") {
		t.Fatalf("adapter fields missing endpoint: %+v", fields)
	}

	// disable is Confirm-gated and flips the local dispatch state.
	dis, ok := m.actionForKey("t")
	if !ok || dis.Label != "disable" {
		t.Fatalf("adapter toggle action = %+v (ok=%v)", dis, ok)
	}
	if !dis.NeedsConfirm() {
		t.Fatal("disabling an adapter must be Confirm-gated")
	}
	// The local toggle still rides the mutation layer (progress + dock
	// feedback) but its Do is nil — the registry API has no adapter write.
	if res := runCmd(t, m.confirmAndRun(dis, "disable")); res.Err != nil {
		t.Fatalf("adapter disable failed: %v", res.Err)
	}
	if !m.adapterDisabled["a1"] {
		t.Fatal("adapter not marked disabled")
	}
	if it, ok := m.ActiveItem(); !ok || !strings.Contains(it.Meta, "disabled (local)") {
		t.Fatalf("row not marked disabled: %+v", it)
	}
	// re-enable restores the row.
	en, ok := m.actionForKey("t")
	if !ok || en.Label != "enable" {
		t.Fatalf("adapter re-enable action = %+v (ok=%v)", en, ok)
	}
	if res := runCmd(t, m.confirmAndRun(en, "enable")); res.Err != nil {
		t.Fatalf("adapter enable failed: %v", res.Err)
	}
	if m.adapterDisabled["a1"] {
		t.Fatal("adapter still disabled after enable")
	}
	// detail reports the dispatch state.
	_, fields, _, _ = m.detail(context.Background(), "adapters", "a1")
	if !hasField(fields, "dispatch", "enabled") {
		t.Fatalf("adapter detail dispatch state wrong: %+v", fields)
	}
}

// ------------------------------------------------------------------- mcp

func TestMCPCRUDToggleSecretInstallAndDelete(t *testing.T) {
	m, _ := newWriteModel(t)

	var created *apiv1.MCPServerCreateRequest
	var updated *apiv1.MCPServerUpdateRequest
	var installed, deleted string
	var setID, setName, setVal, clearID, clearName string
	m.rpcCreateMCP = func(ctx context.Context, r *apiv1.MCPServerCreateRequest) error { created = r; return nil }
	m.rpcUpdateMCP = func(ctx context.Context, r *apiv1.MCPServerUpdateRequest) error { updated = r; return nil }
	m.rpcInstallMCP = func(ctx context.Context, id string) error { installed = id; return nil }
	m.rpcDeleteMCP = func(ctx context.Context, id string) error { deleted = id; return nil }
	m.rpcSetMCPSecret = func(ctx context.Context, id, name, value string) error {
		setID, setName, setVal = id, name, value
		return nil
	}
	m.rpcClearMCPSecret = func(ctx context.Context, id, name string) error { clearID, clearName = id, name; return nil }

	m.SelectSource("mcp")

	// create
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("n did not open the MCP create form")
	}
	m.form.Set("name", "github-mcp")
	m.form.Set("transport", "stdio")
	m.form.Set("command", "npx")
	m.form.Set("args", "-y @mcp/server")
	m.form.Set("enabled", "true")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("mcp create submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("mcp create failed: %v", res.Err)
	}
	if created == nil || created.GetName() != "github-mcp" || created.GetCommand() != "npx" ||
		!created.GetEnabled() || created.GetTransport() != apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO {
		t.Fatalf("mcp create payload wrong: %+v", created)
	}
	if len(created.GetArgs()) != 2 {
		t.Fatalf("mcp args not split: %+v", created.GetArgs())
	}

	m.form = nil // the screen clears the form on submit

	// seed the pane with a cached server
	m.LoadItems("mcp", []kit2.Item{{ID: "m1", Title: "github", Meta: "enabled"}}, "")
	m.SelectSource("mcp")
	m.mcpServers["m1"] = &apiv1.MCPServer{
		Id: "m1", Name: "github", Enabled: true,
		Transport:       apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
		RequiredSecrets: []string{"GITHUB_TOKEN"},
	}

	// edit
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.formOpen() {
		t.Fatal("e did not open the MCP edit form")
	}
	m.form.Set("command", "uvx")
	m.form.Set("enabled", "false")
	cmd, err = m.form.Submit()
	if err != nil {
		t.Fatalf("mcp edit submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("mcp edit failed: %v", res.Err)
	}
	if updated == nil || updated.GetId() != "m1" || updated.GetCommand() != "uvx" || updated.GetEnabled() {
		t.Fatalf("mcp edit payload wrong: %+v", updated)
	}

	m.form = nil // the screen clears the form on submit

	// toggle enabled (currently enabled → disable)
	updated = nil
	tog, ok := m.actionForKey("t")
	if !ok || tog.Label != "disable" || !tog.NeedsConfirm() {
		t.Fatalf("mcp toggle action = %+v (ok=%v)", tog, ok)
	}
	if res := runCmd(t, m.confirmAndRun(tog, "disable")); res.Err != nil {
		t.Fatalf("mcp disable failed: %v", res.Err)
	}
	if updated == nil || updated.GetEnabled() {
		t.Fatalf("mcp disable payload wrong: %+v", updated)
	}

	// install
	inst, ok := m.actionForKey("i")
	if !ok {
		t.Fatal("no install action for a stdio MCP server")
	}
	if res := runCmd(t, m.openActionsDialog(inst)); res.Err != nil {
		t.Fatalf("install failed: %v", res.Err)
	}
	if installed != "m1" {
		t.Fatalf("install RPC did not fire: %q", installed)
	}

	// store a credential (value masked, written once)
	f := m.secretFormForSource()
	if f == nil {
		t.Fatal("no MCP credential form")
	}
	if f.Values["name"] != "GITHUB_TOKEN" {
		t.Fatalf("credential key not prefilled: %q", f.Values["name"])
	}
	f.Set("value", "ghp_supersecret")
	cmd, err = f.Submit()
	if err != nil {
		t.Fatalf("mcp secret submit: %v (%v)", err, f.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("set credential failed: %v", res.Err)
	}
	if setID != "m1" || setName != "GITHUB_TOKEN" || setVal != "ghp_supersecret" {
		t.Fatalf("set credential payload wrong: %q %q %q", setID, setName, setVal)
	}

	// clear the credential (Confirm-gated)
	clr, ok := m.actionForKey("c")
	if !ok || !clr.NeedsConfirm() {
		t.Fatalf("no clear-credential action: %+v (ok=%v)", clr, ok)
	}
	if res := runCmd(t, m.confirmAndRun(clr, "clear credential")); res.Err != nil {
		t.Fatalf("clear credential failed: %v", res.Err)
	}
	if clearID != "m1" || clearName != "GITHUB_TOKEN" {
		t.Fatalf("clear credential payload wrong: %q %q", clearID, clearName)
	}

	// delete (Confirm-gated)
	del, ok := m.actionForKey("x")
	if !ok || !del.NeedsConfirm() {
		t.Fatalf("no delete action for MCP: %+v", del)
	}
	if res := runCmd(t, m.confirmAndRun(del, "delete")); res.Err != nil {
		t.Fatalf("mcp delete failed: %v", res.Err)
	}
	if deleted != "m1" {
		t.Fatalf("mcp delete RPC did not fire: %q", deleted)
	}
}

// ------------------------------------------------------------- providers

func TestProvidersCRUDToggleTokenAndDelete(t *testing.T) {
	m, _ := newWriteModel(t)

	var created *apiv1.ProviderCreateCustomRequest
	var customUpdated *apiv1.ProviderUpdateCustomRequest
	var deleted string
	var enabledCalls [2]bool
	var enabledN int
	var tokenID, tokenVal, clearedID string
	var settings *apiv1.ProviderUpdateSettingsRequest
	m.rpcCreateProvider = func(ctx context.Context, r *apiv1.ProviderCreateCustomRequest) error { created = r; return nil }
	m.rpcUpdateProvider = func(ctx context.Context, r *apiv1.ProviderUpdateCustomRequest) error {
		customUpdated = r
		return nil
	}
	m.rpcDeleteProvider = func(ctx context.Context, id string) error { deleted = id; return nil }
	m.rpcProviderEnabled = func(ctx context.Context, id string, enabled bool) error {
		enabledCalls[enabledN] = enabled
		enabledN++
		return nil
	}
	m.rpcProviderSettings = func(ctx context.Context, id string, enabled bool, ov string) error {
		settings = &apiv1.ProviderUpdateSettingsRequest{ProviderId: id, Enabled: &enabled, BaseUrlOverride: &ov}
		return nil
	}
	m.rpcSetProviderToken = func(ctx context.Context, id, token string) error { tokenID, tokenVal = id, token; return nil }
	m.rpcClearProviderToken = func(ctx context.Context, id string) error { clearedID = id; return nil }

	m.SelectSource("providers")

	// create custom provider
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("n did not open the provider create form")
	}
	m.form.Set("display_name", "Local Ollama")
	m.form.Set("ref_id", "local-ollama")
	m.form.Set("base_url", "http://127.0.0.1:11434")
	m.form.Set("auth_mode", "none")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("provider create submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("provider create failed: %v", res.Err)
	}
	if created == nil || created.GetRefId() != "local-ollama" || created.GetBaseUrl() != "http://127.0.0.1:11434" {
		t.Fatalf("provider create payload wrong: %+v", created)
	}

	m.form = nil // the screen clears the form on submit

	// seed a cached custom provider with a stored token
	m.LoadItems("providers", []kit2.Item{{ID: "p1", Title: "Ollama", Meta: "ollama enabled · custom · token"}}, "")
	m.SelectSource("providers")
	m.providers["p1"] = &apiv1.ProviderEntry{
		Id: "p1", DisplayName: "Ollama", Kind: "openai", Enabled: true,
		IsCustom: true, HasTokenStored: true, BaseUrl: "http://127.0.0.1:11434", AuthMode: "bearer",
	}

	// enable/disable toggle
	tog, ok := m.actionForKey("t")
	if !ok || tog.Label != "disable" || !tog.NeedsConfirm() {
		t.Fatalf("provider toggle action = %+v (ok=%v)", tog, ok)
	}
	if res := runCmd(t, m.confirmAndRun(tog, "disable")); res.Err != nil {
		t.Fatalf("provider disable failed: %v", res.Err)
	}
	if enabledN != 1 || enabledCalls[0] {
		t.Fatalf("provider disable RPC wrong: %v", enabledCalls)
	}

	// edit (settings + custom paths)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.formOpen() {
		t.Fatal("e did not open the provider edit form")
	}
	m.form.Set("enabled", "true")
	m.form.Set("base_url_override", "http://127.0.0.1:9999")
	m.form.Set("display_name", "Ollama Local")
	cmd, err = m.form.Submit()
	if err != nil {
		t.Fatalf("provider edit submit: %v (%v)", err, m.form.Errors)
	}
	if cmd == nil {
		t.Fatal("provider edit produced no cmd")
	}
	// the batch holds two mutation cmds
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected a batched cmd, got %T", cmd())
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		if res, ok := c().(mutateResult); ok {
			m.HandleMutation(res)
		}
	}
	if settings == nil || settings.GetProviderId() != "p1" || !settings.GetEnabled() ||
		settings.GetBaseUrlOverride() != "http://127.0.0.1:9999" {
		t.Fatalf("provider settings payload wrong: %+v", settings)
	}
	if customUpdated == nil || customUpdated.GetRefId() != "p1" || customUpdated.GetDisplayName() != "Ollama Local" {
		t.Fatalf("custom provider update payload wrong: %+v", customUpdated)
	}

	m.form = nil // the screen clears the form on submit

	// store a token (masked)
	f := m.secretFormForSource()
	if f == nil {
		t.Fatal("no provider token form")
	}
	f.Set("token", "sk-secret-token")
	cmd, err = f.Submit()
	if err != nil {
		t.Fatalf("token submit: %v (%v)", err, f.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("set token failed: %v", res.Err)
	}
	if tokenID != "p1" || tokenVal != "sk-secret-token" {
		t.Fatalf("set token payload wrong: %q %q", tokenID, tokenVal)
	}
	if strings.Contains(m.View(), "sk-secret-token") {
		t.Fatal("the token was rendered")
	}

	// clear the token (Confirm-gated)
	clr, ok := m.actionForKey("c")
	if !ok || !clr.NeedsConfirm() {
		t.Fatalf("no clear-token action: %+v (ok=%v)", clr, ok)
	}
	if res := runCmd(t, m.confirmAndRun(clr, "clear token")); res.Err != nil {
		t.Fatalf("clear token failed: %v", res.Err)
	}
	if clearedID != "p1" {
		t.Fatalf("clear token RPC did not fire: %q", clearedID)
	}

	// delete (custom only, Confirm-gated)
	del, ok := m.actionForKey("x")
	if !ok || !del.NeedsConfirm() {
		t.Fatalf("no delete action for a custom provider: %+v", del)
	}
	if res := runCmd(t, m.confirmAndRun(del, "delete")); res.Err != nil {
		t.Fatalf("provider delete failed: %v", res.Err)
	}
	if deleted != "p1" {
		t.Fatalf("provider delete RPC did not fire: %q", deleted)
	}
}

// A built-in provider has no delete action (nothing to delete).
func TestBuiltinProviderHasNoDelete(t *testing.T) {
	m, _ := newWriteModel(t)
	m.LoadItems("providers", []kit2.Item{{ID: "p2", Title: "OpenAI", Meta: "openai enabled"}}, "")
	m.SelectSource("providers")
	m.providers["p2"] = &apiv1.ProviderEntry{Id: "p2", DisplayName: "OpenAI", Enabled: true}
	if _, ok := m.actionForKey("x"); ok {
		t.Fatal("built-in providers must not offer delete")
	}
}

// --------------------------------------------------------------- secrets

func TestSecretsNamesOnlyCreateUpdateDelete(t *testing.T) {
	m, _ := newWriteModel(t)

	var created *apiv1.CreateSecretRequest
	var updated *apiv1.UpdateSecretRequest
	var deleted string
	m.rpcCreateSecret = func(ctx context.Context, r *apiv1.CreateSecretRequest) error { created = r; return nil }
	m.rpcUpdateSecret = func(ctx context.Context, r *apiv1.UpdateSecretRequest) error { updated = r; return nil }
	m.rpcDeleteSecret = func(ctx context.Context, id string) error { deleted = id; return nil }

	// list rows carry names/metadata only
	m.LoadItems("secrets", []kit2.Item{{ID: "s1", Title: "GITHUB_TOKEN", Meta: "value hidden"}}, "")
	m.SelectSource("secrets")
	if it, ok := m.ActiveItem(); !ok || it.Title != "GITHUB_TOKEN" || !strings.Contains(it.Meta, "hidden") {
		t.Fatalf("secret row = %+v", it)
	}
	// the detail never renders a value
	_, fields, _, err := m.detail(context.Background(), "secrets", "s1")
	if err != nil {
		t.Fatalf("secret detail: %v", err)
	}
	if !hasField(fields, "note", "orch never reads a secret value — write it on create, rotate it blind") {
		t.Fatalf("secret detail must explain the value is never read: %+v", fields)
	}

	// create
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("n did not open the secret create form")
	}
	m.form.Set("name", "NEW_TOKEN")
	m.form.Set("value", "top-secret")
	m.form.Set("description", "ci")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("secret create submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("secret create failed: %v", res.Err)
	}
	if created == nil || created.GetName() != "NEW_TOKEN" || created.GetValue() != "top-secret" {
		t.Fatalf("secret create payload wrong: %+v", created)
	}
	if strings.Contains(m.View(), "top-secret") {
		t.Fatal("the secret value was rendered")
	}

	m.form = nil // the screen clears the form on submit

	// update (rotate blind)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.formOpen() {
		t.Fatal("e did not open the rotate form")
	}
	m.form.Set("value", "rotated")
	m.form.Set("description", "ci v2")
	cmd, err = m.form.Submit()
	if err != nil {
		t.Fatalf("secret update submit: %v (%v)", err, m.form.Errors)
	}
	if res := runCmd(t, cmd); res.Err != nil {
		t.Fatalf("secret update failed: %v", res.Err)
	}
	if updated == nil || updated.GetId() != "s1" || updated.GetValue() != "rotated" ||
		updated.GetDescription() != "ci v2" {
		t.Fatalf("secret update payload wrong: %+v", updated)
	}

	// delete (Confirm-gated)
	del, ok := m.actionForKey("x")
	if !ok || !del.NeedsConfirm() {
		t.Fatalf("no delete action for secrets: %+v", del)
	}
	if res := runCmd(t, m.confirmAndRun(del, "delete")); res.Err != nil {
		t.Fatalf("secret delete failed: %v", res.Err)
	}
	if deleted != "s1" {
		t.Fatalf("secret delete RPC did not fire: %q", deleted)
	}
}

// No code path in the control screen reads a secret value (GetSecret is never
// called; only the write-once create/update path carries a value).
func TestControlNeverReadsSecretValue(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "screen.go"))
	if err != nil {
		t.Fatalf("read screen.go: %v", err)
	}
	for _, forbidden := range []string{"GetSecret(", "Secrets.GetSecret"} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("control screen reads a secret value (%q)", forbidden)
		}
	}
}

// ----------------------------------------------------------------- admin

func TestAdminExplicitPermissionState(t *testing.T) {
	m, _ := newWriteModel(t)

	// A refusal renders an EXPLICIT permission state, never a silent pane.
	m.rpcAdminProbe = func(ctx context.Context) error {
		return connect.NewError(connect.CodePermissionDenied, errors.New("permission_denied: missing auth:read"))
	}
	items, _, err := m.fetchAdmin(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchAdmin must not surface the refusal as a fetch error: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("admin pane rendered nothing (silent empty)")
	}
	if items[0].ID != "admin-access" || !strings.Contains(items[0].Meta, "permission required") ||
		!strings.Contains(items[0].Title, "permission") {
		t.Fatalf("admin permission state wrong: %+v", items[0])
	}
	m.LoadItems("admin", items, "")
	if !m.SelectSource("admin") {
		t.Fatal("no admin source")
	}
	// The pane renders the state (never a silent empty pane).
	if !strings.Contains(m.View(), "permission") {
		t.Fatal("the explicit permission state is not rendered")
	}
	_, fields, _, _ := m.detail(context.Background(), "admin", "admin-access")
	if !hasField(fields, "state", "permission required — this credential is not a tenant admin") {
		t.Fatalf("admin detail state wrong: %+v", fields)
	}

	// A granted probe reports granted.
	m.rpcAdminProbe = func(ctx context.Context) error { return nil }
	items, _, _ = m.fetchAdmin(context.Background(), "")
	if !strings.Contains(items[0].Meta, "granted") {
		t.Fatalf("granted admin state wrong: %+v", items[0])
	}
}

// ------------------------------------------------------------- chording

// A pane with no creation surface opens no form on "n"; the modal keys stay
// claimed while a Confirm dialog is open.
func TestControlKeyChordsAndModality(t *testing.T) {
	m, _ := newWriteModel(t)
	m.SelectSource("workers")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.formOpen() {
		t.Fatal("workers has no create surface")
	}
	m.SelectSource("settings")
	if m.newFormForSource() != nil {
		t.Fatal("settings has no create surface")
	}
	if m.editFormForSource() == nil {
		t.Fatal("settings must offer an edit form")
	}
	// ClaimsKeys is false with no modal open.
	if m.ClaimsKeys() {
		t.Fatal("no modal is open")
	}
	m.Open = kit2.Confirm("delete", "sure?", "delete")
	if !m.ClaimsKeys() {
		t.Fatal("a Confirm dialog must claim keys")
	}
}

func hasField(fields []kit2.Field, key, want string) bool {
	for _, f := range fields {
		if f.Key == key {
			return strings.Contains(f.Value, want)
		}
	}
	return false
}
