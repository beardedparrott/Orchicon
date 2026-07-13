// Package adapter implements the public-facing RuntimeAdapterService
// Connect handler (docs/07_API_Specification.md §3.7). It exposes the
// adapter registry: list registered adapters, inspect capabilities.
//
// The gRPC sidecar contract (Register/Heartbeat/Execute) lives
// separately in orchicon.adapter.v1 and is implemented by the adapter
// process, not the control plane (docs/07 §10). This public service is
// what the UI and API clients call.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxKindLen    = 63
	maxVersionLen = 63
	maxEndpointLen = 500
)

// Service implements the public RuntimeAdapterService Connect handler.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedRuntimeAdapterServiceHandler
}

var _ apiv1connect.RuntimeAdapterServiceHandler = (*Service)(nil)

// New constructs a RuntimeAdapterService handler.
func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// ListAdapters returns registered adapters, optionally filtered by kind
// (docs/07 §3.7).
func (s *Service) ListAdapters(ctx context.Context, req *connect.Request[apiv1.ListAdaptersRequest]) (*connect.Response[apiv1.ListAdaptersResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListAdaptersFilter{
		TenantID: tenantID,
		PageSize: int(req.Msg.PageSize),
		AfterID:  req.Msg.PageToken,
	}
	if req.Msg.Kind != nil {
		f.Kind = strings.TrimSpace(*req.Msg.Kind)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	adapters, err := db.ListAdapters(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListAdaptersResponse{}
	for _, a := range adapters {
		resp.Adapters = append(resp.Adapters, rowToProto(a))
	}
	if len(adapters) > 0 {
		resp.NextPageToken = adapters[len(adapters)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// GetAdapterCapabilities returns the capability manifest for a
// registered adapter (docs/04 §3.2).
func (s *Service) GetAdapterCapabilities(ctx context.Context, req *connect.Request[apiv1.GetAdapterCapabilitiesRequest]) (*connect.Response[apiv1.GetAdapterCapabilitiesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	a, err := db.GetAdapter(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetAdapterCapabilitiesResponse{
		Capabilities: string(a.Capabilities),
	}), nil
}

// --- helpers ---------------------------------------------------------------

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("adapter not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

func rowToProto(a db.AdapterRow) *apiv1.RuntimeAdapter {
	p := &apiv1.RuntimeAdapter{
		Id:                    a.ID,
		TenantId:               a.TenantID,
		Kind:                  a.Kind,
		Version:               a.Version,
		Endpoint:              a.Endpoint,
		Capabilities:          string(a.Capabilities),
		Status:                a.Status,
		MaxConcurrentExecutions: int32(a.MaxConcurrentExecutions),
		RegisteredAt:          timestamppb.New(a.RegisteredAt),
	}
	if a.LastHeartbeatAt != nil {
		p.LastHeartbeatAt = timestamppb.New(*a.LastHeartbeatAt)
	}
	return p
}

// validateKind trims and bounds-checks an adapter kind.
func validateKind(kind string) (string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", errors.New("kind must not be empty")
	}
	if utf8.RuneCountInString(kind) > maxKindLen {
		return "", fmt.Errorf("kind must be at most %d characters", maxKindLen)
	}
	return kind, nil
}

// validateVersion trims and bounds-checks a semver version.
func validateVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("version must not be empty")
	}
	if utf8.RuneCountInString(v) > maxVersionLen {
		return "", fmt.Errorf("version must be at most %d characters", maxVersionLen)
	}
	return v, nil
}

// validateEndpoint trims and bounds-checks the endpoint field.
func validateEndpoint(ep string) (string, error) {
	ep = strings.TrimSpace(ep)
	if utf8.RuneCountInString(ep) > maxEndpointLen {
		return "", fmt.Errorf("endpoint must be at most %d characters", maxEndpointLen)
	}
	return ep, nil
}

// Re-export the domain status constants for the handler to use.
var (
	StatusRegistered = domain.AdapterRegistered
	StatusReady      = domain.AdapterReady
	StatusDraining   = domain.AdapterDraining
	StatusExpired    = domain.AdapterExpired
)
