package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func makeWorkerSlug(name string) string {
	s := strings.ToLower(name)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "worker"
	}
	return s
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
	return json.Marshal(workers)
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

func toolCreateWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name       string `json:"name"`
		Purpose    string `json:"purpose"`
		ModelRef   string `json:"model_ref"`
		RuntimeRef string `json:"runtime_ref"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	worker, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		Name:     params.Name,
		Slug:     makeWorkerSlug(params.Name),
		Purpose:  params.Purpose,
		Status:   "draft",
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(worker)
}
