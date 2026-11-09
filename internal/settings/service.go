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
	"github.com/beardedparrott/orchicon/internal/adapter"
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

	// policyPath is the operator's persistent permission policy file
	// (permission_policy.go). Empty falls back to permpolicy.DefaultPath().
	policyPath string

	// validationRegistry is the model-ref validation catalog (ADR-0003).
	validationRegistry adapter.ProviderRegistry
	apiv1connect.UnimplementedSettingsServiceHandler
}

var _ apiv1connect.SettingsServiceHandler = (*Service)(nil)

func New(pool *db.Pool, log *slog.Logger, dsn string) *Service {
	return &Service{pool: pool, log: log, dsn: dsn}
}

// SetValidationRegistry injects the model-ref validation catalog (test / server seam, mirroring
// worker.SetModelRefRegistry).
func (s *Service) SetValidationRegistry(reg adapter.ProviderRegistry) {
	s.validationRegistry = reg
}

// registry returns the injected validation registry or the static
// builtin fallback.
func (s *Service) registry() adapter.ProviderRegistry {
	if s.validationRegistry != nil {
		return s.validationRegistry
	}
	return adapter.NewBuiltinProviderCatalog()
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

// validateModelRef checks a tenant-default model ref against the adapter/provider/model grammar
// (ADR-0003) via the injected registry (builtin ∪ CLI-discovered ∪ tenant-custom providers).
func (s *Service) validateModelRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if _, err := adapter.ParseModelRef(ref, s.registry()); err != nil {
		return err
	}
	return nil
}

func (s *Service) UpdateSettings(ctx context.Context, req *connect.Request[apiv1.UpdateSettingsRequest]) (*connect.Response[apiv1.UpdateSettingsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Validate session TTLs and default model refs before touching the DB.
	if ts := req.Msg.Settings; ts != nil {
		// Validate the stall thresholds before touching the DB.
		if err := validateStallSettings(ts); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if err := validateSessionTTLs(ts.SessionAccessTokenTtlSeconds, ts.SessionRefreshTokenTtlSeconds); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if err := s.validateModelRef(ts.DefaultWorkerModel); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("default_worker_model: %w", err))
		}
		if err := s.validateModelRef(ts.DefaultAskOrchiconModel); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("default_ask_orchicon_model: %w", err))
		}
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// The typed budget columns are written unconditionally on upsert, so begin from the current row and
	// overlay the client's budget JSON on top.
	cur, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("settings: get current for budget merge: %w", err))
	}
	inRow := settingsProtoToRow(req.Msg.Settings)
	inRow.Budget = cur.Budget
	// Same partial-update semantics for the compaction/memory policy (D4): absent
	// context_compaction/memory keys in the client JSON must leave the persisted typed columns
	// untouched.
	inRow.ContextCompactionEnabled = cur.ContextCompactionEnabled
	inRow.ContextCompactionPressureFrac = cur.ContextCompactionPressureFrac
	inRow.ContextRecentTurns = cur.ContextRecentTurns
	inRow.MemoryEnabled = cur.MemoryEnabled
	inRow.MemoryDigestEntries = cur.MemoryDigestEntries

	// THE STALL THRESHOLDS ARE MERGED TOO, and this is the fix for "I have it set to 600 seconds" still
	// producing a two-minute stall.
	mergeStallSettingsFromCurrent(&inRow, req.Msg.Settings, cur)
	if err := inRow.ApplyBudgetJSON(inRow.DefaultBudgetOverrides); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("settings: invalid default_budget_overrides: %w", err))
	}

	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenantID, inRow)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// THE SNAPSHOT IS THE ROW'S OWN SHAPE (db.TenantSettingsRow.AuditFields), so this path and the
	// agent's `update_settings` tool record the SAME fields and neither can drift from the other.
	if err := recordAudit(ctx, ttx.Tx, tenantID, "settings.updated", "settings", tenantID,
		nil, audit.Snapshot(row.AuditFields())); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit settings.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateSettingsResponse{
		Settings: settingsRowToProto(&row),
	}), nil
}

// recordAudit writes an actor-based audit row in the caller's tx, resolving the actor from the
// request context.
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
	// max_concurrent_runs is an optional (oneof) field so the client can distinguish "set to 0 (no
	// cap)" from "leave unchanged"; the server always fills it with the persisted value.
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
		StallToolHangSeconds:             r.StallToolHangSeconds,
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

