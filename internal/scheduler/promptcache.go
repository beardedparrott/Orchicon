package scheduler

import (
	"container/list"
	"strconv"
	"sync"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
)

// Context-section prefix cache (ADR-0009 D5): the rendered context-file sections are the bulk of
// the static prompt prefix, and re-rendering them from disk per execution both costs I/O.
type promptCacheEntry struct {
	key    string // tenant\x00project\x00fingerprint
	bytes  string // the rendered section, verbatim-reusable
	stamps []contextfiles.FileStamp
}

type promptSectionCache struct {
	mu    sync.Mutex
	max   int
	items map[string]*list.Element
	order *list.List
	// lastStamps tracks the per-path stamps of the most recent render for
	// a (tenant, project) so a miss log can name the changed files.
	lastStamps map[string][]contextfiles.FileStamp
}

var globalPromptCache = newPromptSectionCache(64)

func newPromptSectionCache(max int) *promptSectionCache {
	if max <= 0 {
		max = 64
	}
	return &promptSectionCache{
		max:        max,
		items:      map[string]*list.Element{},
		order:      list.New(),
		lastStamps: map[string][]contextfiles.FileStamp{},
	}
}

// get returns the cached section for a key (hit → verbatim reuse).
func (c *promptSectionCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*promptCacheEntry).bytes, true
	}
	return "", false
}

// put stores a freshly-rendered section, evicting the oldest entry past
// the bound.
func (c *promptSectionCache) put(e *promptCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[e.key]; ok {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.items[e.key] = c.order.PushFront(e)
	for c.order.Len() > c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		ev := c.order.Remove(back).(*promptCacheEntry)
		delete(c.items, ev.key)
	}
}

// rememberStamps replaces the per-path stamps recorded for a scope key
// (tenant\x00project) for the next miss diff, returning the previous set.
func (c *promptSectionCache) rememberStamps(scope string, stamps []contextfiles.FileStamp) []contextfiles.FileStamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.lastStamps[scope]
	c.lastStamps[scope] = stamps
	return prev
}

// renderContextSectionCached is the fingerprint-aware wrapper around contextfiles.RenderManifest
// used by BOTH composite builders.
func (r *WorkflowReconciler) renderContextSectionCached(tenantID, projectID, rootNote string, paths []string, projectDir string) (string, string) {
	return r.renderSectionCachedWithPolicy(tenantID, projectID, rootNote, paths, projectDir, contextfiles.ManifestPolicyContext)
}

// renderSectionCachedWithPolicy is the policy-aware form.
func (r *WorkflowReconciler) renderSectionCachedWithPolicy(tenantID, projectID, rootNote string, paths []string, projectDir string, policy contextfiles.ManifestPolicy) (string, string) {
	// Normalize a typed-nil logger (*slog.Logger(nil) boxed in the Info-capable interface is != nil) —
	// struct-literal reconcilers (tests) carry a nil log, and calling Info on it would panic.
	var lg interface{ Info(string, ...any) }
	if r.log != nil {
		lg = r.log
	}
	return renderSectionCachedWithPolicy(globalPromptCache, lg, tenantID, projectID, rootNote, paths, projectDir, policy)
}

// renderContextSectionCached is the CONTEXT-policy entry point, kept as its own name so existing callers and
// tests read the same and cannot accidentally opt into the skills policy.
func renderContextSectionCached(cache *promptSectionCache, log interface{ Info(string, ...any) }, tenantID, projectID, rootNote string, paths []string, projectDir string) (string, string) {
	return renderSectionCachedWithPolicy(cache, log, tenantID, projectID, rootNote, paths, projectDir, contextfiles.ManifestPolicyContext)
}

func renderSectionCachedWithPolicy(cache *promptSectionCache, log interface{ Info(string, ...any) }, tenantID, projectID, rootNote string, paths []string, projectDir string, policy contextfiles.ManifestPolicy) (string, string) {
	fp, stamps, err := contextfiles.Fingerprint(paths, projectDir)
	if err != nil || cache == nil {
		// Fingerprinting must never fail the prompt build — degrade to a
		// plain render (uncached).
		return contextfiles.RenderManifestWithPolicy(rootNote, paths, projectDir, policy), fp
	}
	// rootNote IS PART OF THE KEY.
	key := tenantID + "\x00" + projectID + "\x00" + rootNote + "\x00" + fp + "\x00" + strconv.Itoa(int(policy))
	if cached, ok := cache.get(key); ok {
		return cached, fp
	}
	section := contextfiles.RenderManifestWithPolicy(rootNote, paths, projectDir, policy)
	scope := tenantID + "\x00" + projectID + "\x00" + rootNote
	prev := cache.rememberStamps(scope, stamps)
	if log != nil {
		if diff := contextfiles.DiffStamps(prev, stamps); len(diff) > 0 {
			log.Info("prompt context prefix changed (cache miss)",
				"project", projectID, "fingerprint", fp, "changes", diff)
		} else {
			log.Info("prompt context re-rendered (cache miss)",
				"project", projectID, "fingerprint", fp)
		}
	}
	cache.put(&promptCacheEntry{key: key, bytes: section, stamps: stamps})
	return section, fp
}
