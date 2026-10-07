package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

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
		p.defaultServe = p.newServe(p.baseDataDir, mcpclient.Resolution{})
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
func (p *HostServePool) ServeFor(ctx context.Context, set mcpclient.Resolution) (*HostServe, error) {
	if len(set.Servers) == 0 && len(set.Skills) == 0 {
		return p.DefaultServe(), nil
	}
	key := hostServeSetKey(p.profile, set)

	p.mu.Lock()
	defer p.mu.Unlock()
	if hs, ok := p.entries[key]; ok {
		return hs, nil
	}
	if len(p.entries) >= p.cap {
		return nil, fmt.Errorf(
			"host serve pool is at capacity (%d): no serve can be started for MCP set [%s] — raise ORCHICON_HOST_SERVE_POOL_CAP or reduce the number of distinct project MCP sets",
			p.cap, describeSet(set))
	}
	hs := p.newServe(filepath.Join(p.baseDataDir, "serve-"+key[:16]), set)
	p.entries[key] = hs
	p.log.Info("host serve pool: added serve for MCP set",
		"key", key[:16], "data_dir", hs.dataDir, "servers", describeSet(set))
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
	p.defaultServe = nil
	p.mu.Unlock()
	for _, hs := range serves {
		hs.Stop()
	}
}

// newServe builds one pooled serve with its OWN data dir. dataDir is created
// by HostServe.Start/seedAuth; the pool only names it.
func (p *HostServePool) newServe(dataDir string, set mcpclient.Resolution) *HostServe {
	var hs *HostServe
	if p.profile == ProfileInteractive {
		hs = NewAskHostServe(p.log, dataDir, p.home)
	} else {
		hs = NewHostServe(p.log, dataDir, p.home)
	}
	hs.SetMCPSet(set)
	return hs
}

// hostServeSetKey is the stable identity of a resolved set: a sha256 over the
// permission profile, every server's id + transport-relevant fingerprint
// (command/url/env/headers — already secret-resolved by the caller, so two
// projects with the same ids but different credentials get different keys),
// and every skill's path + content hash. Server and skill lines are sorted so
// two resolutions that differ only in ORDER share one serve.
func hostServeSetKey(profile PermissionProfile, set mcpclient.Resolution) string {
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

	h := sha256.New()
	_, _ = io.WriteString(h, "profile="+string(profile)+"\n")
	for _, l := range srv {
		_, _ = io.WriteString(h, l+"\n")
	}
	for _, l := range skill {
		_, _ = io.WriteString(h, "skill="+l+"\n")
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
