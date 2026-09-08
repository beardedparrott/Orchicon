package fileedit

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/db"
)

// maxObservedContent caps the in-memory "before" cache per path (the
// observer's snapshot). Files larger than this diff against git HEAD instead
// of the cached content.
const maxObservedContent = 1 << 20 // 1 MiB

// Store is the durable half of the ledger: it persists entries and reads
// them back. The server wires a Postgres-backed implementation; tests use
// fakes. Kept narrow so fileedit stays a leaf package. Implementations ALSO
// implement StoreExtras (reconcile.go) — the split keeps the record path
// minimal and the reconcile path explicit.
type Store interface {
	// Append persists entries for an owner in one transaction, allocating
	// the per-owner seq. Returns the rows with seq/id assigned.
	Append(ctx context.Context, tenantID, ownerKind, ownerID string, entries []Entry) ([]*db.FileEditLedgerRow, error)
}

// Hook is the adapter-facing callback: invoked on every mutating tool_use
// event with the owner context and the entries to ledger. Nil-safe on the
// adapter side (nil hook = no ledger).
type Hook func(ctx context.Context, ownerKind, ownerID string, entries []Entry)

// Service is the pipeline assembly: parse (engine outputs / snapshots) →
// diff (already computed) → ledger persist → fan-out publish. One instance
// serves all owners; it is safe for concurrent use.
type Service struct {
	store Store
	log   *slog.Logger
	// Publisher fans out each persisted ledger row onto the owner's event
	// stream (the execution event channel / Ask turn stream). Optional.
	Publisher func(ownerKind, ownerID string, row *db.FileEditLedgerRow)
}

// NewService wires the pipeline. store may be nil for a publish-only /
// dry-run service (tests); Append then simply returns the entries.
func NewService(store Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, log: log}
}

// Record persists entries for an owner and publishes each row. Best-effort
// by contract: an error is logged, never returned to the event loop (a
// ledger gap must never fail a live session).
//
// No-op entries — path set but an EMPTY unified diff (an identical-content
// rewrite, a create of "", or a binary file flagged as such) — are dropped
// here at the sole record funnel: they carry no diff a renderer can show,
// and persisting them would flood the ledger with empty rows and desync the
// per-owner seq from real edits. Callers (observer / engine-parser / git
// reconciler) may hand them to Record liberally; the funnel is the single
// authority on what becomes a row.
func (s *Service) Record(ctx context.Context, tenantID, ownerKind, ownerID string, entries []Entry) {
	if len(entries) == 0 || s.store == nil {
		return
	}
	filtered := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Path == "" || (e.UnifiedDiff == "" && !e.IsBinary) {
			// No path (nothing to diff) or no diff and not a binary marker:
			// skip — it is a no-op or an outside-base observation.
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		return
	}
	entries = filtered
	rows, err := s.store.Append(ctx, tenantID, ownerKind, ownerID, entries)
	if err != nil {
		s.log.Warn("file edit ledger append failed", "owner", ownerID, "error", err)
		return
	}
	if s.Publisher != nil {
		for _, r := range rows {
			s.Publisher(ownerKind, ownerID, r)
		}
	}
}

// RecordEngineOutput parses a worktree tool's structured output and records
// it. The common execution path: adapter evtToolUse → RecordEngineOutput.
func (s *Service) RecordEngineOutput(ctx context.Context, tenantID, ownerKind, ownerID, tool, toolOutput string) {
	entries, err := ParseEngineOutput(toolOutput)
	if err != nil || len(entries) == 0 {
		return
	}
	ledger := make([]Entry, 0, len(entries))
	for _, e := range entries {
		ledger = append(ledger, EntryFromEngine(e, tool))
	}
	s.Record(ctx, tenantID, ownerKind, ownerID, ledger)
}

// --- snapshot observer (opencode built-in write/edit) -----------------------

// Observer produces ground-truth ledger entries for the opencode built-in
// write/edit tools from server-side file reads: "before" is the last
// observed content for the path (in-memory per-owner cache, seeded from
// `git show HEAD:<path>` when absent), "after" is a fresh read immediately
// after the event. Both sides are real file state.
type Observer struct {
	baseDir string // the owner's execution dir (worktree or project dir)

	mu    sync.Mutex
	cache map[string]observed // worktree-relative path → last seen content
}

type observed struct {
	content []byte
	// fromHead marks a cache entry seeded from git HEAD (a proxy value, not
	// a read of the live file).
	fromHead bool
}

// NewObserver builds an observer rooted at baseDir (the run worktree, or the
// project dir for in-place/Ask sessions).
func NewObserver(baseDir string) *Observer {
	return &Observer{baseDir: baseDir, cache: make(map[string]observed)}
}

// ObserveAfter snapshots the path's current on-disk state, diffs it against
// the last observed content and returns a ledger entry (empty Entry{} when
// nothing changed / the file is outside baseDir). The cache is refreshed
// with the fresh read either way.
func (o *Observer) ObserveAfter(rawPath, tool string) Entry {
	rel := NormalizePath(rawPath)
	if rel == "" || o.baseDir == "" {
		return Entry{}
	}
	abs := filepath.Join(o.baseDir, filepath.FromSlash(rel))
	// Containment: the observed path must stay under the base dir.
	if !strings.HasPrefix(abs, filepath.Clean(o.baseDir)+string(os.PathSeparator)) {
		return Entry{}
	}
	after, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// Deletion observed.
			o.mu.Lock()
			before, had := o.cache[rel]
			delete(o.cache, rel)
			o.mu.Unlock()
			if !had {
				return Entry{}
			}
			return EntryFromSnapshots(before.content, nil, rel, tool)
		}
		return Entry{}
	}

	o.mu.Lock()
	cached, had := o.cache[rel]
	o.mu.Unlock()
	before := cached.content
	if !had {
		// Seed from git HEAD: the committed state is the best available
		// "before" for a first observation mid-session.
		before = gitShowHEAD(o.baseDir, rel)
	}
	o.mu.Lock()
	o.cache[rel] = observed{content: after}
	o.mu.Unlock()

	if len(before) > maxObservedContent {
		before = nil // sizes still recorded; diff bounded by git reconciliation
	}
	e := EntryFromSnapshots(before, after, rel, tool)
	// No-op edit (identical before/after, not a binary marker): the cached
	// "before" equaled the fresh "after" — nothing changed, so no ledger
	// entry. Return the zero Entry so callers' `Path != ""` guard skips it
	// (the Record funnel drops such entries as the second line of defense).
	if e.UnifiedDiff == "" && !e.IsBinary && e.Kind == "" {
		return Entry{}
	}
	return e
}

// gitShowHEAD returns the committed content of rel under dir, or nil.
func gitShowHEAD(dir, rel string) []byte {
	cmd := exec.Command("git", "-C", dir, "show", "HEAD:"+rel)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}
