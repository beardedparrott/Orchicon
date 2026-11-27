package secrets

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/auth"
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

// SetOnChanged forwards the service's post-write hook, so a caller that only holds the HANDLER
// (api.Mount) can register the cache invalidation that makes a rotated credential take effect.
func (h *Handler) SetOnChanged(fn func(ctx context.Context, tenantID, name string)) {
	h.svc.SetOnChanged(fn)
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

// GetExecutionSecret serves a secret's plaintext to the worker execution granted it.
//
// THE ERROR PATHS CARRY NO VALUE, and this handler deliberately does not log the request or the
// response — a secret read is recorded on the AUDIT TRAIL (inside ReadForExecution, after the
// authorisation succeeds), which is the one place it should appear. Logging here would put plaintext
// in the plane's log file, which is exactly the silent leak the design exists to avoid.
func (h *Handler) GetExecutionSecret(ctx context.Context, req *connect.Request[apiv1.GetExecutionSecretRequest]) (*connect.Response[apiv1.GetExecutionSecretResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	// WHO IS ASKING. The resolved identity's Subject is `run:<id>` for an execution and anything else
	// for a human/API caller — so a human cannot use this door to read a secret's plaintext, and a
	// worker cannot read a run it was not minted for.
	ident, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no authenticated identity"))
	}
	value, err := h.svc.ReadForExecution(ctx, tenantID, ident.Subject, req.Msg.WorkflowRunId, req.Msg.Name)
	if err != nil {
		// DELIBERATELY OPAQUE. "not available" covers both "no such secret" and "not granted": a
		// worker must not be able to enumerate the tenant's secret names through error text.
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	return connect.NewResponse(&apiv1.GetExecutionSecretResponse{
		Name:  req.Msg.Name,
		Value: value,
	}), nil
}
