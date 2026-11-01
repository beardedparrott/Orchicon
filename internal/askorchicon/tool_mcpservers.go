package askorchicon

// MCP server management tools (ADR-0008, adapter-settings MCP management):
// thin wrappers over internal/mcpsettings so the Ask Orchicon / MCP surface
// mirrors the platform's MCPService. Credentials are write-only via the
// tenant secrets store (${SECRET_NAME} refs, never plaintext values).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func mcpSvc(pool *db.Pool, kek []byte) *mcpsettings.Service {
	return mcpsettings.New(pool, kek, nil)
}

// compactMCPServer is the value-less, compact row returned by the list tool.
// Plaintext credentials never appear — env/header values may be ${SECRET_NAME}
// references and has_secret_stored reports whether any required secret exists.
type compactMCPServer struct {
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	Transport       string                     `json:"transport"`
	Command         string                     `json:"command,omitempty"`
	Args            []string                   `json:"args,omitempty"`
	Env             map[string]string          `json:"env,omitempty"`
	URL             string                     `json:"url,omitempty"`
	Headers         map[string]string          `json:"headers,omitempty"`
	Enabled         bool                       `json:"enabled"`
	CatalogSlug     string                     `json:"catalog_slug,omitempty"`
	InstallStatus   string                     `json:"install_status"`
	InstallResult   *mcpsettings.InstallResult `json:"install_result,omitempty"`
	RequiredSecrets []string                   `json:"required_secrets,omitempty"`
	HasSecretStored bool                       `json:"has_secret_stored"`
	// OwnedBy says WHICH SCOPE supplies this definition — "conversation" or "project".
	//
	// WITHOUT IT THE ANSWER IS UNREADABLE. A conversation inherits its project's servers, so a list that mixes
	// both without saying which is which cannot be checked against what the turn actually gets, and "is this
	// mine or inherited?" is the question an operator asks when a tool is missing.
	OwnedBy string `json:"owned_by"`
}

func compactServer(e mcpsettings.Entry) compactMCPServer {
	res := e.InstallResult
	if res.Runtime == "" && res.Command == "" && !res.OK && res.Error == "" && res.InstalledAt == "" {
		res = mcpsettings.InstallResult{}
	}
	return compactMCPServer{
		ID:              e.ID,
		Name:            e.Name,
		Transport:       e.Transport,
		Command:         e.Command,
		Args:            e.Args,
		Env:             e.Env,
		URL:             e.URL,
		Headers:         e.Headers,
		Enabled:         e.Enabled,
		CatalogSlug:     e.CatalogSlug,
		InstallStatus:   e.InstallStatus,
		InstallResult:   &res,
		RequiredSecrets: e.RequiredSecrets,
		HasSecretStored: e.HasSecretStored,
		OwnedBy:         ownerOf(e),
	}
}

// ownerOf names the scope that supplies an entry. Entry's owner columns are mutually exclusive by construction
// (exactly one is set), so this is a total function rather than a guess.
func ownerOf(e mcpsettings.Entry) string {
	if strings.TrimSpace(e.ConversationID) != "" {
		return "conversation"
	}
	if strings.TrimSpace(e.ProjectID) != "" {
		return "project"
	}
	return "tenant"
}

