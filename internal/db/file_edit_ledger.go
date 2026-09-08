package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FileEditLedgerRow is one entry of the durable file-edit ledger
// (file_edit_ledger): a single server-computed ground-truth diff for one
// path touched by one file-editing tool event of one owner (an execution or
// an Ask conversation). seq is monotonic per (tenant, owner_kind, owner_id)
// and doubles as the stream sequence for resume; id doubles as the stream
// event_id for dedup.
type FileEditLedgerRow struct {
	ID            string
	TenantID      string
	OwnerKind     string
	OwnerID       string
	Seq           int64
	Path          string
	Kind          string // create | modify | delete
	UnifiedDiff   string
	BeforeSize    int64
	AfterSize     int64
	BeforeSHA256  string
	AfterSHA256   string
	Tool          string // batch_write | write | edit | opencode:write | opencode:edit | reconcile:git
	IsBinary      bool
	Truncated     bool
	GitConfirmed  bool
	CreatedAt     time.Time
}

// FileEdit owner_kind values (polymorphic owner: executions + Ask chats).
const (
	FileEditOwnerExecution       = "execution"
	FileEditOwnerAskConversation = "ask_conversation"
)

// AppendFileEditLedger inserts ledger entries idempotently, allocating the
// per-owner seq inside the same transaction (the caller-owned-seq model of
// AppendExecutionSessionParts, but with the counter owned by the ledger
// itself so concurrent writers cannot collide). ON CONFLICT DO NOTHING keeps
// a replayed batch from double-recording. Rows already carry their intended
// content; only Seq is assigned here when zero.
func AppendFileEditLedger(ctx context.Context, tx pgx.Tx, tenantID string, ownerKind, ownerID string, rows []*FileEditLedgerRow) error {
	if len(rows) == 0 {
		return nil
	}
	// Seed the counter from the owner's current max inside this tx; rows
	// that arrive with Seq>0 (replayed/reconciled) keep it.
	next, err := NextFileEditSeq(ctx, tx, tenantID, ownerKind, ownerID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Seq == 0 {
			r.Seq = next
			next++
		}
		if r.ID == "" {
			r.ID = NewID()
		}
		if r.TenantID == "" {
			r.TenantID = tenantID
		}
		if r.Kind == "" {
			r.Kind = "modify"
		}
		const q = `INSERT INTO file_edit_ledger
			(id, tenant_id, owner_kind, owner_id, seq, path, kind, unified_diff,
			 before_size, after_size, before_sha256, after_sha256, tool,
			 is_binary, truncated, git_confirmed, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now())
			ON CONFLICT (tenant_id, owner_kind, owner_id, seq) DO NOTHING`
		if _, err := tx.Exec(ctx, q,
			r.ID, tenantID, ownerKind, ownerID, r.Seq, r.Path, r.Kind, r.UnifiedDiff,
			r.BeforeSize, r.AfterSize, r.BeforeSHA256, r.AfterSHA256, r.Tool,
			r.IsBinary, r.Truncated, r.GitConfirmed); err != nil {
			return fmt.Errorf("db: append file edit ledger: %w", err)
		}
	}
	return nil
}

// NextFileEditSeq returns MAX(seq)+1 for an owner (1 when the owner has no
// rows yet). Called inside the writer's transaction so seq allocation is
// serialized with the inserts.
func NextFileEditSeq(ctx context.Context, tx pgx.Tx, tenantID, ownerKind, ownerID string) (int64, error) {
	const q = `SELECT COALESCE(MAX(seq),0)+1 FROM file_edit_ledger
		WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3`
	var next int64
	if err := tx.QueryRow(ctx, q, tenantID, ownerKind, ownerID).Scan(&next); err != nil {
		return 0, fmt.Errorf("db: next file edit seq: %w", err)
	}
	return next, nil
}

