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

	// onChanged is called AFTER a successful create/update/delete commits, with
	// the tenant and the secret's NAME. Nil is fine (no listener).
	//
	// WHY IT EXISTS. Provider clients are cached by the orchicon registry with the
	// resolved credential BAKED INTO them, and the credential resolver caches the
	// decrypted plaintext for the life of the process. Both are dropped by
	// Registry.Invalidate — which the PROVIDERS service calls on every token
	// mutation (SetProviderToken/ClearProviderToken/UpdateSettings…). The SECRETS
	// service called nothing, so rotating a provider key from Settings → Secrets
	// left the plane authenticating with the OLD value until the process
	// restarted: the operator replaces an expired key, retries, and gets the same
	// 401 with no way to tell the plane had not noticed. That is this hook.
	//
	// It is a callback rather than an import because internal/secrets must stay a
	// store: it knows nothing about providers, and internal/providers already
	// depends on the secrets TABLE, not this package.
	onChanged func(ctx context.Context, tenantID, name string)
}

func New(pool *db.Pool, kek []byte) *Service {
	return &Service{pool: pool, kek: kek}
}

// SetOnChanged installs the post-write hook (see the field's comment). Called
// once at wiring time; nil disables it.
func (s *Service) SetOnChanged(fn func(ctx context.Context, tenantID, name string)) {
	s.onChanged = fn
}

// notify runs the hook, if any. Best-effort by design: a cache that fails to
// clear must not fail the write the operator asked for — the next restart still
// picks the new value up.
func (s *Service) notify(ctx context.Context, tenantID, name string) {
	if s.onChanged != nil {
		s.onChanged(ctx, tenantID, name)
	}
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
	// AFTER the commit: a listener that clears a cache for a write that then
	// rolled back would serve a value the database does not hold.
	s.notify(ctx, tenantID, row.Name)
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
	// A ROTATED VALUE MUST REACH THE NEXT REQUEST, not the next restart.
	s.notify(ctx, tenantID, row.Name)
	return rowToSecret(row), nil
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// READ THE NAME BEFORE THE ROW GOES: the post-write hook is keyed by NAME (a
	// cache is keyed by what it was asked for), and after the delete there is
	// nothing left to look it up by.
	name := ""
	if row, gerr := db.GetSecret(ctx, tx.Tx, tenantID, id); gerr == nil {
		name = row.Name
	}
	if err := db.DeleteSecret(ctx, tx.Tx, tenantID, id); err != nil {
		return err
	}
	_ = audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "secret.deleted", TargetType: "secret", TargetID: id})
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// A DELETED credential must stop being served from cache too: otherwise a
	// cleared token keeps authenticating until restart.
	s.notify(ctx, tenantID, name)
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