// mergeStallSettingsFromCurrent fills every stall column the request OMITTED with the value already
// stored, and leaves a field the client SENT alone — including an explicit 0.
func mergeStallSettingsFromCurrent(inRow *db.TenantSettingsRow, want *apiv1.TenantSettings, cur db.TenantSettingsRow) {
	if inRow == nil {
		return
	}
	if want == nil {
		inRow.StallNoProgressWindowSeconds = cur.StallNoProgressWindowSeconds
		inRow.StallNoFileDiffWindowSeconds = cur.StallNoFileDiffWindowSeconds
		inRow.StallTextLoopWindowSeconds = cur.StallTextLoopWindowSeconds
		inRow.StallRepetitionCount = cur.StallRepetitionCount
		inRow.StallRepetitionWindowSeconds = cur.StallRepetitionWindowSeconds
		inRow.StallNudgeMax = cur.StallNudgeMax
		inRow.StallNudgeReplyWindowSeconds = cur.StallNudgeReplyWindowSeconds
		inRow.StallNudgeCooldownSeconds = cur.StallNudgeCooldownSeconds
		inRow.StallToolHangSeconds = cur.StallToolHangSeconds
		return
	}
	if want.StallNoProgressWindowSeconds == nil {
		inRow.StallNoProgressWindowSeconds = cur.StallNoProgressWindowSeconds
	}
	if want.StallNoFileDiffWindowSeconds == nil {
		inRow.StallNoFileDiffWindowSeconds = cur.StallNoFileDiffWindowSeconds
	}
	if want.StallTextLoopWindowSeconds == nil {
		inRow.StallTextLoopWindowSeconds = cur.StallTextLoopWindowSeconds
	}
	if want.StallRepetitionCount == nil {
		inRow.StallRepetitionCount = cur.StallRepetitionCount
	}
	if want.StallRepetitionWindowSeconds == nil {
		inRow.StallRepetitionWindowSeconds = cur.StallRepetitionWindowSeconds
	}
	if want.StallNudgeMax == nil {
		inRow.StallNudgeMax = cur.StallNudgeMax
	}
	if want.StallNudgeReplyWindowSeconds == nil {
		inRow.StallNudgeReplyWindowSeconds = cur.StallNudgeReplyWindowSeconds
	}
	if want.StallNudgeCooldownSeconds == nil {
		inRow.StallNudgeCooldownSeconds = cur.StallNudgeCooldownSeconds
	}
	if want.StallToolHangSeconds == nil {
		inRow.StallToolHangSeconds = cur.StallToolHangSeconds
	}
}

func settingsProtoToRow(s *apiv1.TenantSettings) db.TenantSettingsRow {
	if s == nil {
		return db.TenantSettingsRow{}
	}
	budget := s.DefaultBudgetOverrides
	if strings.TrimSpace(budget) == "" {
		// The column is jsonb NOT NULL DEFAULT '{}'; an empty string is invalid JSON.
		budget = "{}"
	}
	// max_concurrent_runs is optional in the proto; a nil pointer means the client did not send it and
	// the persisted value must be left untouched (0 IS meaningful — it clears the cap).
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
		StallToolHangSeconds:             s.StallToolHangSeconds,
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

// validateStallSettings rejects a NEGATIVE stall threshold.
func validateStallSettings(ts *apiv1.TenantSettings) error {
	if ts == nil {
		return nil
	}
	seconds := []struct {
		name string
		v    *int64
	}{
		{"stall_no_progress_window_seconds", ts.StallNoProgressWindowSeconds},
		{"stall_no_file_diff_window_seconds", ts.StallNoFileDiffWindowSeconds},
		{"stall_text_loop_window_seconds", ts.StallTextLoopWindowSeconds},
		{"stall_repetition_window_seconds", ts.StallRepetitionWindowSeconds},
		{"stall_nudge_reply_window_seconds", ts.StallNudgeReplyWindowSeconds},
		{"stall_nudge_cooldown_seconds", ts.StallNudgeCooldownSeconds},
		{"stall_tool_hang_seconds", ts.StallToolHangSeconds},
	}
	for _, f := range seconds {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("%s: must be blank (built-in default) or >= 0 (0 disables), got %d", f.name, *f.v)
		}
	}
	counts := []struct {
		name string
		v    *int32
	}{
		{"stall_repetition_count", ts.StallRepetitionCount},
		{"stall_nudge_max", ts.StallNudgeMax},
	}
	for _, f := range counts {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("%s: must be blank (built-in default) or >= 0 (0 disables), got %d", f.name, *f.v)
		}
	}
	return nil
}

// Session TTL validation constants.
const (
	minSessionAccessTokenTTLSeconds  int64 = 30
	maxSessionAccessTokenTTLSeconds  int64 = 86400    // 24 hours
	minSessionRefreshTokenTTLSeconds int64 = 300      // 5 minutes
	maxSessionRefreshTokenTTLSeconds int64 = 31536000 // 1 year
)

// validateSessionTTLs validates the session TTL fields from the proto.
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
	// Only enforce refresh > access when both are explicitly set (non-zero) AND refresh is strictly
	// above its minimum.
	if accessTTL != 0 && refreshTTL != 0 && refreshTTL > minSessionRefreshTokenTTLSeconds && refreshTTL <= accessTTL {
		return fmt.Errorf("session: refresh token TTL must exceed access token TTL")
	}
	return nil
}
