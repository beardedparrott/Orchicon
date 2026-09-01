package mcpsettings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the MCPService Connect handler over Service.
type Handler struct {
	svc *Service
	log *slog.Logger
	apiv1connect.UnimplementedMCPServiceHandler
}

var _ apiv1connect.MCPServiceHandler = (*Handler)(nil)

// NewHandler wires the RPC surface.
func NewHandler(pool *db.Pool, kek []byte, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: New(pool, kek, log), log: log}
}

// Service exposes the core service (tests, wiring seams).
func (h *Handler) Service() *Service { return h.svc }

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// mapErr translates service errors into connect codes (typed, never
// error-string sniffing for the sentinel paths).
func (h *Handler) mapErr(err error) error {
	var refErr *ReferencedError
	if errors.As(err, &refErr) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if errors.Is(err, errInvalidArgument) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (h *Handler) ListMCPServers(ctx context.Context, req *connect.Request[apiv1.MCPServerListRequest]) (*connect.Response[apiv1.MCPServerListResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	entries, err := h.svc.ListForTenant(ctx, tenantID)
	if err != nil {
		return nil, h.mapErr(err)
	}
	out := &apiv1.MCPServerListResponse{}
	for _, e := range entries {
		out.Servers = append(out.Servers, entryToProto(e))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) GetMCPServer(ctx context.Context, req *connect.Request[apiv1.MCPServerGetRequest]) (*connect.Response[apiv1.MCPServerGetResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	e, err := h.svc.Get(ctx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerGetResponse{Server: entryToProto(e)}), nil
}

func (h *Handler) CreateMCPServer(ctx context.Context, req *connect.Request[apiv1.MCPServerCreateRequest]) (*connect.Response[apiv1.MCPServerCreateResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	msg := req.Msg
	e, err := h.svc.Create(ctx, tenantID, CreateInput{
		Name:        msg.Name,
		Transport:   transportFromProto(msg.Transport),
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		URL:         msg.Url,
		Headers:     msg.Headers,
		Enabled:     msg.Enabled,
		CatalogSlug: msg.CatalogSlug,
	})
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerCreateResponse{Server: entryToProto(e)}), nil
}

func (h *Handler) UpdateMCPServer(ctx context.Context, req *connect.Request[apiv1.MCPServerUpdateRequest]) (*connect.Response[apiv1.MCPServerUpdateResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	msg := req.Msg
	in := UpdateInput{ID: msg.Id}
	if msg.Name != nil {
		in.Name = msg.Name
	}
	if msg.Transport != nil {
		t := transportFromProto(*msg.Transport)
		in.Transport = &t
	}
	if msg.Command != nil {
		in.Command = msg.Command
	}
	if msg.ReplaceArgs != nil && *msg.ReplaceArgs {
		in.Args = msg.Args
		in.ReplaceArgs = true
	}
	if msg.ReplaceEnv != nil && *msg.ReplaceEnv {
		in.Env = msg.Env
		in.ReplaceEnv = true
	}
	if msg.Url != nil {
		in.URL = msg.Url
	}
	if msg.ReplaceHeaders != nil && *msg.ReplaceHeaders {
		in.Headers = msg.Headers
		in.ReplaceHeaders = true
	}
	if msg.Enabled != nil {
		in.Enabled = msg.Enabled
	}
	if msg.CatalogSlug != nil {
		in.CatalogSlug = msg.CatalogSlug
	}
	e, err := h.svc.Update(ctx, tenantID, in)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerUpdateResponse{Server: entryToProto(e)}), nil
}

func (h *Handler) DeleteMCPServer(ctx context.Context, req *connect.Request[apiv1.MCPServerDeleteRequest]) (*connect.Response[apiv1.MCPServerDeleteResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	err = h.svc.Delete(ctx, tenantID, req.Msg.Id)
	if err != nil {
		var refErr *ReferencedError
		if errors.As(err, &refErr) {
			var parts []string
			for _, p := range refErr.Projects {
				parts = append(parts, "project "+p.ProjectName)
			}
			for _, w := range refErr.Workers {
				parts = append(parts, "worker "+w.WorkerName)
			}
			if refErr.InTenantDefault {
				parts = append(parts, "the tenant default MCP set")
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("this MCP server is still referenced by %s — remove the references first", strings.Join(parts, ", ")))
		}
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerDeleteResponse{}), nil
}

func (h *Handler) ListMCPCatalog(ctx context.Context, req *connect.Request[apiv1.MCPCatalogListRequest]) (*connect.Response[apiv1.MCPCatalogListResponse], error) {
	out := &apiv1.MCPCatalogListResponse{}
	for _, c := range ListCatalog() {
		out.Entries = append(out.Entries, catalogEntryToProto(c))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) PrefillMCPCatalogEntry(ctx context.Context, req *connect.Request[apiv1.MCPCatalogPrefillRequest]) (*connect.Response[apiv1.MCPCatalogPrefillResponse], error) {
	c, ok := catalogBySlug(strings.TrimSpace(req.Msg.Slug))
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog entry %q not found", req.Msg.Slug))
	}
	prefill := &apiv1.MCPServerCreateRequest{
		Name:        c.DisplayName,
		Transport:   transportToProto(c.Transport),
		Command:     c.DefaultCommand,
		Args:        append([]string{}, c.DefaultArgs...),
		Url:         c.DefaultURL,
		Enabled:     true,
		CatalogSlug: c.Slug,
	}
	prefill.Env = map[string]string{}
	for _, ev := range c.DefaultEnv {
		if !ev.Secret && ev.Default != "" {
			prefill.Env[ev.Name] = ev.Default
		}
	}
	prefill.Headers = map[string]string{}
	for _, hv := range c.DefaultHeaders {
		if !hv.Secret && hv.Default != "" {
			prefill.Headers[hv.Name] = hv.Default
		}
	}
	return connect.NewResponse(&apiv1.MCPCatalogPrefillResponse{Entry: catalogEntryToProto(c), Prefill: prefill}), nil
}

func (h *Handler) InstallMCPRuntime(ctx context.Context, req *connect.Request[apiv1.MCPServerInstallRequest]) (*connect.Response[apiv1.MCPServerInstallResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	out, err := h.svc.Install(ctx, tenantID, InstallInput{ID: req.Msg.Id, DryRun: req.Msg.DryRun})
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerInstallResponse{
		Server:    entryToProto(out.Entry),
		WouldRun:  out.WouldRun,
		Runtime:   out.Runtime,
		Command:   out.Command,
		Available: out.Available,
	}), nil
}

func (h *Handler) DetectMCPRuntimes(ctx context.Context, req *connect.Request[apiv1.MCPRuntimeDetectRequest]) (*connect.Response[apiv1.MCPRuntimeDetectResponse], error) {
	return connect.NewResponse(&apiv1.MCPRuntimeDetectResponse{Available: DetectRuntimes()}), nil
}

func (h *Handler) SetMCPServerSecret(ctx context.Context, req *connect.Request[apiv1.MCPServerSetSecretRequest]) (*connect.Response[apiv1.MCPServerSetSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	name, err := h.svc.SetSecret(ctx, tenantID, req.Msg.Id, req.Msg.Name, req.Msg.Value)
	if err != nil {
		return nil, h.mapErr(err)
	}
	hasStored := false
	if e, gerr := h.svc.Get(ctx, tenantID, req.Msg.Id); gerr == nil {
		hasStored = e.HasSecretStored
	}
	return connect.NewResponse(&apiv1.MCPServerSetSecretResponse{SecretName: name, HasSecretStored: hasStored}), nil
}

func (h *Handler) ClearMCPServerSecret(ctx context.Context, req *connect.Request[apiv1.MCPServerClearSecretRequest]) (*connect.Response[apiv1.MCPServerClearSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err := h.svc.ClearSecret(ctx, tenantID, req.Msg.Id, req.Msg.Name); err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.MCPServerClearSecretResponse{}), nil
}

func (h *Handler) SetProjectMCPServers(ctx context.Context, req *connect.Request[apiv1.ProjectMCPServersSetRequest]) (*connect.Response[apiv1.ProjectMCPServersSetResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err := h.svc.SetProjectSelection(ctx, tenantID, req.Msg.ProjectId, req.Msg.McpServerIds); err != nil {
		return nil, h.mapErr(err)
	}
	ids, err := h.svc.GetProjectSelection(ctx, tenantID, req.Msg.ProjectId)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProjectMCPServersSetResponse{McpServerIds: ids}), nil
}

func (h *Handler) GetProjectMCPServers(ctx context.Context, req *connect.Request[apiv1.ProjectMCPServersGetRequest]) (*connect.Response[apiv1.ProjectMCPServersGetResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	ids, err := h.svc.GetProjectSelection(ctx, tenantID, req.Msg.ProjectId)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProjectMCPServersGetResponse{McpServerIds: ids}), nil
}

func (h *Handler) SetTenantDefaultMCPServers(ctx context.Context, req *connect.Request[apiv1.TenantDefaultMCPServersSetRequest]) (*connect.Response[apiv1.TenantDefaultMCPServersSetResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err := h.svc.SetTenantDefaultSelection(ctx, tenantID, req.Msg.McpServerIds); err != nil {
		return nil, h.mapErr(err)
	}
	ids, err := h.svc.GetTenantDefaultSelection(ctx, tenantID)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.TenantDefaultMCPServersSetResponse{McpServerIds: ids}), nil
}

func (h *Handler) GetTenantDefaultMCPServers(ctx context.Context, req *connect.Request[apiv1.TenantDefaultMCPServersGetRequest]) (*connect.Response[apiv1.TenantDefaultMCPServersGetResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	ids, err := h.svc.GetTenantDefaultSelection(ctx, tenantID)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.TenantDefaultMCPServersGetResponse{McpServerIds: ids}), nil
}

func transportFromProto(t apiv1.MCPServerTransport) string {
	switch t {
	case apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP:
		return TransportStreamable
	default:
		return TransportStdio
	}
}

func transportToProto(t string) apiv1.MCPServerTransport {
	if t == TransportStreamable {
		return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP
	}
	return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO
}

func installStatusToProto(s string) apiv1.MCPInstallStatus {
	switch s {
	case InstallNotInstalled:
		return apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_NOT_INSTALLED
	case InstallInstalling:
		return apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_INSTALLING
	case InstallInstalled:
		return apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_INSTALLED
	case InstallFailed:
		return apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_FAILED
	default:
		return apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_UNKNOWN
	}
}

func entryToProto(e Entry) *apiv1.MCPServer {
	out := &apiv1.MCPServer{
		Id:              e.ID,
		Name:            e.Name,
		Transport:       transportToProto(e.Transport),
		Command:         e.Command,
		Url:             e.URL,
		Enabled:         e.Enabled,
		CatalogSlug:     e.CatalogSlug,
		InstallStatus:   installStatusToProto(e.InstallStatus),
		RequiredSecrets: append([]string{}, e.RequiredSecrets...),
		HasSecretStored: e.HasSecretStored,
	}
	out.Args = append(out.Args, e.Args...)
	out.Env = map[string]string{}
	for k, v := range e.Env {
		out.Env[k] = v
	}
	out.Headers = map[string]string{}
	for k, v := range e.Headers {
		out.Headers[k] = v
	}
	if e.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
			out.CreatedAt = timestamppb.New(t)
		}
	}
	if e.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, e.UpdatedAt); err == nil {
			out.UpdatedAt = timestamppb.New(t)
		}
	}
	if e.InstallResult.Runtime != "" || e.InstallResult.Command != "" || e.InstallResult.OK || e.InstallResult.Error != "" {
		out.InstallResult = &apiv1.MCPInstallResult{
			Runtime:     e.InstallResult.Runtime,
			Command:     e.InstallResult.Command,
			Ok:          e.InstallResult.OK,
			Error:       e.InstallResult.Error,
			InstalledAt: e.InstallResult.InstalledAt,
		}
	}
	return out
}

func catalogEntryToProto(c CatalogEntry) *apiv1.MCPCatalogEntry {
	out := &apiv1.MCPCatalogEntry{
		Slug:             c.Slug,
		DisplayName:      c.DisplayName,
		Description:      c.Description,
		Transport:        c.Transport,
		InstallMechanism: c.InstallMechanism,
		DefaultCommand:   c.DefaultCommand,
		DefaultUrl:       c.DefaultURL,
		DocsUrl:          c.DocsURL,
		RequiredEnv:      append([]string{}, c.RequiredEnv...),
	}
	out.DefaultArgs = append(out.DefaultArgs, c.DefaultArgs...)
	for _, ev := range c.DefaultEnv {
		out.DefaultEnv = append(out.DefaultEnv, &apiv1.MCPCatalogEnvVar{
			Name: ev.Name, Default: ev.Default, Secret: ev.Secret, Description: ev.Description,
		})
	}
	for _, hv := range c.DefaultHeaders {
		out.DefaultHeaders = append(out.DefaultHeaders, &apiv1.MCPCatalogEnvVar{
			Name: hv.Name, Default: hv.Default, Secret: hv.Secret, Description: hv.Description,
		})
	}
	return out
}
