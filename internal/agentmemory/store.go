// Package agentmemory is the durable cross-session agent-memory channel
// for native Orchicon worker sessions: a per-project SQLite FTS5 store at
// <project_dir>/.orchicon/memory.db.
//
// # SQLite-vs-Postgres decision
//
// Memory is per-PROJECT durable knowledge (cross-session, cross-execution)
// and the project directory is where the JSONL transcripts, session and
// offload artifacts already live — a memory DB beside them inherits the
// same backup/restore/delete lifecycle. Postgres is the CONTROL-PLANE
// database: a memory tool executing inside a worker would need a Postgres
// connection from the runtime container to the plane DB — a new
// cross-boundary channel with secrets/network/tenant plumbing per
// execution. SQLite is a local file: zero new channels, and it works in
// the sandbox and in every runtime image. FTS5 (bm25 ranked full-text
// search, phrase queries, highlight) is built into SQLite, so ranked
// search needs zero extra services. A local embedded store has no
// network/credential dependency: a write either lands in the file or
// errors — it can never silently look alive via a remote fallback. The
// trade-offs (single-writer file, no control-plane UI reader) are
// accepted: memory is project-scoped and the memory tools ARE the query
// surface.
//
// The package is a leaf: it imports database/sql + modernc.org/sqlite
// only, never an adapter/scheduler/orchicon package, so both the native
// engine and any future tool-suite can use it without import cycles.
//
// # Isolation semantics
//
// Entries are filtered by tenant_id + project_dir on EVERY query (SQL
// level). Cross-session reads work because the DB lives at the true
// project dir (not the per-run worktree) and later executions of the same
// tenant+project read earlier entries through the same filters.
package agentmemory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// maxBodyHintBytes caps how much of a body is stored when a caller passes
// an oversized body (defensive; the tool layer caps earlier).
const maxBodyHintBytes = 64 * 1024

// schema creates the FTS5 virtual table. Text columns (title, body, tags)
// are indexed; metadata columns are UNINDEXED (stored but not searchable).
const schema = `CREATE VIRTUAL TABLE IF NOT EXISTS memory_entries USING fts5(
  title,
  body,
  tags,
  created_at UNINDEXED,
  updated_at UNINDEXED,
  execution_id UNINDEXED,
  worker_id UNINDEXED,
  tenant_id UNINDEXED,
  project_dir UNINDEXED,
  tokenize = 'porter unicode61'
);`

// ErrNotFound is returned when a memory entry id does not exist (or is
// filtered out by the tenant/project isolation filters).
var ErrNotFound = errors.New("agentmemory: entry not found")

// Entry is one memory entry as returned by Read/Search/List.
type Entry struct {
	ID          int64
	Title       string
	Body        string
	Tags        []string
	Snippet     string // search only: highlighted body snippet
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExecutionID string
	WorkerID    string
}

// WriteInput describes a memory write (create or replace).
type WriteInput struct {
	TenantID    string
	ProjectDir  string
	ExecutionID string
	WorkerID    string
	Title       string
	Body        string
	Tags        []string
	// ID > 0 replaces the existing entry with that id (delete + re-insert).
	ID int64
}

// Store is one project's memory store (a single *sql.DB on the project's
// memory.db). Writes are serialized by a mutex (single-writer file);
// reads are concurrent.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// entryCols is the non-snippet SELECT column list shared by every query.
const entryCols = `rowid, title, body, tags, created_at, updated_at, execution_id, worker_id`

