package secrets

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

var nameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
const maxSecretsPerItem = 10

type Service struct {
	pool *db.Pool
	kek  []byte
}

func New(pool *db.Pool, kek []byte) *Service {
	return &Service{pool: pool, kek: kek}
}

func (s *Service) requireKEK() error {
	if len(s.kek) != 32 {
		return fmt.Errorf("secrets store unavailable: KEK not configured")
	}
	return nil
}

func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: must match ^[A-Z][A-Z0-9_]+$", name)
	}
	if len(name) > 64 {
		return fmt.Errorf("secret name too long (max 64)")
	}
	return nil
}

func ValidateSecretIDs(ids []string) error {
	if len(ids) > maxSecretsPerItem {
		return fmt.Errorf("too many secrets: max %d, got %d", maxSecretsPerItem, len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("secret id must not be empty")
		}
		if seen[id] {
			return fmt.Errorf("duplicate secret id %q", id)
		}
		seen[id] = true
	}
	return nil
}

type Secret struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	KeyVersion  int16  `json:"key_version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func rowToSecret(r db.SecretRow) Secret {
	return Secret{ID: r.ID, TenantID: r.TenantID, Name: r.Name, Description: r.Description, KeyVersion: r.KeyVersion, CreatedAt: r.CreatedAt.Format("2006-01-02T15:04:05Z"), UpdatedAt: r.UpdatedAt.Format("2006-01-02T15:04:05Z")}
}

func (s *Service) Create(ctx context.Context, tenantID, name, value, description string) (Secret, error) {
	if err := s.requireKEK(); err != nil {
		return Secret{}, err
	}
	if err := ValidateName(name); err != nil {
		return Secret{}, err
	}
	if value == "" {
		return Secret{}, fmt.Errorf("secret value must not be empty")
	}
	ct, err := secretcrypto.Encrypt([]byte(value), s.kek)
	if err != nil {
		return Secret{}, err
	}
	id := uuid.NewString()
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Secret{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.CreateSecret(ctx, tx.Tx, db.SecretRow{ID: id, TenantID: tenantID, Name: name, Description: description, Ciphertext: ct, KeyVersion: 1})
	if err != nil {
		return Secret{}, err
	}
	_ = audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "secret.created", TargetType: "secret", TargetID: id, After: audit.Snapshot(map[string]string{"name": name})})
	if err := tx.Commit(ctx); err != nil {
		return Secret{}, err
	}
	return rowToSecret(row), nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (Secret, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Secret{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetSecret(ctx, tx.Tx, tenantID, id)
	if err != nil {
		return Secret{}, err
	}
	_ = tx.Commit(ctx)
	return rowToSecret(row), nil
}

func (s *Service) List(ctx context.Context, tenantID, search string, pageSize int, pageToken string) ([]Secret, string, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListSecrets(ctx, tx.Tx, tenantID, search, pageSize, pageToken)
	if err != nil {
		return nil, "", err
	}
	_ = tx.Commit(ctx)
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToSecret(r))
	}
	var next string
	if len(out) == pageSize && len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func (s *Service) Update(ctx context.Context, tenantID, id string, value *string, description *string) (Secret, error) {
	if value != nil {
		if err := s.requireKEK(); err != nil {
			return Secret{}, err
		}
		if *value == "" {
			return Secret{}, fmt.Errorf("secret value must not be empty")
		}
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Secret{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ct *string
	var kv *int16
	if value != nil {
		enc, err := secretcrypto.Encrypt([]byte(*value), s.kek)
		if err != nil {
			return Secret{}, err
		}
		ct = &enc
		v := int16(1)
		kv = &v
	}
	row, err := db.UpdateSecret(ctx, tx.Tx, tenantID, id, description, ct, kv)
	if err != nil {
		return Secret{}, err
	}
	fields := map[string]any{}
	if description != nil {
		fields["description"] = *description
	}
	if value != nil {
		fields["value_rotated"] = true
	}
	_ = audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "secret.updated", TargetType: "secret", TargetID: id, After: audit.Snapshot(fields)})
	if err := tx.Commit(ctx); err != nil {
		return Secret{}, err
	}
	return rowToSecret(row), nil
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := db.DeleteSecret(ctx, tx.Tx, tenantID, id); err != nil {
		return err
	}
	_ = audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "secret.deleted", TargetType: "secret", TargetID: id})
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Service) DecryptForInjection(ctx context.Context, tenantID string, ids []string) (map[string]string, error) {
	if err := s.requireKEK(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	if err := ValidateSecretIDs(ids); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	m, err := db.BatchGetSecrets(ctx, tx.Tx, tenantID, ids)
	if err != nil {
		return nil, err
	}
	if len(m) != len(ids) {
		return nil, fmt.Errorf("one or more secrets not found")
	}
	out := map[string]string{}
	for _, id := range ids {
		row := m[id]
		pt, err := secretcrypto.Decrypt(row.Ciphertext, s.kek)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", row.Name, err)
		}
		out[row.Name] = string(pt)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *Service) BatchGetForValidation(ctx context.Context, tenantID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	m, err := db.BatchGetSecrets(ctx, tx.Tx, tenantID, ids)
	if err != nil {
		return err
	}
	if len(m) != len(ids) {
		return fmt.Errorf("one or more secrets not found")
	}
	_ = tx.Commit(ctx)
	return nil
}

var _ = pgx.Tx(nil)