// ListFileEditLedger returns the ledger for an owner in seq order, optionally
// from a sequence (exclusive; 0 = from the start) — the fetch/resume read of
// the GetSessionFileEdits RPC. maxSeq is the owner's current highest seq (0
// when empty), so a client knows when it is caught up.
func ListFileEditLedger(ctx context.Context, tx pgx.Tx, tenantID, ownerKind, ownerID string, fromSeq int64) ([]*FileEditLedgerRow, int64, error) {
	q := `SELECT id, tenant_id, owner_kind, owner_id, seq, path, kind, unified_diff,
		before_size, after_size, before_sha256, after_sha256, tool,
		is_binary, truncated, git_confirmed, created_at
		FROM file_edit_ledger
		WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3`
	args := []any{tenantID, ownerKind, ownerID}
	if fromSeq > 0 {
		q += ` AND seq > $4`
		args = append(args, fromSeq)
	}
	q += ` ORDER BY seq ASC LIMIT 10000`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list file edit ledger: %w", err)
	}
	defer rows.Close()

	var out []*FileEditLedgerRow
	var maxSeq int64
	for rows.Next() {
		r := &FileEditLedgerRow{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.OwnerKind, &r.OwnerID, &r.Seq,
			&r.Path, &r.Kind, &r.UnifiedDiff, &r.BeforeSize, &r.AfterSize,
			&r.BeforeSHA256, &r.AfterSHA256, &r.Tool, &r.IsBinary, &r.Truncated,
			&r.GitConfirmed, &r.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("db: scan file edit ledger: %w", err)
		}
		out = append(out, r)
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// maxSeq must reflect the OWNER's head, not just this page's tail, so a
	// filtered page still reports the true resume point.
	if fromSeq > 0 || len(out) == 0 {
		const q2 = `SELECT COALESCE(MAX(seq),0) FROM file_edit_ledger
			WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3`
		if err := tx.QueryRow(ctx, q2, tenantID, ownerKind, ownerID).Scan(&maxSeq); err != nil {
			return nil, 0, fmt.Errorf("db: file edit ledger max seq: %w", err)
		}
	}
	return out, maxSeq, nil
}

// LastObservedFileEdit returns the newest ledger row for one owner+path —
// the "before" side for the next edit of that path (the opencode built-in
// tool observer). Returns nil when the owner never touched the path.
func LastObservedFileEdit(ctx context.Context, tx pgx.Tx, tenantID, ownerKind, ownerID, path string) (*FileEditLedgerRow, error) {
	const q = `SELECT id, tenant_id, owner_kind, owner_id, seq, path, kind, unified_diff,
		before_size, after_size, before_sha256, after_sha256, tool,
		is_binary, truncated, git_confirmed, created_at
		FROM file_edit_ledger
		WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3 AND path=$4
		ORDER BY seq DESC LIMIT 1`
	r := &FileEditLedgerRow{}
	err := tx.QueryRow(ctx, q, tenantID, ownerKind, ownerID, path).Scan(
		&r.ID, &r.TenantID, &r.OwnerKind, &r.OwnerID, &r.Seq,
		&r.Path, &r.Kind, &r.UnifiedDiff, &r.BeforeSize, &r.AfterSize,
		&r.BeforeSHA256, &r.AfterSHA256, &r.Tool, &r.IsBinary, &r.Truncated,
		&r.GitConfirmed, &r.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: last observed file edit: %w", err)
	}
	return r, nil
}

// MarkFileEditsGitConfirmed flips git_confirmed=TRUE for every unconfirmed
// row of an owner whose path is in the given set (the git reconciliation
// match step). Returns the number of rows updated.
func MarkFileEditsGitConfirmed(ctx context.Context, tx pgx.Tx, tenantID, ownerKind, ownerID string, paths []string) (int64, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx,
		`UPDATE file_edit_ledger SET git_confirmed = TRUE
		 WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3
		   AND git_confirmed = FALSE AND path = ANY($4)`,
		tenantID, ownerKind, ownerID, paths)
	if err != nil {
		return 0, fmt.Errorf("db: mark file edits git confirmed: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LedgerPaths returns the distinct paths an owner's ledger has recorded —
// the "known" set the reconciliation compares git's changed set against.
func LedgerPaths(ctx context.Context, tx pgx.Tx, tenantID, ownerKind, ownerID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT path FROM file_edit_ledger
		 WHERE tenant_id=$1 AND owner_kind=$2 AND owner_id=$3`,
		tenantID, ownerKind, ownerID)
	if err != nil {
		return nil, fmt.Errorf("db: ledger paths: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
