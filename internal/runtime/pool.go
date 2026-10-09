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
// adopt pass re-lease for active runs. Leases are SELF-HEALING: every
// idempotent checkout of an active run renews the lease, so a lease that
// sits idle past the staleness window belongs to a run that died without
// releasing (the aborted-run leak class) and is reaped with its container
// instead of surviving until the next daemon restart.
type daemonPool struct {
	d  *Daemon
	mu sync.Mutex
	// entries is the full inventory by container name.
	entries map[string]*poolEntry
	// clean lists available (reset, warm) container names per environment.
	clean map[string][]string
	// leased maps an active run id to its container name.
	leased map[string]string
	// swept records the instances whose pre-restart containers have already been
	// reconciled since this daemon started, so that costs one listing per
	// instance per daemon lifetime rather than one per lease. See
	// sweepPreRestartOrphans.
	swept map[string]bool
}

// poolEntry is the pool's bookkeeping for one container.
type poolEntry struct {
	name       string
	envKey     string
	image      string
	mounts     []MountSpec
	serveCfg   string
	projectDir string
	// adapterKinds is the run's boot profile (CreateRequest.AdapterKinds),
	// carried so a reset container keeps the same profile — and therefore
	// re-keys under the same environment instead of drifting (AC 4).
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
		leased:  make(map[string]string),
		swept:   make(map[string]bool),
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
	// The git strategy is a credential-relevant host input: a "none"
	// (ephemeral) container is created WITHOUT a push-capable GH_TOKEN and
	// WITHOUT the git credential mounts, so it must never pool with a
	// local/pr container that carries them. Fold it into the key so the two
	// environments stay separate — under the same key a "none" run could
	// reuse a warm token-bearing container (the pre-fix leak class).
	if req.GitStrategy != "" {
		_, _ = io.WriteString(h, "gs="+req.GitStrategy+"\n")
	}
	// The BOOT PROFILE (the adapter kinds the run's step workers need) is a
	// credential-relevant host input: it decides which adapter install
	// mounts (the host's model config/auth) the container is created with
	// and which in-container serve it warms. A container warmed for an
	// opencode run must never be reused for a native-only run and vice
	// versa — fold the sorted profile into the key so a profile change
	// invalidates the pooled container (AC 4). A NIL profile folds away: it
	// denotes the legacy "unspecified" case, which resolves to the default
	// (opencode) profile anyway. An explicitly EMPTY (non-nil) profile keys
	// distinct from both — it demands no adapter kind.
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
		if ent != nil {
			// Lease renewal: idempotent re-checkouts (the run-start gate, the
			// adapter's dispatch self-heal, and the plane's 30s adopt sweep)
			// keep an ACTIVE run's lease young. The stale-lease reap keys off
			// this: a lease idle past the staleness window was abandoned by a
			// run that can no longer renew it — a lost release, never a live
			// run.
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

// containerForRun returns the leased container name for a run ("" when
// the run holds no lease). Read-only: exec routing must never create or
// mutate a lease.
func (p *daemonPool) containerForRun(runID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leased[runID]
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
		// The boot profile MUST survive the reset: dropping it would (a)
		// re-create a native-only container with opencode mounts and (b)
		// re-key the container under the wrong environment (AC 4).
		AdapterKinds: old.adapterKinds,
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
// containers are exempt unless their lease has gone STALE (see
// reapStaleLeases) — a live run renews every ≤30s.
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
	// Second pass on the same tick: stale leases (dead run, lost terminal
	// release) are reaped here so a leak can never outlive the staleness
	// window — every live run renews within 30s.
	p.reapStaleLeases()
}

// resetPool clears the in-memory pool at daemon start.
//
// IT DELIBERATELY TOUCHES NO CONTAINER, and that is the fix for a real
// cross-instance hazard. It used to call listRuntimes("") and `rm -f` every
// orchicon.workflow container on the host — so starting the daemon for ONE
// instance destroyed EVERY instance's containers, including a sibling's live
// one mid-run.
//
// The launcher is careful to be instance-scoped for exactly this reason
// (scripts/container.sh stop_runtime_daemon: "a dev rebuild must never
// hard-kill a live prod fire's container mid-run") — and the wholesale
// start-time sweep defeated that protection, because up_instance starts the
// daemon right after the instance-scoped reap.
//
// Leases live in this process, so a restart loses them and every container from
// before is untracked. But untracked is NOT dead: a container can be running a
// session this daemon can no longer route to. Removal is therefore deferred to
// sweepPreRestartOrphans, which is scoped to one instance and only runs once
// that instance's own plane proves it is alive. Without a scoped trigger the
// only options are a blind sweep (the hazard) or no sweep at all (a leak).
func (p *daemonPool) resetPool() {
	p.mu.Lock()
	p.entries = make(map[string]*poolEntry)
	p.clean = make(map[string][]string)
	p.leased = make(map[string]string)
	p.swept = make(map[string]bool)
	p.mu.Unlock()
}

// sweepPreRestartOrphans removes the containers `instance` owned BEFORE this
// daemon started — the ones whose leases died with the previous process.
//
// INSTANCE-SCOPED, and that is the whole point: a daemon restart is a host-wide
// event, but the containers it leaves behind belong to individual instances, and
// another instance's plane may be mid-run. Sweeping only the instance that is
// asking cannot reach across that boundary — the label the sweep filters on is
// the same one the create path wrote (daemon.go's instanceID), so "whose
// containers are these?" is answered by the label rather than guessed.
//
// THE TRIGGER IS WHAT MAKES THE REMOVAL SAFE rather than merely cautious. It
// runs on this instance's own first lease request, which proves that plane is
// alive; and because the daemon lost the lease map, any container of its own the
// pool does not track can no longer be serving a run this daemon can route to —
// the run re-leases onto a fresh container. So the untracked ones are genuinely
// this instance's orphans, and they are removed instead of leaking until the
// next restart.
//
// ONCE PER INSTANCE per daemon lifetime: the claim is taken before the docker
// calls so two concurrent first requests cannot both sweep, and released again
// if the listing fails so a transient docker error defers cleanup to the next
// lease rather than losing it for the daemon's lifetime.
//
// An empty instance is REFUSED rather than read as "all": listRuntimes("") means
// every instance on the host, which is precisely the behaviour being removed.
// The create path normalises through instanceID before calling, so an empty
// value cannot legitimately arrive here.
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
		// Re-check under the lock immediately before removing: a container the
		// pool has since tracked belongs to a live lease and must survive.
		//
		// A NARROW WINDOW REMAINS between this check and the `rm` below, and it is
		// stated rather than glossed: a container created AND tracked inside those
		// microseconds by a concurrent first request for the same instance would be
		// removed despite being tracked. Closing it fully would mean either a docker
		// call under the pool lock (which this file forbids) or comparing each
		// candidate's creation time against the daemon's start. The window costs at
		// most one run's container on an instance that is restarting its runtime
		// anyway — recoverable, unlike the host-wide sweep this replaced.
		p.mu.Lock()
		_, tracked := p.entries[n]
		p.mu.Unlock()
		if tracked {
			continue
		}
		p.d.Log.Info("pre-restart sweep: removing untracked container", "instance", instance, "container", n)
		_, _ = p.d.docker("rm", "-f", n)
	}
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

// reapStaleLeases removes LEASED containers whose lease has not been
// renewed past the staleness window — the immortality fix for the
// aborted-run leak class. A lease normally renews on every checkout: the
// run-start gate, the adapter's dispatch self-heal, and the plane's 30s
// adopt sweep all re-checkout active runs idempotently (checkout's
// existing-lease fast path now bumps lastUsed). So a lease idle past the
// window belongs to a run that terminalized while its release was lost or
// dropped (ReapForRun is warn-and-drop; a terminal run is never retried) —
// its container would otherwise survive until the next daemon reset while
// never being reaped by the idle pass (leased entries were exempt).
// Defaults to a generous multiple of the 30s renewal cadence; overridable
// via ORCHICON_RUNTIME_LEASE_MAX. The window must only exceed the renewal
// cadence — set it generously (hours, not minutes) so a wedged host (GC
// pause, suspended VM) can never lose a legitimately live run's container.
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
		_, _ = p.d.docker("rm", "-f", v.name)
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

// leaseMaxAge resolves the leased-container staleness window
// (ORCHICON_RUNTIME_LEASE_MAX). Default 30m — every live path renews a
// lease within 30s (the adopt sweep), so 30m gives an enormous margin over
// the renewal cadence while bounding any leak to half an hour.
func (p *daemonPool) leaseMaxAge() time.Duration {
	if v := p.d.envDuration("ORCHICON_RUNTIME_LEASE_MAX"); v > 0 {
		return v
	}
	return 30 * time.Minute
}