func toolListMCPServers(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	// SCOPE-ADDRESSED, mirroring the MCPService RPC (MCPServerListRequest's
	// project_id / conversation_id → Service.ListForScope): pass an owner to
	// list that owner's definitions (project ∪ conversation), or neither to
	// list every definition in the tenant. Before this the tool called
	// ListForTenant unconditionally, so the Ask surface listed every owner's
	// definitions — a drift from the platform surface it mirrors.
	var params struct {
		ProjectID      string `json:"project_id"`
		ConversationID string `json:"conversation_id"`
	}
	if len(args) > 0 && string(args) != "null" {
		_ = json.Unmarshal(args, &params)
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)

	// A CONVERSATION'S SCOPE INCLUDES ITS PROJECT, AND LEAVING THAT OUT MADE THIS TOOL LIE.
	//
	// The TURN resolves project ∪ conversation (mcpsettings.Resolver.resolveConversation →
	// ListMCPServersByOwner with BOTH ids). This tool passed only the conversation, so the query's project arm
	// matched nothing and it answered with the conversation's own rows alone — which for a conversation that
	// inherits everything from its project is ZERO.
	//
	// REPORTED as the root cause of a whole wasted session: a model asked what MCP servers it had, was told
	// none, concluded its aseprite tools were unavailable, and spent the session driving the server over stdio
	// JSON-RPC through bash — flattening every image result to text, while the platform log for the same turns
	// recorded two registered servers. The tool contradicted the runtime, and the model believed the tool.
	//
	// So a conversation_id is EXPANDED to its project here, through the conversation's own row — the same
	// source the turn reads — rather than asking the caller to pass both and hoping it does.
	projectID := strings.TrimSpace(params.ProjectID)
	conversationID := strings.TrimSpace(params.ConversationID)
	if conversationID != "" && projectID == "" {
		if resolved, rerr := conversationProject(ctx, pool, tenantID, conversationID); rerr == nil {
			projectID = resolved
		}
		// A failed lookup is NOT fatal: the conversation's own rows are still the right answer for the part of
		// the scope that does not depend on the project.
	}
	list, err := svc.ListForScope(ctx, tenantID, projectID, conversationID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, e := range list {
		out = append(out, compactServer(e))
	}
	return json.Marshal(newCompactList(out, "get_mcp_server"))
}

// conversationProject reads a conversation's project id, or "" when it has none. Best-effort by design: the
// caller reports the conversation's own servers regardless.
func conversationProject(ctx context.Context, pool *db.Pool, tenantID, conversationID string) (string, error) {
	tx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	conv, err := db.GetConversation(ctx, tx.Tx, tenantID, conversationID)
	if err != nil {
		return "", err
	}
	return conv.ProjectID, nil
}

func toolGetMCPServer(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(params.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)
	e, err := svc.Get(ctx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(compactServer(e))
}

func toolCreateMCPServer(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name           string            `json:"name"`
		Transport      string            `json:"transport"`
		Command        string            `json:"command"`
		Args           []string          `json:"args"`
		Env            map[string]string `json:"env"`
		URL            string            `json:"url"`
		Headers        map[string]string `json:"headers"`
		Enabled        bool              `json:"enabled"`
		CatalogSlug    string            `json:"catalog_slug"`
		ProjectID      string            `json:"project_id"`
		ConversationID string            `json:"conversation_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)
	e, err := svc.Create(ctx, tenantID, mcpsettings.CreateInput{
		Name:           params.Name,
		ProjectID:      params.ProjectID,
		ConversationID: params.ConversationID,
		Transport:      params.Transport,
		Command:        params.Command,
		Args:           params.Args,
		Env:            params.Env,
		URL:            params.URL,
		Headers:        params.Headers,
		Enabled:        params.Enabled,
		CatalogSlug:    params.CatalogSlug,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(compactServer(e))
}

func toolUpdateMCPServer(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID             string            `json:"id"`
		Transport      *string           `json:"transport"`
		Command        *string           `json:"command"`
		Args           []string          `json:"args"`
		ReplaceArgs    *bool             `json:"replace_args"`
		Env            map[string]string `json:"env"`
		ReplaceEnv     *bool             `json:"replace_env"`
		URL            *string           `json:"url"`
		Headers        map[string]string `json:"headers"`
		ReplaceHeaders *bool             `json:"replace_headers"`
		Enabled        *bool             `json:"enabled"`
		CatalogSlug    *string           `json:"catalog_slug"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)
	in := mcpsettings.UpdateInput{ID: params.ID}
	in.Transport = params.Transport
	in.Command = params.Command
	if params.ReplaceArgs != nil && *params.ReplaceArgs {
		in.ReplaceArgs = true
		in.Args = params.Args
	}
	if params.ReplaceEnv != nil && *params.ReplaceEnv {
		in.ReplaceEnv = true
		in.Env = params.Env
	} else if params.Env != nil {
		in.Env = params.Env
	}
	in.URL = params.URL
	if params.ReplaceHeaders != nil && *params.ReplaceHeaders {
		in.ReplaceHeaders = true
		in.Headers = params.Headers
	} else if params.Headers != nil {
		in.Headers = params.Headers
	}
	in.Enabled = params.Enabled
	in.CatalogSlug = params.CatalogSlug
	e, err := svc.Update(ctx, tenantID, in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(compactServer(e))
}

func toolDeleteMCPServer(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)
	if err := svc.Delete(ctx, tenantID, params.ID); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"deleted": true, "id": params.ID})
}

func toolInstallMCPServer(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID     string `json:"id"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, nil)
	tenantID := tenant.FromContext(ctx)
	out, err := svc.Install(ctx, tenantID, mcpsettings.InstallInput{ID: params.ID, DryRun: params.DryRun})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"server":    compactServer(out.Entry),
		"would_run": out.WouldRun,
		"runtime":   out.Runtime,
		"command":   out.Command,
		"available": out.Available,
	})
}

func toolListMCPCatalog(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	list := mcpsettings.ListCatalog()
	out := make([]any, 0, len(list))
	for _, c := range list {
		out = append(out, c)
	}
	return json.Marshal(newCompactList(out, ""))
}

func toolSetMCPServerSecret(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, kek)
	tenantID := tenant.FromContext(ctx)
	secretName, err := svc.SetSecret(ctx, tenantID, params.ID, params.Name, params.Value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"stored": true, "secret_name": secretName})
}

func toolClearMCPServerSecret(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	svc := mcpSvc(pool, kek)
	tenantID := tenant.FromContext(ctx)
	if err := svc.ClearSecret(ctx, tenantID, params.ID, params.Name); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"cleared": true})
}
