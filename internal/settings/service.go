package settings

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the SettingsService Connect handler.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedSettingsServiceHandler
}

var _ apiv1connect.SettingsServiceHandler = (*Service)(nil)

func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) GetSettings(ctx context.Context, req *connect.Request[apiv1.GetSettingsRequest]) (*connect.Response[apiv1.GetSettingsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetSettingsResponse{
		Settings: settingsRowToProto(&row),
	}), nil
}

func (s *Service) UpdateSettings(ctx context.Context, req *connect.Request[apiv1.UpdateSettingsRequest]) (*connect.Response[apiv1.UpdateSettingsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenantID, settingsProtoToRow(req.Msg.Settings))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateSettingsResponse{
		Settings: settingsRowToProto(&row),
	}), nil
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("no tenant in context")
	}
	return id, nil
}

func settingsRowToProto(r *db.TenantSettingsRow) *apiv1.TenantSettings {
	return &apiv1.TenantSettings{
		DefaultWorkerModel:              r.DefaultWorkerModel,
		DefaultAskOrchiconModel:         r.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds:    r.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds:    r.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:      r.StallTextLoopWindowSeconds,
		StallRepetitionCount:            r.StallRepetitionCount,
		StallRepetitionWindowSeconds:    r.StallRepetitionWindowSeconds,
		CreatedAt:                       timestamppb.New(r.CreatedAt),
		UpdatedAt:                       timestamppb.New(r.UpdatedAt),
	}
}

func settingsProtoToRow(s *apiv1.TenantSettings) db.TenantSettingsRow {
	if s == nil {
		return db.TenantSettingsRow{}
	}
	return db.TenantSettingsRow{
		DefaultWorkerModel:          s.DefaultWorkerModel,
		DefaultAskOrchiconModel:     s.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds: s.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds: s.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:   s.StallTextLoopWindowSeconds,
		StallRepetitionCount:         s.StallRepetitionCount,
		StallRepetitionWindowSeconds: s.StallRepetitionWindowSeconds,
	}
}
