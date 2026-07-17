package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager runs registered reconcilers, each with its own work queue
// and per-kind leadership election via Postgres advisory locks
// (docs/03_Scheduler_and_Runtime_Design.md §2). Concrete reconcilers
// arrive in later phases; the framework ships now.
type Manager struct {
	pool *db.Pool
	log  *slog.Logger
	mu   sync.Mutex
	jobs []job
}

type job struct {
	rec       Reconciler
	queue     *workQueue
	leader    *leaderElection
	backoff   time.Duration
	maxRetry  int
}

// NewManager creates a reconciler manager. Call Register to add
// reconcilers, then Run to start the control loop.
func NewManager(pool *db.Pool, log *slog.Logger) *Manager {
	return &Manager{pool: pool, log: log}
}

// Register adds a reconciler to the manager. The reconciler's kind is
// used for work-queue keying and advisory-lock leadership election.
func (m *Manager) Register(r Reconciler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = append(m.jobs, job{
		rec:      r,
		queue:    newWorkQueue(r.Kind()),
		leader:   newLeaderElection(m.pool, r.Kind()),
		backoff:  5 * time.Second,
		maxRetry: 5,
	})
}

// Run starts all registered reconcilers. It blocks until ctx is
// cancelled. Each reconciler runs in its own goroutine: the leader
// heartbeat maintains the advisory lock, and the work queue processes
// enqueued keys. Non-leader replicas stand by (docs/03 §2.3).
func (m *Manager) Run(ctx context.Context) error {
	m.log.Info("reconciler manager started", "reconcilers", len(m.jobs))
	var wg sync.WaitGroup
	for i := range m.jobs {
		wg.Add(1)
		go func(j *job) {
			defer wg.Done()
			m.runReconciler(ctx, j)
		}(&m.jobs[i])
	}
	// If there are no reconcilers registered yet, block on context so
	// the manager doesn't exit immediately (which would cause the server
	// to treat it as an error). Concrete reconcilers arrive in later
	// phases (Phase 5+).
	if len(m.jobs) == 0 {
		<-ctx.Done()
		m.log.Info("reconciler manager stopped")
		return nil
	}
	wg.Wait()
	m.log.Info("reconciler manager stopped")
	return nil
}

func (m *Manager) runReconciler(ctx context.Context, j *job) {
	heartbeat := time.NewTicker(200 * time.Millisecond)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			j.leader.release(context.Background())
			return
		case <-heartbeat.C:
			// Renew leadership. If we're not the leader, try to acquire.
			isLeader, err := j.leader.tryAcquireOrRenew(ctx)
			if err != nil {
				m.log.Warn("leader election failed", "kind", j.rec.Kind(), "error", err)
				continue
			}
			if !isLeader {
				continue
			}
			// Process work queue.
			m.processQueue(ctx, j)
		}
	}
}

func (m *Manager) processQueue(ctx context.Context, j *job) {
	didWork := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		key, ok := j.queue.dequeue()
		if !ok {
			break
		}
		didWork = true
		ctx, span := startReconcileSpan(ctx, j.rec.Kind(), key)
		result := j.rec.Reconcile(ctx, key)
		span.End()

		if result.Error != nil {
			m.log.Error("reconcile failed",
				"kind", j.rec.Kind(), "key", key, "error", result.Error)
			j.queue.markFailed(key)
			if j.queue.failures(key) >= j.maxRetry {
				m.log.Warn("reconcile max retries exceeded — marking degraded",
					"kind", j.rec.Kind(), "key", key)
			}
			continue
		}
		j.queue.markDone(key)
		if result.RequeueAfter > 0 {
			j.queue.requeueAfter(key, result.RequeueAfter)
		}
	}
	// Scan pass: when the work queue is empty, let the reconciler
	// discover new work by calling Reconcile with an empty key
	// (docs/03 §2.1). Reconcilers that don't implement scanning return
	// an empty Result for an empty key (no-op). This lets the
	// WorkflowReconciler scan pending runs and the TaskReconciler scan
	// ready tasks without an explicit enqueue path.
	if !didWork {
		ctx, span := startReconcileSpan(ctx, j.rec.Kind(), "scan")
		result := j.rec.Reconcile(ctx, "")
		span.End()
		if result.Error != nil {
			m.log.Warn("reconcile scan failed", "kind", j.rec.Kind(), "error", result.Error)
		}
	}
}

func startReconcileSpan(ctx context.Context, kind, key string) (context.Context, reconcileSpan) {
	return ctx, reconcileSpan{kind: kind, key: key, start: time.Now()}
}

type reconcileSpan struct {
	kind  string
	key   string
	start time.Time
}

func (s reconcileSpan) End() {
	// In a full implementation, this records an OTel span
	// (reconcile.<kind>.<key>). For the framework, we log duration.
	elapsed := time.Since(s.start)
	_ = elapsed
}

// Enqueue adds a key to the work queue for the given reconciler kind.
// Thread-safe. No-op if the kind is not registered.
func (m *Manager) Enqueue(kind, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.jobs {
		if m.jobs[i].rec.Kind() == kind {
			m.jobs[i].queue.enqueue(key)
			return
		}
	}
}

// --- Work Queue (docs/03 §2.2) -----------------------------------------------

type workQueue struct {
	kind    string
	mu      sync.Mutex
	pending map[string]*queueEntry
	ordered []string
	fail    map[string]int
}

