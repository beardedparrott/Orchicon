package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the SettingsService Connect handler.
type Service struct {
	pool      *db.Pool
	log       *slog.Logger
	dsn       string // Postgres DSN for backup/restore
	apiv1connect.UnimplementedSettingsServiceHandler
}

var _ apiv1connect.SettingsServiceHandler = (*Service)(nil)

func New(pool *db.Pool, log *slog.Logger, dsn string) *Service {
	return &Service{pool: pool, log: log, dsn: dsn}
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "settings.updated", "settings", tenantID,
		nil, audit.Snapshot(map[string]any{"default_worker_model": row.DefaultWorkerModel, "default_ask_orchicon_model": row.DefaultAskOrchiconModel})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit settings.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateSettingsResponse{
		Settings: settingsRowToProto(&row),
	}), nil
}

// recordAudit writes an actor-based audit row in the caller's tx,
// resolving the actor from the request context. Must be called in the
// same transaction as the mutation so the row commits atomically.
func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}

// recordAuditShort writes an audit row in its own short tenant tx. Used
// for backup/restore ops that mutate the filesystem/DB outside a tenant
// tx (CreateBackup / RestoreBackup / DeleteBackup).
func (s *Service) recordAuditShort(ctx context.Context, action string, before, after json.RawMessage) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		s.log.Error("audit: begin tx", "error", err, "action", action)
		return
	}
	defer ttx.Rollback(ctx)
	if err := recordAudit(ctx, ttx.Tx, tenantID, action, "settings", tenantID, before, after); err != nil {
		s.log.Error("audit: record", "error", err, "action", action)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Error("audit: commit", "error", err, "action", action)
	}
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
		DefaultBudgetOverrides:          string(r.DefaultBudgetOverrides),
		ExecutionReapGraceSeconds:       r.ExecutionReapGraceSeconds,
		ExecutionReapConsecutiveFailures: r.ExecutionReapConsecutiveFailures,
		BackupSchedule:                  r.BackupSchedule,
		BackupRetentionDays:             r.BackupRetentionDays,
		BackupDirectory:                 r.BackupDirectory,
		LogDirectory:                    r.LogDirectory,
		LogMaxSizeMb:                    r.LogMaxSizeMB,
		LogRollIntervalHours:            r.LogRollIntervalHours,
		LogRetentionDays:                r.LogRetentionDays,
		LogMaxFiles:                     r.LogMaxFiles,
		CreatedAt:                       timestamppb.New(r.CreatedAt),
		UpdatedAt:                       timestamppb.New(r.UpdatedAt),
	}
}

func settingsProtoToRow(s *apiv1.TenantSettings) db.TenantSettingsRow {
	if s == nil {
		return db.TenantSettingsRow{}
	}
	budget := s.DefaultBudgetOverrides
	if strings.TrimSpace(budget) == "" {
		// The column is jsonb NOT NULL DEFAULT '{}'; an empty string is
		// invalid JSON. Treat an unset field as "no override" so a client
		// that only edits log settings doesn't break the update.
		budget = "{}"
	}
	return db.TenantSettingsRow{
		DefaultWorkerModel:          s.DefaultWorkerModel,
		DefaultAskOrchiconModel:     s.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds: s.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds: s.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:   s.StallTextLoopWindowSeconds,
		StallRepetitionCount:         s.StallRepetitionCount,
		StallRepetitionWindowSeconds: s.StallRepetitionWindowSeconds,
		DefaultBudgetOverrides:       []byte(budget),
		ExecutionReapGraceSeconds:    s.ExecutionReapGraceSeconds,
		ExecutionReapConsecutiveFailures: s.ExecutionReapConsecutiveFailures,
		BackupSchedule:              s.BackupSchedule,
		BackupRetentionDays:         s.BackupRetentionDays,
		BackupDirectory:             s.BackupDirectory,
		LogDirectory:                s.LogDirectory,
		LogMaxSizeMB:                s.LogMaxSizeMb,
		LogRollIntervalHours:        s.LogRollIntervalHours,
		LogRetentionDays:            s.LogRetentionDays,
		LogMaxFiles:                 s.LogMaxFiles,
	}
}

func (s *Service) backupDir(ctx context.Context) string {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return ""
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return ""
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil || row.BackupDirectory == "" {
		d, _ := backup.DefaultDir()
		return d
	}
	return row.BackupDirectory
}

func (s *Service) CreateBackup(ctx context.Context, req *connect.Request[apiv1.CreateBackupRequest]) (*connect.Response[apiv1.CreateBackupResponse], error) {
	dir := req.Msg.Directory
	if dir == "" {
		dir = s.backupDir(ctx)
	}
	info, err := backup.Create(ctx, s.dsn, dir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("backup: %w", err))
	}
	s.recordAuditShort(ctx, "backup.created", nil, audit.Snapshot(map[string]any{"name": info.Name, "size_bytes": info.SizeBytes}))
	return connect.NewResponse(&apiv1.CreateBackupResponse{
		Name:      info.Name,
		SizeBytes: info.SizeBytes,
		CreatedAt: info.CreatedAt.UTC().Format(time.RFC3339),
	}), nil
}

func (s *Service) ListBackups(ctx context.Context, req *connect.Request[apiv1.ListBackupsRequest]) (*connect.Response[apiv1.ListBackupsResponse], error) {
	dir := req.Msg.Directory
	if dir == "" {
		dir = s.backupDir(ctx)
	}
	list, err := backup.List(dir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list backups: %w", err))
	}
	entries := make([]*apiv1.BackupEntry, 0, len(list))
	for _, b := range list {
		entries = append(entries, &apiv1.BackupEntry{
			Name:      b.Name,
			SizeBytes: b.SizeBytes,
			CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return connect.NewResponse(&apiv1.ListBackupsResponse{Backups: entries}), nil
}

func (s *Service) RestoreBackup(ctx context.Context, req *connect.Request[apiv1.RestoreBackupRequest]) (*connect.Response[apiv1.RestoreBackupResponse], error) {
	dir := req.Msg.Directory
	if dir == "" {
		dir = s.backupDir(ctx)
	}
	path := req.Msg.Name
	if !containsPathSeparator(path) {
		path = dir + "/" + path
	}
	if err := backup.Restore(ctx, s.dsn, path); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("restore: %w", err))
	}
	s.recordAuditShort(ctx, "backup.restored", nil, audit.Snapshot(map[string]any{"name": req.Msg.Name, "path": path}))
	return connect.NewResponse(&apiv1.RestoreBackupResponse{}), nil
}

func (s *Service) DeleteBackup(ctx context.Context, req *connect.Request[apiv1.DeleteBackupRequest]) (*connect.Response[apiv1.DeleteBackupResponse], error) {
	dir := req.Msg.Directory
	if dir == "" {
		dir = s.backupDir(ctx)
	}
	if err := backup.Delete(dir, req.Msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("delete backup: %w", err))
	}
	s.recordAuditShort(ctx, "backup.deleted", audit.Snapshot(map[string]any{"name": req.Msg.Name}), nil)
	return connect.NewResponse(&apiv1.DeleteBackupResponse{}), nil
}

func containsPathSeparator(s string) bool {
	for _, c := range s {
		if c == '/' || c == '\\' {
			return true
		}
	}
	return false
}
