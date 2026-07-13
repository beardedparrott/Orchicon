package db

import (
	"context"
	"fmt"
)

// SeedDevTenant inserts the default dev tenant (tnt_dev) if it does not
// already exist. This runs on boot so the control plane has a tenant
// context before auth (Phase 9) lands. The tenants table has no
// tenant_id column (it IS the tenant root — docs/09 §3.1) so this write
// is not subject to RLS. Production tenants are provisioned through the
// Admin surface, never through this path.
func SeedDevTenant(ctx context.Context, p *Pool) error {
	const q = `INSERT INTO tenants (id, slug, name, status)
		VALUES ('tnt_dev', 'dev', 'Development Tenant', 'active')
		ON CONFLICT (id) DO NOTHING`
	if _, err := p.Exec(ctx, q); err != nil {
		return fmt.Errorf("db: seed dev tenant: %w", err)
	}
	return nil
}
