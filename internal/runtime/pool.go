package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Warm pool of runtime containers.
type daemonPool struct {
	d  *Daemon
	mu sync.Mutex
	// entries is the full inventory by container name.
	entries map[string]*poolEntry
	// clean lists available (reset, warm) container names per environment.
	clean map[string][]string
	// leased maps an active run id to its container name.
	leased map[string]string
	// swept records the instances whose pre-restart containers have already been reconciled since this
	// daemon started, so that costs one listing per instance per daemon lifetime rather than one per.
	swept map[string]bool
	// served records every instance this daemon has leased for.
	served map[string]bool
	// untracked is the two-pass memory the live reconciliation needs: container name → when it was
	// FIRST seen present-but-untracked.
	untracked map[string]orphanCandidate
}

// orphanCandidate is one present-but-untracked container, remembered across reconciliation passes.
type orphanCandidate struct {
	instance string
	since    time.Time
}

// poolEntry is the pool's bookkeeping for one container.
type poolEntry struct {
	name       string
	envKey     string
	image      string
	mounts     []MountSpec
	serveCfg   string
	projectDir string
	// adapterKinds is the run's boot profile (CreateRequest.AdapterKinds), carried so a reset container
	// keeps the same profile.
	adapterKinds []string
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
		leased:    make(map[string]string),
		swept:     make(map[string]bool),
		served:    make(map[string]bool),
		untracked: make(map[string]orphanCandidate),
	}
}

