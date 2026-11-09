package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// HostServePool hands out a HostServe per RESOLVED MCP set for the in-process (local) execution
// population.
type HostServePool struct {
	log         *slog.Logger
	baseDataDir string
	home        string
	profile     PermissionProfile

	mu      sync.Mutex
	entries map[string]*HostServe
	// defaultServe is the EMPTY-set serve: today's single host serve, on the base data dir.
	defaultServe *HostServe
}

// NewHostServePool constructs the pool.
func NewHostServePool(log *slog.Logger, baseDataDir, home string, profile PermissionProfile) *HostServePool {
	return &HostServePool{
		log:         log,
		baseDataDir: baseDataDir,
		home:        home,
		profile:     profile,
		entries:     map[string]*HostServe{},
	}
}

// SetDefaultServe adopts an ALREADY-CONSTRUCTED serve as the pool's empty-set default, so the plane
// keeps exactly ONE default host serve (the object the server already holds and supervises).
func (p *HostServePool) SetDefaultServe(hs *HostServe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultServe = hs
}

// DefaultServe returns the empty-set serve (the pre-pool single host serve).
func (p *HostServePool) DefaultServe() *HostServe {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultServe == nil {
		p.defaultServe = p.newServe(p.baseDataDir, mcpclient.Resolution{})
	}
	return p.defaultServe
}

// ServeFor returns the serve whose baked config matches set, starting nothing (the caller still
// calls EnsureStarted).
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

// hostServeSetKey is the stable identity of a resolved set: a sha256 over the permission profile,
// every server's id + transport-relevant fingerprint (command/url/env/headers.
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

// It is NEVER logged — it carries resolved env/headers values; only the server IDS (describeSet /
// ProvenanceString) are ever logged.
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

// describeSet renders a resolved set's server IDS for a LOG or an error message.
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
