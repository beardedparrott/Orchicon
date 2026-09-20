package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetSecretByName resolves a tenant secret by NAME within a tenant.
// Consumed by the provider credential resolver (internal/orchicon), which
// stores secret NAMES (not ids) in provider profiles. Same tx/RLS pattern
// as GetSecret.
func GetSecretByName(ctx context.Context, tx pgx.Tx, tenantID, name string) (SecretRow, error) {
	const q = `SELECT ` + secretCols + ` FROM tenant_secrets WHERE tenant_id=$1 AND name=$2`
	var r SecretRow
	err := tx.QueryRow(ctx, q, tenantID, name).Scan(&r.ID, &r.TenantID, &r.Name, &r.Description, &r.Ciphertext, &r.KeyVersion, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretRow{}, ErrNotFound
	}
	if err != nil {
		return SecretRow{}, fmt.Errorf("db: get secret by name: %w", err)
	}
	return r, nil
}