// poolEnvKey derives the environment key for a create request: the image + the sorted mount set + a
// fingerprint of the read-once HOST inputs baked into every container at create time (opencode.
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
	// A warm pooled container whose serve config differs from the requesting run's MUST NOT be reused:
	// e.g. a container baked before the plane channel existed (or with a different run's worktree base.
	if req.ServeConfig != "" {
		_, _ = io.WriteString(h, "cfg="+req.ServeConfig+"\n")
	}
	// The git strategy is a credential-relevant host input: a "none" (ephemeral) container is created
	// WITHOUT a push-capable GH_TOKEN and WITHOUT the git credential mounts.
	if req.GitStrategy != "" {
		_, _ = io.WriteString(h, "gs="+req.GitStrategy+"\n")
	}
	// The BOOT PROFILE (the adapter kinds the run's step workers need) is a credential-relevant host
	// input: it decides which adapter install mounts (the host's model config/auth) the container.
	if req.AdapterKinds != nil {
		_, _ = io.WriteString(h, "demand="+strings.Join(requestedKinds(req), ",")+"\n")
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

// checkout leases a container for a run: the run's existing lease (if any), a clean pooled
// container for the environment (verified up + serving), or a freshly created one.
func (p *daemonPool) checkout(ctx context.Context, runID string, req CreateRequest) (*CreateResponse, error) {
	if runID == "" {
		return nil, fmt.Errorf("pool checkout: run id required")
	}
	// REMEMBER THAT THIS DAEMON SERVES THIS INSTANCE.
	p.mu.Lock()
	if p.served == nil {
		p.served = make(map[string]bool)
	}
	if inst := instanceID(req.InstanceID); inst != "" {
		p.served[inst] = true
	}
	p.mu.Unlock()

	p.mu.Lock()
	if name, ok := p.leased[runID]; ok {
		ent := p.entries[name]
		if ent != nil {
			// Lease renewal: idempotent re-checkouts (the run-start gate, the adapter's dispatch self-heal, and
			// the plane's 30s adopt sweep) keep an ACTIVE run's lease young.
			ent.lastUsed = time.Now()
			p.mu.Unlock()
			return ent.response(), nil
		}
		// Lease points at a dropped entry (shouldn't happen) — clear it and
		// fall through to a fresh lease.
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
		// A clean pooled container must still be running + serving; a wedged/exited one (e.g. reset raced a
		// crash) is dropped and the run gets a fresh create instead.
		running, _ := p.d.containerRunning(chosen.name)
		if running && serveUsableAt(chosen.serveURL, chosen.servePassword) {
			p.d.Log.Info("pool checkout: reused warm container", "run", runID, "container", chosen.name)
			return chosen.response(), nil
		}
		p.mu.Lock()
		delete(p.entries, chosen.name)
		delete(p.leased, runID)
		p.mu.Unlock()
		_ = p.d.removeContainer(chosen.name)
	}

	resp, err := p.d.createContainer(poolName(key), req)
	if err != nil {
		return nil, err
	}
	// A concurrent checkout for the SAME run may have won the create race while we were inside
	// createContainer (both saw no lease).
	p.mu.Lock()
	if winnerName, ok := p.leased[runID]; ok && winnerName != resp.Name {
		winner := p.entries[winnerName]
		p.mu.Unlock()
		if winner != nil {
			_ = p.d.removeContainer(resp.Name)
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
		adapterKinds:  req.AdapterKinds,
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

// containerForRun returns the leased container name for a run ("" when the run holds no lease).
func (p *daemonPool) containerForRun(runID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leased[runID]
}

// release ends a run's lease: the container is removed and reset in the background (fresh container
// + warm serve) back into the pool — the run's environment never touches the next run's.
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

// The replacement enters the clean pool ONLY after its serve answers — a dud would otherwise sit in
// the pool for an idle-reap lifetime.
func (p *daemonPool) resetAndPool(old *poolEntry) {
	_ = p.d.removeContainer(old.name)
	// Re-derive the env key from the CURRENT host state instead of reusing old.envKey: a read-once host
	// input (config/auth/adapter/token) that changed while the run was leased must land the reset.
	req := CreateRequest{
		Image:       old.image,
		Mounts:      old.mounts,
		ServeConfig: old.serveCfg,
		ProjectDir:  old.projectDir,
		// The boot profile MUST survive the reset: dropping it would (a) re-create a native-only container
		// with opencode mounts and (b) re-key the container under the wrong environment (AC 4).
		AdapterKinds: old.adapterKinds,
	}
	envKey := poolEnvKey(req, p.d.hostInputsFingerprint())
	name := poolName(envKey)
	resp, err := p.d.createContainer(name, req)
	if err != nil {
		p.d.Log.Warn("pool reset failed — dropping container", "container", old.name, "error", err)
		return
	}
	// The serve probe gates the entry into the pool.
	if resp.ServeURL != "" && !serveUsableAt(resp.ServeURL, resp.ServePassword) {
		p.d.Log.Warn("pool reset: serve not usable — dropping container", "container", name)
		_ = p.d.removeContainer(name)
		return
	}
	ent := &poolEntry{
		name:          name,
		envKey:        envKey,
		image:         old.image,
		mounts:        old.mounts,
		serveCfg:      old.serveCfg,
		projectDir:    old.projectDir,
		adapterKinds:  old.adapterKinds,
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
		// engine call under the pool lock).
		p.d.Log.Info("pool reset: at per-env cap, dropping container", "env", ent.envKey, "container", name)
		_ = p.d.removeContainer(name)
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

// idleReap removes clean containers that have been unused past the idle window, keeping the pool
// bounded across many projects/images.
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
		_ = p.d.removeContainer(ent.name)
		p.mu.Lock()
		delete(p.entries, ent.name)
		p.mu.Unlock()
	}
	// Second pass on the same tick: stale leases (dead run, lost terminal release) are reaped here so a
	// leak can never outlive the staleness window — every live run renews within 30s.
	p.reapStaleLeases()
	// Third pass: containers the pool does not track at all.
	p.reconcileOrphans()
}

// resetPool clears the in-memory pool at daemon start.
func (p *daemonPool) resetPool() {
	p.mu.Lock()
	p.entries = make(map[string]*poolEntry)
	p.clean = make(map[string][]string)
	p.leased = make(map[string]string)
	p.swept = make(map[string]bool)
	p.mu.Unlock()
}

// sweepPreRestartOrphans removes the containers `instance` owned BEFORE this daemon started — the
// ones whose leases died with the previous process.
func (p *daemonPool) sweepPreRestartOrphans(instance string) {
	if instance == "" {
		return
	}
	p.mu.Lock()
	if p.swept[instance] {
		p.mu.Unlock()
		return
	}
	p.swept[instance] = true
	p.mu.Unlock()

	names, err := p.d.listRuntimes(instance)
	if err != nil {
		// Do not spend the one claim on a failed listing.
		p.mu.Lock()
		delete(p.swept, instance)
		p.mu.Unlock()
		p.d.Log.Warn("pre-restart sweep: list runtimes", "instance", instance, "error", err)
		return
	}
	for _, n := range names {
		// Re-check under the lock immediately before removing: a container the pool has since tracked
		// belongs to a live lease and must survive.
		p.mu.Lock()
		_, tracked := p.entries[n]
		p.mu.Unlock()
		if tracked {
			continue
		}
		p.d.Log.Info("pre-restart sweep: removing untracked container", "instance", instance, "container", n)
		_ = p.d.removeContainer(n)
	}
}

// reconcileOrphans is the LIVE half of container reconciliation: it removes containers belonging to
// an instance this daemon serves that the pool does not track.
func (p *daemonPool) reconcileOrphans() {
	p.mu.Lock()
	instances := make([]string, 0, len(p.served))
	for inst := range p.served {
		instances = append(instances, inst)
	}
	p.mu.Unlock()
	if len(instances) == 0 {
		return
	}
	grace := p.orphanGrace()
	now := time.Now()
	for _, instance := range instances {
		names, err := p.d.listRuntimes(instance)
		if err != nil {
			// A failed listing is NOT evidence that containers are gone, so nothing is forgotten here —
			// the next pass simply tries again.
			p.d.Log.Warn("live sweep: list runtimes", "instance", instance, "error", err)
			continue
		}
		present := make(map[string]bool, len(names))
		var victims []string
		p.mu.Lock()
		for _, n := range names {
			present[n] = true
			if _, tracked := p.entries[n]; tracked {
				// It became OURS between passes — forget the candidate.
				delete(p.untracked, n)
				continue
			}
			cand, seen := p.untracked[n]
			if !seen {
				p.untracked[n] = orphanCandidate{instance: instance, since: now}
				continue
			}
			if now.Sub(cand.since) >= grace {
				victims = append(victims, n)
			}
		}
		// Forget candidates for this instance that are no longer present at all (removed by hand, or by
		// another actor), so the map cannot grow without bound.
		for n, cand := range p.untracked {
			if cand.instance == instance && !present[n] {
				delete(p.untracked, n)
			}
		}
		p.mu.Unlock()

		for _, n := range victims {
			// Re-check immediately before the destructive call: a checkout in the meantime must win.
			p.mu.Lock()
			_, tracked := p.entries[n]
			p.mu.Unlock()
			if tracked {
				continue
			}
			p.d.Log.Warn("live sweep: removing untracked container",
				"instance", instance, "container", n, "grace", grace,
				"hint", "present in the engine but not tracked by this daemon")
			_ = p.d.removeContainer(n)
			p.mu.Lock()
			delete(p.untracked, n)
			p.mu.Unlock()
		}
	}
}

// orphanGrace resolves ORCHICON_RUNTIME_ORPHAN_GRACE (default 2m): how long a container must be
// present but untracked before the live sweep reclaims it.
func (p *daemonPool) orphanGrace() time.Duration {
	if v := p.d.envDuration("ORCHICON_RUNTIME_ORPHAN_GRACE"); v > 0 {
		return v
	}
	return 2 * time.Minute
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

// reapStaleLeases removes LEASED containers whose lease has not been renewed past the staleness
// window — the immortality fix for the aborted-run leak class.
func (p *daemonPool) reapStaleLeases() {
	window := p.leaseMaxAge()
	if window <= 0 {
		return
	}
	// Snapshot victims under the lock; docker calls happen after unlock.
	type victim struct {
		runID, name string
	}
	var victims []victim
	p.mu.Lock()
	cutoff := time.Now().Add(-window)
	for runID, name := range p.leased {
		ent := p.entries[name]
		switch {
		case ent == nil:
			// Dangling lease to a dropped entry — reap the mapping itself.
			victims = append(victims, victim{runID: runID, name: name})
		case ent.lastUsed.Before(cutoff):
			victims = append(victims, victim{runID: runID, name: ent.name})
		}
	}
	p.mu.Unlock()
	for _, v := range victims {
		p.d.Log.Warn("pool stale-lease reap: removing abandoned run container",
			"run", v.runID, "container", v.name,
			"hint", "run died without releasing (lost terminal reap)")
		_ = p.d.removeContainer(v.name)
		p.mu.Lock()
		// Delete only if the mapping still points at the same container — a
		// concurrent checkout could have re-leased the run meanwhile.
		if cur, ok := p.leased[v.runID]; ok && cur == v.name {
			delete(p.leased, v.runID)
		}
		if ent, ok := p.entries[v.name]; ok && ent.leasedBy == v.runID {
			delete(p.entries, v.name)
		}
		p.mu.Unlock()
	}
}

// leaseMaxAge resolves the leased-container staleness window (ORCHICON_RUNTIME_LEASE_MAX).
func (p *daemonPool) leaseMaxAge() time.Duration {
	if v := p.d.envDuration("ORCHICON_RUNTIME_LEASE_MAX"); v > 0 {
		return v
	}
	return 30 * time.Minute
}
