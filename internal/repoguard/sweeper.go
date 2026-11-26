package repoguard

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
)

// RefreshInterval is how often the list of project directories is re-read from the database. The check itself
// reads one small file per project and runs on the caller's (much shorter) tick.
const RefreshInterval = 60 * time.Second

// AuditAction is the audit event written when the guard heals a project.
const AuditAction = "repo.config_healed"

type target struct {
	tenantID  string
	projectID string
	dir       string
}

// Sweeper checks every active project's own checkout and heals platform-originated poisoning.
type Sweeper struct {
	pool *db.Pool
	log  *slog.Logger
	now  func() time.Time

	mu        sync.Mutex
	targets   []target
	refreshed time.Time
}

// NewSweeper builds the sweeper. A nil logger uses slog's default.
func NewSweeper(pool *db.Pool, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{pool: pool, log: log, now: time.Now}
}

// refresh re-reads (tenant, project, dir) triples, at most once per RefreshInterval. A directory shared by
// several projects is checked once.
func (s *Sweeper) refresh(ctx context.Context) {
	s.mu.Lock()
	if !s.refreshed.IsZero() && s.now().Sub(s.refreshed) < RefreshInterval {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	var out []target
	seen := map[string]bool{}
	const pageSize = 200
	afterTenant := ""
	for {
		tenants, err := db.ListTenants(ctx, s.pool, pageSize, afterTenant)
		if err != nil {
			s.log.Warn("repo guard: list tenants failed", "error", err)
			return
		}
		if len(tenants) == 0 {
			break
		}
		for _, t := range tenants {
			ttx, err := s.pool.BeginTenantTx(ctx, t.ID)
			if err != nil {
				s.log.Warn("repo guard: tenant tx failed", "tenant", t.ID, "error", err)
				continue
			}
			afterProject := ""
			for {
				rows, lerr := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
					TenantID: t.ID, ExcludeStatuses: []string{"archived"}, PageSize: 500, AfterID: afterProject,
				})
				if lerr != nil {
					s.log.Warn("repo guard: list projects failed", "tenant", t.ID, "error", lerr)
					break
				}
				for _, p := range rows {
					if p.ProjectDir != "" && !seen[p.ProjectDir] {
						seen[p.ProjectDir] = true
						out = append(out, target{tenantID: t.ID, projectID: p.ID, dir: p.ProjectDir})
					}
				}
				if len(rows) < 500 {
					break
				}
				afterProject = rows[len(rows)-1].ID
			}
			_ = ttx.Rollback(ctx)
		}
		if len(tenants) < pageSize {
			break
		}
		afterTenant = tenants[len(tenants)-1].ID
	}

	s.mu.Lock()
	s.targets, s.refreshed = out, s.now()
	s.mu.Unlock()
}

// Sweep checks every known project once and returns how many it healed.
func (s *Sweeper) Sweep(ctx context.Context) int {
	s.refresh(ctx)
	s.mu.Lock()
	targets := append([]target(nil), s.targets...)
	s.mu.Unlock()

	healed := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			return healed
		}
		res := Check(ctx, t.dir)
		if len(res.Findings) == 0 {
			continue
		}
		s.alert(ctx, t, res)
		if res.Healed {
			healed++
		}
	}
	return healed
}

// alert says what was found, loudly, and records it. The log line carries the value that was removed and the run
// it named: that run is the first suspect, and the only evidence of who wrote it that survives the repair.
func (s *Sweeper) alert(ctx context.Context, t target, res Result) {
	runs := []string{}
	for _, f := range res.Findings {
		if f.Run != "" {
			runs = append(runs, f.Run)
		}
	}
	attrs := []any{"project", t.projectID, "dir", t.dir, "findings", res.Findings, "runs", runs, "healed", res.Healed}
	if res.Err != nil {
		s.log.Error("REPO GUARD: a project's own git config is poisoned and could NOT be repaired — "+
			"git in the operator's checkout is acting on a worker's worktree", append(attrs, "error", res.Err)...)
	} else {
		s.log.Warn("REPO GUARD: a project's own git config was poisoned by a run and has been repaired "+
			"(core.worktree / fixture identity removed). Something wrote to the operator's checkout config — "+
			"see 'runs' for the worktree it named", attrs...)
	}

	before, _ := json.Marshal(res.Findings)
	after, _ := json.Marshal(map[string]any{"healed": res.Healed, "error": errString(res.Err)})
	ttx, err := s.pool.BeginTenantTx(ctx, t.tenantID)
	if err != nil {
		s.log.Warn("repo guard: audit tx failed", "project", t.projectID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID: t.tenantID, ActorType: "system", Action: AuditAction,
		TargetType: "project", TargetID: t.projectID, Before: before, After: after,
	}); err != nil {
		s.log.Warn("repo guard: audit record failed", "project", t.projectID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Warn("repo guard: audit commit failed", "project", t.projectID, "error", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