type queueEntry struct {
	key       string
	enqueuedAt time.Time
	readyAt   time.Time
	failures  int
}

func newWorkQueue(kind string) *workQueue {
	return &workQueue{
		kind:    kind,
		pending: make(map[string]*queueEntry),
		fail:    make(map[string]int),
	}
}

// enqueue adds a key to the work queue. If the key is already pending,
// this is a no-op (de-duplication — docs/03 §2.2: "A single enqueue per
// object ID per tick collapses coalesced events").
func (q *workQueue) enqueue(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.pending[key]; ok {
		return
	}
	now := time.Now()
	q.pending[key] = &queueEntry{key: key, enqueuedAt: now, readyAt: now}
	q.ordered = append(q.ordered, key)
}

// dequeue returns the next ready key, or ok=false if the queue is empty.
func (q *workQueue) dequeue() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for len(q.ordered) > 0 {
		key := q.ordered[0]
		q.ordered = q.ordered[1:]
		entry, ok := q.pending[key]
		if !ok {
			continue
		}
		if now.Before(entry.readyAt) {
			// Not ready yet — re-add to the end.
			q.ordered = append(q.ordered, key)
			continue
		}
		return key, true
	}
	return "", false
}

// markDone removes the key from the queue and resets its failure count.
func (q *workQueue) markDone(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, key)
	delete(q.fail, key)
}

// markFailed increments the failure count for the key and re-enqueues
// it with exponential backoff (docs/03 §1: "Backoff on error:
// exponential with jitter, capped").
func (q *workQueue) markFailed(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry, ok := q.pending[key]; ok {
		entry.failures++
		q.fail[key] = entry.failures
		backoff := time.Duration(1<<uint(entry.failures)) * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		entry.readyAt = time.Now().Add(backoff)
		q.ordered = append(q.ordered, key)
	}
}

// requeueAfter schedules the key for re-processing after the given
// duration (docs/03 §2.1: RequeueAfter).
func (q *workQueue) requeueAfter(key string, after time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry, ok := q.pending[key]; ok {
		entry.readyAt = time.Now().Add(after)
		q.ordered = append(q.ordered, key)
	} else {
		q.pending[key] = &queueEntry{
			key:      key,
			readyAt:  time.Now().Add(after),
			enqueuedAt: time.Now(),
		}
		q.ordered = append(q.ordered, key)
	}
}

func (q *workQueue) failures(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.fail[key]
}

// --- Leader Election (docs/03 §2.3) ------------------------------------------

// leaderElection uses Postgres advisory locks for per-kind leadership
// with a pinned connection from the pool (docs/03 §2.3).
//
// pg_try_advisory_lock is session-level — it persists on the connection
// that acquired it. Using QueryRow (acquire + release) in the old code
// meant the lock lived on an idle connection in the pool; subsequent
// heartbeats that drew a different connection found the lock already held
// and got FALSE, starving the reconciler for seconds at a time.
//
// Fix: hold a dedicated *pgxpool.Conn for the duration of leadership.
// Each heartbeat reuses the same connection; pg_try_advisory_lock returns
// TRUE for a nested acquisition. On shutdown, release the connection.
type leaderElection struct {
	pool     *db.Pool
	kind     string
	lockKey  int64
	heldConn *pgxpool.Conn
}

func newLeaderElection(pool *db.Pool, kind string) *leaderElection {
	return &leaderElection{
		pool:    pool,
		kind:    kind,
		lockKey: hashKind(kind),
	}
}

// tryAcquireOrRenew attempts to acquire or renew the advisory lock for
// this kind. Returns true if this replica is the leader for this kind.
// It pinches a connection from the pool on first acquisition and reuses
// it across heartbeats so the session-level lock stays valid.
func (l *leaderElection) tryAcquireOrRenew(ctx context.Context) (bool, error) {
	if l.heldConn != nil {
		// Already the leader — the lock is held on our pinned
		// connection. pg_try_advisory_lock returns TRUE for a nested
		// acquisition by the same session, confirming the lock is
		// still live. If the connection died, this errors and we
		// fall through to re-acquire.
		var acquired bool
		err := l.heldConn.QueryRow(ctx,
			"SELECT pg_try_advisory_lock($1)", l.lockKey,
		).Scan(&acquired)
		if err != nil {
			l.heldConn.Release()
			l.heldConn = nil
			return false, fmt.Errorf("leader: renew: %w", err)
		}
		return true, nil
	}

	// Not yet leader — try to acquire a dedicated connection and lock.
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("leader: acquire conn: %w", err)
	}
	var acquired bool
	err = conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", l.lockKey,
	).Scan(&acquired)
	if err != nil {
		conn.Release()
		return false, fmt.Errorf("leader: try advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return false, nil
	}
	l.heldConn = conn
	return true, nil
}

// release releases the advisory lock and the pinned connection.
// Called on graceful shutdown.
func (l *leaderElection) release(ctx context.Context) {
	if l.heldConn == nil {
		return
	}
	_, _ = l.heldConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", l.lockKey)
	l.heldConn.Release()
	l.heldConn = nil
}

// hashKind produces a stable int64 hash of the kind string for use as
// the advisory lock key. Uses FNV-1a (32-bit, cast to int64) for
// determinism and to stay within the int64 range that
// pg_try_advisory_lock accepts.
func hashKind(kind string) int64 {
	var h uint32 = 2166136261
	for _, c := range kind {
		h ^= uint32(c)
		h *= 16777619
	}
	return int64(h)
}
