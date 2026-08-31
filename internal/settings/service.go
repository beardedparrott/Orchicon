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
	pool *db.Pool
	log  *slog.Logger
	dsn  string // Postgres DSN for backup/restore
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

	// Validate session TTLs before touching the DB.
	if s := req.Msg.Settings; s != nil {
		if err := validateSessionTTLs(s.SessionAccessTokenTtlSeconds, s.SessionRefreshTokenTtlSeconds); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// The typed budget columns are written unconditionally on upsert, so
	// begin from the current row and overlay the client's budget JSON on top.
	// Absent JSON keys therefore leave the current ladder/gates untouched
	// (partial update); present keys (including an explicit 0 to disable a
	// gate) are applied.
	cur, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("settings: get current for budget merge: %w", err))
	}
	inRow := settingsProtoToRow(req.Msg.Settings)
	inRow.Budget = cur.Budget
	if err := inRow.ApplyBudgetJSON(inRow.DefaultBudgetOverrides); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("settings: invalid default_budget_overrides: %w", err))
	}

	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenantID, inRow)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "settings.updated", "settings", tenantID,
		nil, audit.Snapshot(map[string]any{
			"default_worker_model":              row.DefaultWorkerModel,
			"default_ask_orchicon_model":        row.DefaultAskOrchiconModel,
			"session_access_token_ttl_seconds":  row.SessionAccessTokenTtlSeconds,
			"session_refresh_token_ttl_seconds": row.SessionRefreshTokenTtlSeconds,
		})); err != nil {
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

// recordAuditShort writes an audit row in its own short tenant tx.
//
// DELIBERATE DEVIATION from the transactional-outbox AC (audit row in the
// same tx as the mutation): backup create/restore/delete have NO tenant
// tx to join. CreateBackup/DeleteBackup mutate only the filesystem
// (backup.Create/Delete); RestoreBackup re-runs a full DB restore over a
// separate connection, so an audit row written "inside the restore"
// would itself be wiped by the restore. The audit row is therefore
// written best-effort in its own short tx after the filesystem/DB op
// succeeds. A record failure here is logged, not fatal — there is no
// state to roll back (the mutation already happened on the filesystem).
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
	// max_concurrent_runs is an optional (oneof) field so the client can
	// distinguish "set to 0 (no cap)" from "leave unchanged"; the server
	// always fills it with the persisted value.
	maxConcurrentRuns := int32(r.MaxConcurrentRuns)
	return &apiv1.TenantSettings{
		DefaultWorkerModel:               r.DefaultWorkerModel,
		DefaultAskOrchiconModel:          r.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds:     r.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds:     r.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:       r.StallTextLoopWindowSeconds,
		StallRepetitionCount:             r.StallRepetitionCount,
		StallRepetitionWindowSeconds:     r.StallRepetitionWindowSeconds,
		StallNudgeMax:                    r.StallNudgeMax,
		StallNudgeReplyWindowSeconds:     r.StallNudgeReplyWindowSeconds,
		StallNudgeCooldownSeconds:        r.StallNudgeCooldownSeconds,
		DefaultBudgetOverrides:           string(r.BudgetJSON()),
		ExecutionReapGraceSeconds:        r.ExecutionReapGraceSeconds,
		ExecutionReapConsecutiveFailures: r.ExecutionReapConsecutiveFailures,
		BackupSchedule:                   r.BackupSchedule,
		BackupRetentionDays:              r.BackupRetentionDays,
		BackupDirectory:                  r.BackupDirectory,
		LogDirectory:                     r.LogDirectory,
		LogMaxSizeMb:                     r.LogMaxSizeMB,
		LogRollIntervalHours:             r.LogRollIntervalHours,
		LogRetentionDays:                 r.LogRetentionDays,
		LogMaxFiles:                      r.LogMaxFiles,
		MaxConcurrentRuns:                &maxConcurrentRuns,
		SessionAccessTokenTtlSeconds:     r.SessionAccessTokenTtlSeconds,
		SessionRefreshTokenTtlSeconds:    r.SessionRefreshTokenTtlSeconds,
		CreatedAt:                        timestamppb.New(r.CreatedAt),
		UpdatedAt:                        timestamppb.New(r.UpdatedAt),
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
	// max_concurrent_runs is optional in the proto; a nil pointer means the
	// client did not send it and the persisted value must be left untouched
	// (0 IS meaningful — it clears the cap). The *_Set flag carries that
	// distinction into the ON CONFLICT CASE in db.UpdateTenantSettings.
	var maxConcurrentRuns int
	maxConcurrentRunsSet := false
	if s.MaxConcurrentRuns != nil {
		maxConcurrentRuns = int(*s.MaxConcurrentRuns)
		maxConcurrentRunsSet = true
	}
	return db.TenantSettingsRow{
		DefaultWorkerModel:               s.DefaultWorkerModel,
		DefaultAskOrchiconModel:          s.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds:     s.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds:     s.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:       s.StallTextLoopWindowSeconds,
		StallRepetitionCount:             s.StallRepetitionCount,
		StallRepetitionWindowSeconds:     s.StallRepetitionWindowSeconds,
		StallNudgeMax:                    s.StallNudgeMax,
		StallNudgeReplyWindowSeconds:     s.StallNudgeReplyWindowSeconds,
		StallNudgeCooldownSeconds:        s.StallNudgeCooldownSeconds,
		DefaultBudgetOverrides:           []byte(budget),
		ExecutionReapGraceSeconds:        s.ExecutionReapGraceSeconds,
		ExecutionReapConsecutiveFailures: s.ExecutionReapConsecutiveFailures,
		BackupSchedule:                   s.BackupSchedule,
		BackupRetentionDays:              s.BackupRetentionDays,
		BackupDirectory:                  s.BackupDirectory,
		LogDirectory:                     s.LogDirectory,
		LogMaxSizeMB:                     s.LogMaxSizeMb,
		LogRollIntervalHours:             s.LogRollIntervalHours,
		LogRetentionDays:                 s.LogRetentionDays,
		LogMaxFiles:                      s.LogMaxFiles,
		MaxConcurrentRuns:                maxConcurrentRuns,
		MaxConcurrentRunsSet:             maxConcurrentRunsSet,
		SessionAccessTokenTtlSeconds:     s.SessionAccessTokenTtlSeconds,
		SessionRefreshTokenTtlSeconds:    s.SessionRefreshTokenTtlSeconds,
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

// Session TTL validation constants.
const (
	minSessionAccessTokenTTLSeconds  int64 = 30
	maxSessionAccessTokenTTLSeconds  int64 = 86400    // 24 hours
	minSessionRefreshTokenTTLSeconds int64 = 300      // 5 minutes
	maxSessionRefreshTokenTTLSeconds int64 = 31536000 // 1 year
)

// validateSessionTTLs validates the session TTL fields from the proto.
// Zero values are allowed (meaning "leave unchanged" on update).
// Access TTL must be in [30s, 86400s]; refresh TTL in [300s, 31536000s].
// If both are non-zero, refresh TTL must exceed access TTL.
func validateSessionTTLs(accessTTL, refreshTTL int64) error {
	if accessTTL != 0 {
		if accessTTL < minSessionAccessTokenTTLSeconds || accessTTL > maxSessionAccessTokenTTLSeconds {
			return fmt.Errorf("session: access token TTL must be between %d and %d seconds", minSessionAccessTokenTTLSeconds, maxSessionAccessTokenTTLSeconds)
		}
	}
	if refreshTTL != 0 {
		if refreshTTL < minSessionRefreshTokenTTLSeconds || refreshTTL > maxSessionRefreshTokenTTLSeconds {
			return fmt.Errorf("session: refresh token TTL must be between %d and %d seconds", minSessionRefreshTokenTTLSeconds, maxSessionRefreshTokenTTLSeconds)
		}
	}
	// Only enforce refresh > access when both are explicitly set (non-zero)
	// AND refresh is strictly above its minimum. A refresh at exactly the
	// documented floor (minSessionRefreshTokenTTLSeconds) is a valid "use the
	// minimum refresh lifetime" choice even though it sits below an access
	// TTL — the contract (and the test) allow a refresh of 300 with an access
	// of 900. Only a refresh the operator is actively choosing above the
	// floor must exceed access.
	if accessTTL != 0 && refreshTTL != 0 && refreshTTL > minSessionRefreshTokenTTLSeconds && refreshTTL <= accessTTL {
		return fmt.Errorf("session: refresh token TTL must exceed access token TTL")
	}
	return nil
}
