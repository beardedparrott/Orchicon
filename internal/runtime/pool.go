package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Warm pool of runtime containers.
//
// Containers are keyed by ENVIRONMENT (image + project mounts): runs for
// the same project reuse a pre-warmed, serve-proven container instead of
// cold-starting a fresh one per run. Leases are exclusive per run — a
// container is handed to exactly one run at a time and is never shared.
// When a run ends, the container is reset in the BACKGROUND (rm -f +
// recreate with the identical spec + warm the serve) so the pool only ever
// hands out PRISTINE environments: nothing from the previous run's state
// (installed packages, /tmp, opencode sessions/data) crosses the boundary,
// which preserves the security property that motivated the sandbox. The
// reset runs off the dispatch path, so a checkout never blocks on it — a
// miss just creates fresh.
//
// The pool is daemon-resident and memory-only: a daemon restart resets it
// (all containers are removed at start) and the plane's run-start gate +
// adopt pass re-lease for active runs.
type daemonPool struct {
	d  *Daemon
	mu sync.Mutex
	// entries is the full inventory by container name.
	entries map[string]*poolEntry
	// clean lists available (reset, warm) container names per environment.
	clean map[string][]string
	// leased maps an active run id to its container name.
	leased map[string]string
}

// poolEntry is the pool's bookkeeping for one container.
type poolEntry struct {
	name       string
	envKey     string
	image      string
	mounts     []MountSpec
	serveCfg   string
	projectDir string
	// servePort/PW/URL are the published serve creds (set once the serve is
	// up; reused on idempotent checkouts).
	servePort     int
	servePassword string
	serveURL      string
	// planeURL is the sandbox plane /healthz base URL (empty on base/gui
	// images — no plane boot).
	planeURL string
	leasedBy string // run id; "" = available
	lastUsed time.Time
}

func newDaemonPool(d *Daemon) *daemonPool {
	return &daemonPool{
		d:       d,
		entries: make(map[string]*poolEntry),
		clean:   make(map[string][]string),
		leased:  make(map[string]string),
	}
}

