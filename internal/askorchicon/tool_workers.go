package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/worker"
)

// Header field bounds for tool-driven worker edits — mirror
// internal/worker's validateName/validateTextField limits so the tool
// path enforces the same input contract as the Connect RPC.
const (
	toolWorkerMaxNameLen    = 500
	toolWorkerMaxDescLen    = 1 << 14
	toolWorkerMaxPurposeLen = 1 << 14
)

// rowWithExtra marshals row and merges extra top-level keys into the flat
// row JSON. Tool responses keep the flat row shape existing callers parse
// while surfacing the created draft version (version, version_id).
func rowWithExtra(row any, extra map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		m[k] = v
	}
	return json.Marshal(m)
}

func toolListWorkers(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	workers, err := db.ListWorkers(ctx, ttx.Tx, db.ListWorkersFilter{
		TenantID: tenantID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(workers))
	for _, w := range workers {
		out = append(out, compactWorker(w))
	}
	return json.Marshal(newCompactList(out, "get_worker"))
}

func toolGetWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	worker, err := db.GetWorker(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(worker)
}

// toolCreateWorker creates a worker AND its first draft version-1 row in
// one transaction via the shared worker.CreateWorkerTx core (the service
// path's implementation), persisting model_ref and the prompt fields on
// the version and writing the worker.created audit row. The worker is
// immediately editable and publishable from the UI.
func toolCreateWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name         string `json:"name"`
		Purpose      string `json:"purpose"`
		ModelRef     string `json:"model_ref"`
		Description  string `json:"description"`
		VersionNote  string `json:"version_note"`
		Role         string `json:"role"`
		Skills       string `json:"skills"`
		Behavior     string `json:"behavior"`
		AgentsMD     string `json:"agents_md"`
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(params.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	tenantID := tenant.FromContext(ctx)
	in := worker.CreateWorkerInput{
		TenantID:     tenantID,
		Name:         params.Name,
		Purpose:      params.Purpose,
		ModelRef:     params.ModelRef,
		Description:  params.Description,
		VersionNote:  params.VersionNote,
		Role:         params.Role,
		Skills:       params.Skills,
		Behavior:     params.Behavior,
		AgentsMD:     params.AgentsMD,
		SystemPrompt: params.SystemPrompt,
	}
	if err := worker.ValidateCreateWorkerInput(&in); err != nil {
		return nil, err
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	created, createdVersion, err := worker.CreateWorkerTx(ctx, ttx.Tx, in)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return rowWithExtra(created, map[string]any{
		"version":    createdVersion.Version,
		"version_id": createdVersion.ID,
	})
}

// toolUpdateWorker edits the worker's header fields (name, description,
// purpose) — the same scope as Service.UpdateWorker, with the same field
// bounds — and writes a worker.updated audit row (no outbox event: the
// service writes none for header updates, so this IS parity).
func toolUpdateWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Purpose     string `json:"purpose"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	update := db.UpdateWorkerFields{}
	if v := strings.TrimSpace(params.Name); v != "" {
		if utf8.RuneCountInString(v) > toolWorkerMaxNameLen {
			return nil, fmt.Errorf("name must be at most %d characters", toolWorkerMaxNameLen)
		}
		update.Name = &v
	}
	if v := strings.TrimSpace(params.Description); v != "" {
		if utf8.RuneCountInString(v) > toolWorkerMaxDescLen {
			return nil, fmt.Errorf("description must be at most %d characters", toolWorkerMaxDescLen)
		}
		update.Description = &v
	}
	if v := strings.TrimSpace(params.Purpose); v != "" {
		if utf8.RuneCountInString(v) > toolWorkerMaxPurposeLen {
			return nil, fmt.Errorf("purpose must be at most %d characters", toolWorkerMaxPurposeLen)
		}
		update.Purpose = &v
	}
	updated, err := db.UpdateWorker(ctx, ttx.Tx, tenantID, params.ID, current.Version, update)
	if err != nil {
		return nil, err
	}
	snapshot := func(w db.WorkerRow) json.RawMessage {
		return audit.Snapshot(map[string]any{
			"id": w.ID, "name": w.Name, "description": w.Description,
			"purpose": w.Purpose, "status": w.Status,
		})
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.updated", "worker", updated.ID,
		snapshot(current), snapshot(updated)); err != nil {
		return nil, fmt.Errorf("audit worker.updated: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

func toolDeleteWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	if err := db.DeleteWorker(ctx, ttx.Tx, tenantID, params.ID); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"status": "deleted"})
}

func toolListWorkerVersions(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.WorkerID == "" {
		return nil, fmt.Errorf("worker_id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	versions, err := db.ListWorkerVersions(ctx, ttx.Tx, tenantID, params.WorkerID)
	if err != nil {
		return nil, err
	}
	if versions == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(versions)
}

func toolPublishWorkerVersion(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.WorkerID == "" || params.Version <= 0 {
		return nil, fmt.Errorf("worker_id and a positive version are required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	version, err := db.PublishWorkerVersion(ctx, ttx.Tx, tenantID, params.WorkerID, params.Version)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(version)
}

func toolDeprecateWorkerVersion(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.WorkerID == "" || params.Version <= 0 {
		return nil, fmt.Errorf("worker_id and a positive version are required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	version, err := db.DeprecateWorkerVersion(ctx, ttx.Tx, tenantID, params.WorkerID, params.Version)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(version)
}

func toolSetActiveWorkerVersion(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.WorkerID == "" || params.Version <= 0 {
		return nil, fmt.Errorf("worker_id and a positive version are required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, params.WorkerID)
	if err != nil {
		return nil, err
	}
	worker, err := db.SetActiveWorkerVersion(ctx, ttx.Tx, tenantID, params.WorkerID, current.Version, params.Version)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(worker)
}
