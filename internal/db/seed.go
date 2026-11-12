package db

import (
	"context"
	"fmt"
)

// SeedDevTenant inserts the deployment tenant (ORCHICON_DEPLOYMENT_TENANT_ID, default "tnt_dev") if
// it does not already exist.
func SeedDevTenant(ctx context.Context, p *Pool, tenantID string) error {
	const q = `INSERT INTO tenants (id, slug, name, status)
		VALUES ($1, $1, 'Development Tenant', 'active')
		ON CONFLICT (id) DO NOTHING`
	if _, err := p.Exec(ctx, q, tenantID); err != nil {
		return fmt.Errorf("db: seed dev tenant: %w", err)
	}
	return nil
}
