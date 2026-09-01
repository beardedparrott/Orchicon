package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Handler implements the ProviderService Connect handler over Service.
type Handler struct {
	svc *Service
	log *slog.Logger
	apiv1connect.UnimplementedProviderServiceHandler
}

var _ apiv1connect.ProviderServiceHandler = (*Handler)(nil)

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

func (h *Handler) ListProviders(ctx context.Context, req *connect.Request[apiv1.ProviderListRequest]) (*connect.Response[apiv1.ProviderListResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	entries, err := h.svc.ListForTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &apiv1.ProviderListResponse{}
	for _, e := range entries {
		out.Providers = append(out.Providers, entryToProto(e))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) UpdateProviderSettings(ctx context.Context, req *connect.Request[apiv1.ProviderUpdateSettingsRequest]) (*connect.Response[apiv1.ProviderUpdateSettingsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	msg := req.Msg
	in := UpdateSettingsInput{ProviderID: msg.ProviderId}
	if msg.Enabled != nil {
		in.Enabled = msg.Enabled
	}
	if msg.BaseUrlOverride != nil {
		in.BaseURLOverride = msg.BaseUrlOverride
	}
	if msg.NumCtxDefault != nil {
		n := int64(*msg.NumCtxDefault)
		in.NumCtxDefault = &n
	}
	if msg.HiddenModels != nil {
		in.HiddenModels = msg.HiddenModels
		in.HiddenModelsSet = true
	}
	if msg.ManualModels != nil {
		for _, m := range msg.ManualModels {
			in.ManualModels = append(in.ManualModels, ManualModel{
				ID: m.Id, Context: m.Context, MaxOutput: m.MaxOutput, Reasoning: m.Reasoning,
			})
		}
	}
	if msg.ReplaceManualModels != nil {
		in.ManualReplace = *msg.ReplaceManualModels
	}
	entry, err := h.svc.UpdateSettings(ctx, tenantID, in)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderUpdateSettingsResponse{Provider: entryToProto(entry)}), nil
}

func (h *Handler) CreateCustomProvider(ctx context.Context, req *connect.Request[apiv1.ProviderCreateCustomRequest]) (*connect.Response[apiv1.ProviderCreateCustomResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	entry, err := h.svc.CreateCustom(ctx, tenantID, CreateCustomInput{
		DisplayName: req.Msg.DisplayName,
		RefID:       req.Msg.RefId,
		BaseURL:     req.Msg.BaseUrl,
		AuthMode:    req.Msg.AuthMode,
	})
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderCreateCustomResponse{Provider: entryToProto(entry)}), nil
}

func (h *Handler) UpdateCustomProvider(ctx context.Context, req *connect.Request[apiv1.ProviderUpdateCustomRequest]) (*connect.Response[apiv1.ProviderUpdateCustomResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	msg := req.Msg
	in := UpdateCustomInput{RefID: msg.RefId}
	if msg.DisplayName != nil {
		in.DisplayName = msg.DisplayName
	}
	if msg.BaseUrl != nil {
		in.BaseURL = msg.BaseUrl
	}
	if msg.AuthMode != nil {
		in.AuthMode = msg.AuthMode
	}
	entry, err := h.svc.UpdateCustom(ctx, tenantID, in)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderUpdateCustomResponse{Provider: entryToProto(entry)}), nil
}

func (h *Handler) DeleteCustomProvider(ctx context.Context, req *connect.Request[apiv1.ProviderDeleteCustomRequest]) (*connect.Response[apiv1.ProviderDeleteCustomResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	err = h.svc.DeleteCustom(ctx, tenantID, req.Msg.RefId)
	if err != nil {
		var refErr *ReferencedError
		if errors.As(err, &refErr) {
			// The guard list must reach the operator: it rides the error
			// message (the UI renders errors, never error-response fields).
			names := make([]string, 0, len(refErr.Workers))
			for _, w := range refErr.Workers {
				names = append(names, fmt.Sprintf("%s (%s)", w.WorkerName, w.ModelRef))
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("provider is still referenced by workers — reassign them first: %s",
					strings.Join(names, ", ")))
		}
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderDeleteCustomResponse{}), nil
}

func (h *Handler) SetProviderToken(ctx context.Context, req *connect.Request[apiv1.ProviderSetTokenRequest]) (*connect.Response[apiv1.ProviderSetTokenResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	name, err := h.svc.SetProviderToken(ctx, tenantID, req.Msg.ProviderId, req.Msg.Token)
	if err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderSetTokenResponse{SecretName: name}), nil
}

func (h *Handler) ClearProviderToken(ctx context.Context, req *connect.Request[apiv1.ProviderClearTokenRequest]) (*connect.Response[apiv1.ProviderClearTokenResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err := h.svc.ClearProviderToken(ctx, tenantID, req.Msg.ProviderId); err != nil {
		return nil, h.mapErr(err)
	}
	return connect.NewResponse(&apiv1.ProviderClearTokenResponse{}), nil
}

func (h *Handler) ListProviderModels(ctx context.Context, req *connect.Request[apiv1.ProviderModelsRequest]) (*connect.Response[apiv1.ProviderModelsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	res, err := h.svc.ListProviderModels(ctx, tenantID, req.Msg.ProviderId)
	if err != nil {
		return nil, h.mapErr(err)
	}
	out := &apiv1.ProviderModelsResponse{Degraded: res.Degraded, Enabled: res.Enabled}
	for _, m := range res.Models {
		out.Models = append(out.Models, &apiv1.ProviderModel{
			Id: m.ID, Context: m.Context, MaxOutput: m.MaxOutput,
			Reasoning: m.Reasoning, Source: m.Source,
			WarnNoContext: m.WarnNoContext, Visible: m.Visible,
		})
	}
	return connect.NewResponse(out), nil
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

func entryToProto(e Entry) *apiv1.ProviderEntry {
	out := &apiv1.ProviderEntry{
		Id:              e.ID,
		DisplayName:     e.DisplayName,
		Kind:            e.Kind,
		BaseUrl:         e.BaseURL,
		BaseUrlOverride: e.BaseURLOverride,
		Enabled:         e.Enabled,
		AuthMode:        e.AuthMode,
		IsCustom:        e.IsCustom,
		ReadOnly:        e.ReadOnly,
		HasTokenStored:  e.HasTokenStored,
		TokenSecretName: e.TokenSecretName,
		NumCtxDefault:   e.NumCtxDefault,
	}
	out.HiddenModels = append(out.HiddenModels, e.HiddenModels...)
	return out
}
