package secrets

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	svc *Service
	log *slog.Logger
	apiv1connect.UnimplementedSecretsServiceHandler
}

var _ apiv1connect.SecretsServiceHandler = (*Handler)(nil)

func NewHandler(pool *db.Pool, kek []byte, log *slog.Logger) *Handler {
	return &Handler{svc: New(pool, kek), log: log}
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func toProto(s Secret) *apiv1.TenantSecret {
	var ca, ua *timestamppb.Timestamp
	if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
		ca = timestamppb.New(t)
	}
	if t, err := time.Parse(time.RFC3339, s.UpdatedAt); err == nil {
		ua = timestamppb.New(t)
	}
	if ca == nil {
		ca = timestamppb.Now()
	}
	if ua == nil {
		ua = timestamppb.Now()
	}
	return &apiv1.TenantSecret{Id: s.ID, TenantId: s.TenantID, Name: s.Name, Description: s.Description, KeyVersion: int32(s.KeyVersion), CreatedAt: ca, UpdatedAt: ua}
}

func (h *Handler) ListSecrets(ctx context.Context, req *connect.Request[apiv1.ListSecretsRequest]) (*connect.Response[apiv1.ListSecretsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	secrets, next, err := h.svc.List(ctx, tenantID, req.Msg.Search, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &apiv1.ListSecretsResponse{NextPageToken: next}
	for _, s := range secrets {
		out.Secrets = append(out.Secrets, toProto(s))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) GetSecret(ctx context.Context, req *connect.Request[apiv1.GetSecretRequest]) (*connect.Response[apiv1.GetSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id required"))
	}
	s, err := h.svc.Get(ctx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetSecretResponse{Secret: toProto(s)}), nil
}

func (h *Handler) CreateSecret(ctx context.Context, req *connect.Request[apiv1.CreateSecretRequest]) (*connect.Response[apiv1.CreateSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	s, err := h.svc.Create(ctx, tenantID, req.Msg.Name, req.Msg.Value, req.Msg.Description)
	if err != nil {
		if isValidation(err) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if isKEKUnavailable(err) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	h.log.Info("secret created", "id", s.ID, "name", s.Name)
	return connect.NewResponse(&apiv1.CreateSecretResponse{Secret: toProto(s)}), nil
}

func (h *Handler) UpdateSecret(ctx context.Context, req *connect.Request[apiv1.UpdateSecretRequest]) (*connect.Response[apiv1.UpdateSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	var val *string
	if req.Msg.Value != nil {
		val = req.Msg.Value
	}
	var desc *string
	if req.Msg.Description != nil {
		desc = req.Msg.Description
	}
	s, err := h.svc.Update(ctx, tenantID, req.Msg.Id, val, desc)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		if isValidation(err) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateSecretResponse{Secret: toProto(s)}), nil
}

func (h *Handler) DeleteSecret(ctx context.Context, req *connect.Request[apiv1.DeleteSecretRequest]) (*connect.Response[apiv1.DeleteSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err := h.svc.Delete(ctx, tenantID, req.Msg.Id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.DeleteSecretResponse{}), nil
}

func isValidation(err error) bool {
	s := err.Error()
	return contains(s, "invalid") || contains(s, "must match") || contains(s, "too many") || contains(s, "too long")
}
func isKEKUnavailable(err error) bool { return contains(err.Error(), "KEK not configured") }
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
