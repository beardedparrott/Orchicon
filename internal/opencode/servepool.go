package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// HostServePool hands out a HostServe per RESOLVED MCP set for the in-process
// (local) execution population.
//
// WHY KEYED BY SET — and why a cross-project union is FORBIDDEN. A host serve
// builds its OPENCODE_CONFIG_CONTENT ONCE and then lives for the plane,
// serving heterogeneous work over time (standalone dispatches, local-mode
// runs, follow-ups, Ask). Unioning several projects' MCP sets onto that one
// serve would make one project's servers visible in ANOTHER project's session
// — a leak with no consent boundary. So the serve a session lands on must be
// one whose baked config is exactly the set that session is entitled to, and
// nothing else. Keying each serve by its exact resolved set is what makes
// "cross-project union is forbidden" safe to state absolutely.
//
// THE SPIKE DECIDED THIS SHAPE (child 4, AC 7). opencode's serve API exposes
// runtime ADD (`POST /mcp`) and connect/disconnect, but NO `DELETE /mcp/{name}`
// — a configured server cannot be removed for the life of the process. So the
// "runtime add/remove per session" option 1 was NOT available, and the host
// serve could not be one process reconfigured per session. Option 2 — a POOL
// of serves, one per resolved set, each with its own data dir — is the landing.
// (What IS available: `PATCH /config` can DISABLE a server and per-message
// `tools` maps can gate MCP tools per turn, but a serve still eagerly connects
// every configured server at startup — the reason SkipUserMCP exists — so
// neither substitutes for a serve whose config is exactly the session's set.)
//
// LAZINESS mirrors adapter.TenantDemandSet: a serve is created only when a
// session's resolved set requires it, and the EMPTY set reuses the single
// default serve — so a plane that configures no MCP keeps today's
// one-process topology, unchanged.
//
// EACH ENTRY OWNS ITS OWN DATA DIR. Two opencode serve processes must not
// share one sqlite data dir (see NewAskHostServe's warning); the pool derives
// a per-key dir so they never do.
type HostServePool struct {
	log         *slog.Logger
	baseDataDir string
	home        string
	profile     PermissionProfile
	cap         int

	mu      sync.Mutex
	entries map[string]*HostServe
	// setIndex maps the profile+MCP-set identity (WORKERS EXCLUDED) to the first
	// serve created for that set, whatever workers it carries. It exists ONLY for
	// the degradation path in ServeFor: when a worker-carrying key cannot fit, the
	// session must land on A serve for its OWN MCP set — any one — because the set is
	// what must not leak, and a serve carrying other workers' agents is inert for a
	// session whose own agent is absent (selection fails, so the trim is skipped and
	// the full prompt rides the message).
	//
	// It is deliberately a SEPARATE index rather than a second key in entries: exact
	// reuse must still be exact, and only the fallback is allowed to be approximate.
	setIndex map[string]*HostServe
	// defaultServe is the EMPTY-set serve: today's single host serve, on the
	// base data dir. It is what an unconfigured plane (and every session with
	// no project) gets, so nothing changes for those deployments.
	defaultServe *HostServe
}

// DefaultHostServePoolCap bounds how many pooled serve PROCESSES (each with
// its own sqlite data dir) a plane may hold. An unbounded pool multiplies
// processes and data dirs, so a NEW key beyond the cap is REFUSED LOUDLY
// (naming the set) rather than evicting a live serve under a running session.
const DefaultHostServePoolCap = 4

// NewHostServePool constructs the pool. baseDataDir is the same dedicated
// opencode data dir the single host serve uses (the empty-set default serve
// lives there, exactly as before); home overrides the operator home used to
// seed model auth. profile selects the permission profile each pooled serve
// is built with (the worker serve vs the Ask serve).
func NewHostServePool(log *slog.Logger, baseDataDir, home string, profile PermissionProfile) *HostServePool {
	limit := DefaultHostServePoolCap
	if v := os.Getenv("ORCHICON_HOST_SERVE_POOL_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	return &HostServePool{
		log:         log,
		baseDataDir: baseDataDir,
		home:        home,
		profile:     profile,
		cap:         limit,
		entries:     map[string]*HostServe{},
		setIndex:    map[string]*HostServe{},
	}
}

// SetDefaultServe adopts an ALREADY-CONSTRUCTED serve as the pool's
// empty-set default, so the plane keeps exactly ONE default host serve (the
// object the server already holds and supervises) rather than the pool
// building a second manager over the same data dir — two managers over one
// sqlite dir would spawn two serve processes that share it, which the serve
// must never do.
func (p *HostServePool) SetDefaultServe(hs *HostServe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultServe = hs
}

// DefaultServe returns the empty-set serve (the pre-pool single host serve).
// It is lazily constructed on first use so an opencode-free plane still
// spawns nothing.
func (p *HostServePool) DefaultServe() *HostServe {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultServe == nil {
		p.defaultServe = p.newServe(p.baseDataDir, mcpclient.Resolution{}, nil)
	}
	return p.defaultServe
}