// Open opens (creating and migrating if needed) the memory DB at
// <projectDir>/.orchicon/memory.db. The .orchicon directory is created if
// missing. It returns an error only when the file cannot be created or the
// schema cannot be applied — never a silent in-memory fallback.
func Open(projectDir string) (*Store, error) {
	dir := filepath.Join(projectDir, ".orchicon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentmemory: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "memory.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("agentmemory: open %s: %w", path, err)
	}
	// A local single-writer file: WAL keeps readers non-blocking; a busy
	// timeout prevents transient lock contention from failing a tool call.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("agentmemory: pragma: %w", err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("agentmemory: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Write creates a new entry (in.ID <= 0) or replaces an existing one
// (in.ID > 0). An FTS5 row cannot be updated in place, so a replace is a
// delete + re-insert inside one transaction (the new row gets a fresh
// rowid — callers must use the returned id, never assume id stability).
// The returned id is the rowid of the written row.
func (s *Store) Write(ctx context.Context, in WriteInput) (int64, error) {
	if strings.TrimSpace(in.TenantID) == "" {
		return 0, fmt.Errorf("agentmemory: tenant_id is required")
	}
	if strings.TrimSpace(in.ProjectDir) == "" {
		return 0, fmt.Errorf("agentmemory: project_dir is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return 0, fmt.Errorf("agentmemory: title is required")
	}
	body := in.Body
	if len(body) > maxBodyHintBytes {
		body = body[:maxBodyHintBytes]
	}
	tags := ""
	if len(in.Tags) > 0 {
		tags = strings.Join(in.Tags, " ")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if in.ID > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM memory_entries WHERE rowid = ? AND tenant_id = ? AND project_dir = ?`,
			in.ID, in.TenantID, in.ProjectDir); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO memory_entries(title, body, tags, created_at, updated_at, execution_id, worker_id, tenant_id, project_dir)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		title, body, tags, now, now, in.ExecutionID, in.WorkerID, in.TenantID, in.ProjectDir)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// scanEntry scans entryCols (plus any extra dests) into an Entry. extra
// destinations are appended to the single Scan call so queries with a
// trailing snippet column pass one destination per result column.
func scanEntry(scan func(dest ...any) error, e *Entry, extra ...any) error {
	var tags, created, updated string
	dests := []any{&e.ID, &e.Title, &e.Body, &tags, &created, &updated,
		&e.ExecutionID, &e.WorkerID}
	dests = append(dests, extra...)
	if err := scan(dests...); err != nil {
		return err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339, created)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	if strings.TrimSpace(tags) != "" {
		e.Tags = strings.Fields(tags)
	}
	return nil
}

// Search ranks entries by bm25 against a plain-text query. The query is
// bound as a single FTS5 phrase (auto-quoted) so it is always literal —
// raw user text can never inject FTS5 operators (AND/OR/quotes/*) or
// cause a silent syntax error. limit is clamped to [1,50].
func (s *Store) Search(ctx context.Context, tenantID, projectDir, q string, limit int) ([]Entry, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("agentmemory: query is required")
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryCols+`,
		        snippet(memory_entries, 1, '<mark>', '</mark>', '...', 24) AS snippet
		 FROM memory_entries
		 WHERE memory_entries MATCH ? AND tenant_id = ? AND project_dir = ?
		 ORDER BY bm25(memory_entries) LIMIT ?`,
		quotePhrase(q), tenantID, projectDir, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var snippet string
		if err := scanEntry(rows.Scan, &e, &snippet); err != nil {
			return nil, err
		}
		e.Snippet = snippet
		out = append(out, e)
	}
	return out, rows.Err()
}

// Read fetches one entry by id, filtered by tenant + project. Returns
// ErrNotFound when the id does not exist or belongs to another
// tenant/project.
func (s *Store) Read(ctx context.Context, tenantID, projectDir string, id int64) (Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryCols+` FROM memory_entries WHERE rowid = ? AND tenant_id = ? AND project_dir = ?`,
		id, tenantID, projectDir)
	if err != nil {
		return Entry{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Entry{}, ErrNotFound
	}
	var e Entry
	if err := scanEntry(rows.Scan, &e); err != nil {
		return Entry{}, err
	}
	return e, rows.Err()
}

// List returns the most recent entries (rowid DESC), newest first,
// filtered by tenant + project. limit/offset bound the page; limit is
// clamped to [1,50].
func (s *Store) List(ctx context.Context, tenantID, projectDir string, limit, offset int) ([]Entry, error) {
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, title, tags, created_at FROM memory_entries
		 WHERE tenant_id = ? AND project_dir = ?
		 ORDER BY rowid DESC LIMIT ? OFFSET ?`,
		tenantID, projectDir, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var tags, created string
		if err := rows.Scan(&e.ID, &e.Title, &tags, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if strings.TrimSpace(tags) != "" {
			e.Tags = strings.Fields(tags)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes one entry by id, filtered by tenant + project. Returns
// ErrNotFound when no row matched.
func (s *Store) Delete(ctx context.Context, tenantID, projectDir string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_entries WHERE rowid = ? AND tenant_id = ? AND project_dir = ?`,
		id, tenantID, projectDir)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// quotePhrase wraps a plain-text query as a single FTS5 phrase so it is
// matched literally: the raw string is escaped by doubling any embedded
// double quotes, then surrounded by double quotes. This prevents FTS5
// operator injection and silent syntax errors from arbitrary user text.
func quotePhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}
