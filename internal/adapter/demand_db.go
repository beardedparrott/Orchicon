package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// TenantDemandSet collects the host-side adapter demand set for a tenant: the tenant's Ask-default
// model ref ∪ the model refs of its dispatchable workers' latest published versions.
func TenantDemandSet(ctx context.Context, tx pgx.Tx, tenantID string) (DemandSet, error) {
	settings, err := db.GetTenantSettings(ctx, tx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("adapter: tenant demand set (settings): %w", err)
	}
	var refs []string
	if ref := strings.TrimSpace(settings.DefaultAskOrchiconModel); ref != "" {
		refs = append(refs, ref)
	}
	workerRefs, err := db.ListPublishedWorkerModelRefs(ctx, tx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("adapter: tenant demand set (workers): %w", err)
	}
	refs = append(refs, workerRefs...)
	return AdapterDemandSet(refs...), nil
}