// ServeFor returns the serve whose baked config matches set, starting nothing
// (the caller still calls EnsureStarted). The EMPTY set maps to the default
// serve; any non-empty set is keyed by its exact contents, so two disjoint
// sets never share a serve and two sets differing only in credentials land on
// different serves (a token is never shared across projects).
//
// At the cap, a NEW key is refused with a LOUD error NAMING the set — never a
// silent fallback to a serve carrying the wrong config, and never an eviction
// of a live serve out from under a running session.
//
// WORKERS ARE PART OF THE KEY TOO, and for a correctness reason rather than a
// performance one. Since a serve registers one agent per step worker (the only
// channel that delivers a per-execution prompt on 2.x — see adapter.WorkerAgent), a
// serve built WITHOUT a given worker's agent cannot carry that worker's prompt. So
// the pair (resolved MCP set, worker set) is the serve's identity.
//
// WORKER EXHAUSTION DEGRADES; MCP-SET EXHAUSTION STILL REFUSES. The two dimensions
// are NOT equally severe, so they do not get the same answer:
//
//   - A serve with the WRONG MCP set leaks one project's servers into another
//     session. That is a correctness violation and must refuse.
//   - A serve missing a worker's AGENT is a DEGRADATION: agent selection fails, so
//     the adapter skips the message trim and the worker gets its full prompt on the
//     message instead — correct, merely untrimmed. Failing the run here would turn a
//     working execution into a broken one, which is exactly what an in-process run on
//     a plane with no runtime daemon (or a local-mode project) would have hit.
//
// So a capacity refusal driven by WORKERS retries the SAME MCP set without them, and
// only a refusal the MCP set itself causes is fatal.
func (p *HostServePool) ServeFor(ctx context.Context, set mcpclient.Resolution, workers []adapter.WorkerAgent) (*HostServe, error) {
	hs, err := p.serveForSet(set, workers)
	if err == nil {
		return hs, nil
	}
	if len(workers) > 0 && errors.Is(err, errPoolAtCapacity) {
		// Land on ANY serve for this MCP set — not specifically a worker-free one.
		// The SET is what must not leak; a serve already serving this set carries
		// the right servers, and the absence of THIS session's agent is a bounded
		// degradation (agent selection fails, so the adapter sends the full prompt
		// on the message). Requiring a worker-FREE serve instead was a HOLE: a pool
		// filled up with (set, otherWorker) entries refused a new worker on the SAME
		// set, even though reusing one of them leaks nothing.
		if hs := p.serveForSetIndexed(set); hs != nil {
			p.log.Warn("host serve pool at capacity for this worker set — reusing the serve already holding this MCP set; "+
				"the session keeps its FULL prompt on the message instead of having the run-level prefix carried by an agent",
				"workers", len(workers), "servers", describeSet(set))
			return hs, nil
		}
		// Nothing is pooled for this set at all, so this is a genuine SET addition
		// that the cap refuses — the case that must stay fatal.
	}
	return nil, err
}

// errPoolAtCapacity marks the LOUD refusal, so ServeFor can tell a capacity refusal
// apart from a start failure and choose whether a degradation is safe.
var errPoolAtCapacity = errors.New("host serve pool is at capacity")

