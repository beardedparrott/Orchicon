package fileedit

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
)

// PGStore is the Postgres-backed Store: ledger rows persist in
// file_edit_ledger through db.AppendFileEditLedger (same pool/tx style as
// every other table; SQL lives in internal/db per repo convention).
type PGStore struct {
	pool *db.Pool
}

// NewPGStore wires the durable ledger store.
func NewPGStore(pool *db.Pool) *PGStore { return &PGStore{pool: pool} }

// Append persists entries for an owner, allocating the per-owner seq inside
// one tenant transaction. Returns the persisted rows (seq/id assigned).
func (s *PGStore) Append(ctx context.Context, tenantID, ownerKind, ownerID string, entries []Entry) ([]*db.FileEditLedgerRow, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("fileedit: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	rows := make([]*db.FileEditLedgerRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, &db.FileEditLedgerRow{
			TenantID:     tenantID,
			OwnerKind:    ownerKind,
			OwnerID:      ownerID,
			Path:         e.Path,
			Kind:         e.Kind,
			UnifiedDiff:  e.UnifiedDiff,
			BeforeSize:   e.BeforeSize,
			AfterSize:    e.AfterSize,
			BeforeSHA256: e.BeforeSHA,
			AfterSHA256:  e.AfterSHA,
			Tool:         e.Tool,
			IsBinary:     e.IsBinary,
			Truncated:    e.Truncated,
		})
	}
	if err := db.AppendFileEditLedger(ctx, ttx.Tx, tenantID, ownerKind, ownerID, rows); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("fileedit: commit: %w", err)
	}
	return rows, nil
}

// List returns the ledger page for an owner (fromSeq exclusive; 0 = all)
// plus the owner's max seq — the GetSessionFileEdits read.
func (s *PGStore) List(ctx context.Context, tenantID, ownerKind, ownerID string, fromSeq int64) ([]*db.FileEditLedgerRow, int64, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("fileedit: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	return db.ListFileEditLedger(ctx, ttx.Tx, tenantID, ownerKind, ownerID, fromSeq)
}

// MarkGitConfirmed implements StoreExtras for the git reconciliation.
func (s *PGStore) MarkGitConfirmed(ctx context.Context, tenantID, ownerKind, ownerID string, paths []string) error {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("fileedit: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.MarkFileEditsGitConfirmed(ctx, ttx.Tx, tenantID, ownerKind, ownerID, paths); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// KnownPaths implements StoreExtras for the git reconciliation.
func (s *PGStore) KnownPaths(ctx context.Context, tenantID, ownerKind, ownerID string) ([]string, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("fileedit: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	return db.LedgerPaths(ctx, ttx.Tx, tenantID, ownerKind, ownerID)
}
