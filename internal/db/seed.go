package db

import (
	"context"
	"fmt"
)

// SeedDevTenant inserts the deployment tenant (ORCHICON_DEPLOYMENT_TENANT_ID,
// default "tnt_dev") if it does not already exist. This runs on boot so the
// control plane has a tenant context before auth (Phase 9) lands. The
// tenants table has no tenant_id column (it IS the tenant root — docs/09
// §3.1) so this write is not subject to RLS. The slug mirrors the tenant id
// so the tenants_slug_idx unique index can never collide with another
// tenant (single-tenant-per-deployment: the id and slug are 1:1); the
// id-conflict DO NOTHING keeps the seed idempotent, preserving an existing
// row (e.g. a pre-existing tnt_dev row) untouched. Production tenants are
// provisioned through the Admin surface, never through this path.
func SeedDevTenant(ctx context.Context, p *Pool, tenantID string) error {
	const q = `INSERT INTO tenants (id, slug, name, status)
		VALUES ($1, $1, 'Development Tenant', 'active')
		ON CONFLICT (id) DO NOTHING`
	if _, err := p.Exec(ctx, q, tenantID); err != nil {
		return fmt.Errorf("db: seed dev tenant: %w", err)
	}
	return nil
}