// serveForSetIndexed returns an ALREADY-POOLED serve for this MCP set, ignoring which
// workers it carries, or nil when the set has no serve yet. It never creates anything
// and never consults the cap, so it cannot fail.
//
// The empty set maps to the default serve: an unconfigured plane already serves it,
// and there are no servers to leak.
func (p *HostServePool) serveForSetIndexed(set mcpclient.Resolution) *HostServe {
	if len(set.Servers) == 0 && len(set.Skills) == 0 {
		return p.DefaultServe()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setIndex[hostServeSetKey(p.profile, set, nil)]
}

func (p *HostServePool) serveForSet(set mcpclient.Resolution, workers []adapter.WorkerAgent) (*HostServe, error) {
	if len(set.Servers) == 0 && len(set.Skills) == 0 && len(workers) == 0 {
		return p.DefaultServe(), nil
	}
	key := hostServeSetKey(p.profile, set, workers)

	p.mu.Lock()
	defer p.mu.Unlock()
	if hs, ok := p.entries[key]; ok {
		return hs, nil
	}
	if len(p.entries) >= p.cap {
		return nil, fmt.Errorf(
			"%w (%d): no serve can be started for MCP set [%s] with %d worker agent(s) — raise ORCHICON_HOST_SERVE_POOL_CAP or reduce the number of distinct (project MCP set, worker) pairs",
			errPoolAtCapacity, p.cap, describeSet(set), len(workers))
	}
	hs := p.newServe(filepath.Join(p.baseDataDir, "serve-"+key[:16]), set, workers)
	p.entries[key] = hs
	// Index by the set ALONE (workers excluded) if this is the set's first serve:
	// it is the fallback target for a later worker that does not fit.
	if sk := hostServeSetKey(p.profile, set, nil); p.setIndex[sk] == nil {
		p.setIndex[sk] = hs
	}
	p.log.Info("host serve pool: added serve",
		"key", key[:16], "data_dir", hs.dataDir, "servers", describeSet(set), "worker_agents", len(workers))
	return hs, nil
}

// Size reports how many NON-default pooled serves exist (the default serve is
// not counted — it is today's serve, not an addition).
func (p *HostServePool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Stop kills every pooled serve and the default one. Called with the plane.
func (p *HostServePool) Stop() {
	p.mu.Lock()
	serves := make([]*HostServe, 0, len(p.entries)+1)
	for _, hs := range p.entries {
		serves = append(serves, hs)
	}
	if p.defaultServe != nil {
		serves = append(serves, p.defaultServe)
	}
	p.entries = map[string]*HostServe{}
	p.setIndex = map[string]*HostServe{}
	p.defaultServe = nil
	p.mu.Unlock()
	for _, hs := range serves {
		hs.Stop()
	}
}

// newServe builds one pooled serve with its OWN data dir. dataDir is created
// by HostServe.Start/seedAuth; the pool only names it.
func (p *HostServePool) newServe(dataDir string, set mcpclient.Resolution, workers []adapter.WorkerAgent) *HostServe {
	var hs *HostServe
	if p.profile == ProfileInteractive {
		hs = NewAskHostServe(p.log, dataDir, p.home)
	} else {
		hs = NewHostServe(p.log, dataDir, p.home)
	}
	hs.SetMCPSet(set)
	// Set BEFORE the serve starts: the worker agents ride the serve's baked config.
	hs.SetWorkers(workers)
	return hs
}

// hostServeSetKey is the stable identity of a resolved set: a sha256 over the
// permission profile, every server's id + transport-relevant fingerprint
// (command/url/env/headers — already secret-resolved by the caller, so two
// projects with the same ids but different credentials get different keys),
// and every skill's path + content hash. Server and skill lines are sorted so
// two resolutions that differ only in ORDER share one serve.
func hostServeSetKey(profile PermissionProfile, set mcpclient.Resolution, workers []adapter.WorkerAgent) string {
	srv := make([]string, 0, len(set.Servers))
	for _, s := range set.Servers {
		srv = append(srv, serverFingerprint(s.Spec))
	}
	sort.Strings(srv)
	skill := make([]string, 0, len(set.Skills))
	for _, s := range set.Skills {
		skill = append(skill, s.Path+"\x00"+sha256Hex(s.Content))
	}
	sort.Strings(skill)
	// Workers are hashed by name + prompt CONTENT (not identity): two resolutions
	// whose agents are byte-identical share one serve, which is what keeps the
	// container path's per-run key stable across identical runs.
	wrk := make([]string, 0, len(workers))
	for _, w := range workers {
		wrk = append(wrk, w.Name+"\x00"+sha256Hex(w.Prompt)+"\x00"+w.Model)
	}
	sort.Strings(wrk)

	h := sha256.New()
	_, _ = io.WriteString(h, "profile="+string(profile)+"\n")
	for _, l := range srv {
		_, _ = io.WriteString(h, l+"\n")
	}
	for _, l := range skill {
		_, _ = io.WriteString(h, "skill="+l+"\n")
	}
	for _, l := range wrk {
		_, _ = io.WriteString(h, "worker="+l+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// serverFingerprint renders one server spec into a stable, credential-bearing
// line. It is NEVER logged — it carries resolved env/headers values; only the
// server IDS (describeSet / ProvenanceString) are ever logged.
func serverFingerprint(s mcpclient.ServerSpec) string {
	envKeys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	hdrKeys := make([]string, 0, len(s.Headers))
	for k := range s.Headers {
		hdrKeys = append(hdrKeys, k)
	}
	sort.Strings(hdrKeys)

	h := sha256.New()
	_, _ = io.WriteString(h, "id="+s.ID+"\ntype="+string(s.TransportType())+"\n")
	_, _ = io.WriteString(h, "url="+s.URL+"\n")
	for _, c := range s.Command {
		_, _ = io.WriteString(h, "cmd="+c+"\n")
	}
	for _, k := range envKeys {
		_, _ = io.WriteString(h, "env="+k+"="+s.Env[k]+"\n")
	}
	for _, k := range hdrKeys {
		_, _ = io.WriteString(h, "hdr="+k+"="+s.Headers[k]+"\n")
	}
	return s.ID + ":" + hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// describeSet renders a resolved set's server IDS for a LOG or an error
// message. It reads Spec.ID only — never env/headers (see ProvenanceString's
// rule: the specs carry resolved credentials).
func describeSet(set mcpclient.Resolution) string {
	ids := make([]string, 0, len(set.Servers))
	for _, s := range set.Servers {
		if s.Spec.ID != "" {
			ids = append(ids, s.Spec.ID)
		}
	}
	if len(ids) == 0 && len(set.Skills) == 0 {
		return ""
	}
	sort.Strings(ids)
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	if len(set.Skills) > 0 {
		out += fmt.Sprintf(" (+%d skills)", len(set.Skills))
	}
	return out
}