// poolEnvKey derives the environment key for a create request: the image +
// the sorted mount set + a fingerprint of the read-once HOST inputs baked
// into every container at create time (opencode config/auth, the adapter
// install, the resolved GH token — see hostfp.go). Runs with the same image +
// mounts + host inputs share a pool; different environments never share a
// container (bind mounts are fixed at create time and cannot change on a
// running container, and the serve caches the host inputs at boot). hostFp ==
// "" folds away — the key then reduces to image + mounts (no behavior change
// when HostHome is unset / nothing applies).
func poolEnvKey(req CreateRequest, hostFp string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "img="+req.Image+"\n")
	mounts := append([]MountSpec(nil), req.Mounts...)
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Source != mounts[j].Source {
			return mounts[i].Source < mounts[j].Source
		}
		return mounts[i].Dest < mounts[j].Dest
	})
	for _, m := range mounts {
		_, _ = io.WriteString(h, fmt.Sprintf("%s:%s\n", m.Source, m.Dest))
	}
	// The serve config (OPENCODE_CONFIG_CONTENT) is baked into the
	// container at create time and CANNOT change on a live container: it
	// carries the worktree MCP base dir, the plane channel env, and the
	// permission rules — all run-scoped. A warm pooled container whose
	// serve config differs from the requesting run's MUST NOT be reused:
	// e.g. a container baked before the plane channel existed (or with a
	// different run's worktree base / plane token) would serve stale,
	// wrong-scope credentials and tools. Fold the config into the env key
	// so runs needing a different serve config get a fresh container with
	// their own baked config. resetAndPool recomputes the key from the
	// entry's OWN serveCfg, so pooled entries stay consistently keyed.
	if req.ServeConfig != "" {
		_, _ = io.WriteString(h, "cfg="+req.ServeConfig+"\n")
	}
	if hostFp != "" {
		_, _ = io.WriteString(h, "host="+hostFp+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// poolName builds a unique container name for a pool environment. Names are
// NOT run-derived — a container is reused across runs of the same env.
func poolName(envKey string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("orchicon-runtime-%s-%s", envKey, hex.EncodeToString(b))
}

// checkout leases a container for a run: the run's existing lease (if any),
// a clean pooled container for the environment (verified up + serving), or a
// freshly created one. Idempotent per run id — the run-start gate and the
// adapter's dispatch-time self-heal both call Create, and both converge to
// the same lease. Returns the serve creds (0/"" if the daemon could not
// bring the serve up — the caller fails the run rather than dispatching).
func (p *daemonPool) checkout(ctx context.Context, runID string, req CreateRequest) (*CreateResponse, error) {
	if runID == "" {
		return nil, fmt.Errorf("pool checkout: run id required")
	}
	p.mu.Lock()
	if name, ok := p.leased[runID]; ok {
		ent := p.entries[name]
		p.mu.Unlock()
		if ent != nil {
			return ent.response(), nil
		}
		// Lease points at a dropped entry (shouldn't happen) — clear it and
		// fall through to a fresh lease.
		p.mu.Lock()
		delete(p.leased, runID)
		p.mu.Unlock()
	} else {
		p.mu.Unlock()
	}

	key := poolEnvKey(req, p.d.hostInputsFingerprint())
	p.mu.Lock()
	var chosen *poolEntry
	if names := p.clean[key]; len(names) > 0 {
		name := names[len(names)-1]
		p.clean[key] = names[:len(names)-1]
		ent := p.entries[name]
		if ent != nil {
			ent.leasedBy = runID
			ent.lastUsed = time.Now()
			p.leased[runID] = name
			chosen = ent
		}
	}
	p.mu.Unlock()

	if chosen != nil {
		// A clean pooled container must still be running + serving; a
		// wedged/exited one (e.g. reset raced a crash) is dropped and the
		// run gets a fresh create instead.
		running, _ := p.d.containerRunning(chosen.name)
		if running && serveUsableAt(chosen.serveURL, chosen.servePassword) {
			p.d.Log.Info("pool checkout: reused warm container", "run", runID, "container", chosen.name)
			return chosen.response(), nil
		}
		p.mu.Lock()
		delete(p.entries, chosen.name)
		delete(p.leased, runID)
		p.mu.Unlock()
		_, _ = p.d.docker("rm", "-f", chosen.name)
	}

	resp, err := p.d.createContainer(poolName(key), req)
	if err != nil {
		return nil, err
	}
	// A concurrent checkout for the SAME run may have won the create race
	// while we were inside createContainer (both saw no lease). If a lease
	// now exists, drop the container we just made and hand back the winner's
	// — never two containers for one run.
	p.mu.Lock()
	if winnerName, ok := p.leased[runID]; ok && winnerName != resp.Name {
		winner := p.entries[winnerName]
		p.mu.Unlock()
		if winner != nil {
			_, _ = p.d.docker("rm", "-f", resp.Name)
			return winner.response(), nil
		}
		p.mu.Lock()
		delete(p.leased, runID)
	}
	ent := &poolEntry{
		name:          resp.Name,
		envKey:        key,
		image:         req.Image,
		mounts:        req.Mounts,
		serveCfg:      req.ServeConfig,
		projectDir:    req.ProjectDir,
		servePort:     resp.ServePort,
		servePassword: resp.ServePassword,
		serveURL:      resp.ServeURL,
		planeURL:      resp.PlaneURL,
		leasedBy:      runID,
		lastUsed:      time.Now(),
	}
	p.entries[ent.name] = ent
	p.leased[runID] = ent.name
	p.mu.Unlock()
	p.d.Log.Info("pool checkout: created fresh container", "run", runID, "container", ent.name)
	return resp, nil
}

// release ends a run's lease: the container is removed and reset in the
// background (fresh container + warm serve) back into the pool — the run's
// environment never touches the next run's. The reset is off the dispatch
// path; a checkout that finds the pool empty just creates fresh.
func (p *daemonPool) release(runID string) {
	p.mu.Lock()
	name, ok := p.leased[runID]
	if !ok {
		p.mu.Unlock()
		return
	}
	ent := p.entries[name]
	delete(p.leased, runID)
	if ent != nil {
		ent.leasedBy = ""
	}
	p.mu.Unlock()
	if ent == nil {
		return
	}
	p.d.Log.Info("pool release: resetting run container", "run", runID, "container", ent.name)
	go p.resetAndPool(ent)
}

// resetAndPool recreates a released container fresh (pristine + warm serve)
// and returns it to the clean pool, respecting the per-env cap. A failed
// reset is dropped — the next checkout creates fresh. The released
// container is removed FIRST so a reset never leaks its predecessor.
func (p *daemonPool) resetAndPool(old *poolEntry) {
	_, _ = p.d.docker("rm", "-f", old.name)
	// Re-derive the env key from the CURRENT host state instead of reusing
	// old.envKey: a read-once host input (config/auth/adapter/token) that
	// changed while the run was leased must land the reset container under
	// the NEW key — under the old key it would idle-reap unused while the
	// next checkout created fresh anyway.
	req := CreateRequest{
		Image:       old.image,
		Mounts:      old.mounts,
		ServeConfig: old.serveCfg,
		ProjectDir:  old.projectDir,
	}
	envKey := poolEnvKey(req, p.d.hostInputsFingerprint())
	name := poolName(envKey)
	resp, err := p.d.createContainer(name, req)
	if err != nil {
		p.d.Log.Warn("pool reset failed — dropping container", "container", old.name, "error", err)
		return
	}
	ent := &poolEntry{
		name:          name,
		envKey:        envKey,
		image:         old.image,
		mounts:        old.mounts,
		serveCfg:      old.serveCfg,
		projectDir:    old.projectDir,
		servePort:     resp.ServePort,
		servePassword: resp.ServePassword,
		serveURL:      resp.ServeURL,
		planeURL:      resp.PlaneURL,
		lastUsed:      time.Now(),
	}
	p.mu.Lock()
	overCap := len(p.clean[ent.envKey]) >= p.poolCap()
	if !overCap {
		p.entries[name] = ent
		p.clean[ent.envKey] = append(p.clean[ent.envKey], name)
	}
	p.mu.Unlock()
	if overCap {
		// At the per-env cap: drop the freshly-reset container (never a
		// docker call under the pool lock).
		p.d.Log.Info("pool reset: at per-env cap, dropping container", "env", ent.envKey, "container", name)
		_, _ = p.d.docker("rm", "-f", name)
	}
}

// response builds the CreateResponse for an existing entry.
func (e *poolEntry) response() *CreateResponse {
	return &CreateResponse{
		Name:          e.name,
		Running:       true,
		ServePort:     e.servePort,
		ServePassword: e.servePassword,
		ServeURL:      e.serveURL,
		PlaneURL:      e.planeURL,
	}
}

// idleReap removes clean containers that have been unused past the idle
// window, keeping the pool bounded across many projects/images. Leased
// containers are never touched.
func (p *daemonPool) idleReap() {
	p.mu.Lock()
	var stale []*poolEntry
	for envKey, names := range p.clean {
		kept := names[:0]
		for _, name := range names {
			ent := p.entries[name]
			if ent != nil && time.Since(ent.lastUsed) > p.idleWindow() {
				stale = append(stale, ent)
			} else {
				kept = append(kept, name)
			}
		}
		p.clean[envKey] = kept
	}
	p.mu.Unlock()
	for _, ent := range stale {
		p.d.Log.Info("pool idle reap: removing warm container", "env", ent.envKey, "container", ent.name)
		_, _ = p.d.docker("rm", "-f", ent.name)
		p.mu.Lock()
		delete(p.entries, ent.name)
		p.mu.Unlock()
	}
}

// resetPool removes EVERY runtime container at daemon start. The pool is
// memory-only (leases live in the daemon process), so after a restart the
// plane's run-start gate and adopt pass re-lease for active runs; stale
// containers from before the restart (plane-down leaks included) are gone.
func (p *daemonPool) resetPool() {
	names, err := p.d.listRuntimes("")
	if err != nil {
		p.d.Log.Warn("pool reset: list runtimes", "error", err)
		return
	}
	for _, n := range names {
		p.d.Log.Info("pool reset: removing pre-restart container", "container", n)
		_, _ = p.d.docker("rm", "-f", n)
	}
	p.mu.Lock()
	p.entries = make(map[string]*poolEntry)
	p.clean = make(map[string][]string)
	p.leased = make(map[string]string)
	p.mu.Unlock()
}

// poolIdleWindow resolves ORCHICON_RUNTIME_POOL_IDLE (default 10m).
func (p *daemonPool) idleWindow() time.Duration {
	if v := p.d.envDuration("ORCHICON_RUNTIME_POOL_IDLE"); v > 0 {
		return v
	}
	return 10 * time.Minute
}

// poolCap resolves ORCHICON_RUNTIME_POOL_CAP (default 1) — the max number
// of clean (idle) containers kept per environment.
func (p *daemonPool) poolCap() int {
	if v := p.d.envInt("ORCHICON_RUNTIME_POOL_CAP", 1); v > 0 {
		return v
	}
	return 1
}
