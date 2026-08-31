package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secrets"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func secretsService(pool *db.Pool, kek []byte) (*secrets.Service, error) {
	if len(kek) != 32 {
		return nil, fmt.Errorf("secrets store unavailable: KEK not configured")
	}
	return secrets.New(pool, kek), nil
}

func toolListSecrets(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Search    string `json:"search"`
		PageSize  int    `json:"page_size"`
		PageToken string `json:"page_token"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	svc, err := secretsService(pool, kek)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContext(ctx)
	list, next, err := svc.List(ctx, tenantID, params.Search, params.PageSize, params.PageToken)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s) // secrets.Secret is already a value-less compact row
	}
	env := newCompactList(out, "")
	env.NextPageToken = next
	return json.Marshal(env)
}

func toolCreateSecret(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name        string `json:"name"`
		Value       string `json:"value"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Name == "" || params.Value == "" {
		return nil, fmt.Errorf("name and value are required")
	}
	svc, err := secretsService(pool, kek)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContext(ctx)
	s, err := svc.Create(ctx, tenantID, params.Name, params.Value, params.Description)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func toolUpdateSecret(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID          string  `json:"id"`
		Value       *string `json:"value"`
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if params.Value == nil && params.Description == nil {
		return nil, fmt.Errorf("at least one of value or description required")
	}
	svc, err := secretsService(pool, kek)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContext(ctx)
	s, err := svc.Update(ctx, tenantID, params.ID, params.Value, params.Description)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func toolDeleteSecret(ctx context.Context, pool *db.Pool, kek []byte, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	svc, err := secretsService(pool, kek)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContext(ctx)
	if err := svc.Delete(ctx, tenantID, params.ID); err != nil {
		return nil, err
	}
	// Ensure empty object not null
	return json.Marshal(map[string]string{"id": params.ID, "status": "deleted"})
}

// Ensure db import used for vet even if service hides it.
var _ = db.ErrNotFound
