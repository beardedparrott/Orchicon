package scheduler

import (
	"container/list"
	"log/slog"
	"sync"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
)

// standalonePromptLog is the logger the standalone composite path uses
// for prefix-cache hit/miss lines. buildStandaloneComposite is a package
// function (no reconciler receiver), so it logs through the default
// logger — same destination the daemon's structured log goes to.
func standalonePromptLog() *slog.Logger { return slog.Default() }

// Context-section prefix cache (ADR-0009 D5): the rendered context-file
// sections are the bulk of the static prompt prefix, and re-rendering
// them from disk per execution both costs I/O and — when a file changed
// mid-chain — silently shifts the prefix bytes (a cache miss).
//
// renderContextSectionCached computes the context-file fingerprint and,
// when the fingerprint is unchanged from a recent render, reuses the
// previous section VERBATIM. A changed fingerprint logs a per-path diff
// (which file moved) so an operator can attribute a prefix-cache miss
// to a concrete edit. The cache is bounded (LRU) and process-local —
// it is an optimization, never a source of truth: a restart simply
// re-renders.
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

// renderContextSectionCached is the fingerprint-aware wrapper around
// contextfiles.RenderManifest used by BOTH composite builders. On a
// fingerprint hit the previously rendered bytes are reused verbatim
// (byte-stability across executions — the cost lever); on a miss the
// section is rendered from disk, cached, and (when prior stamps exist)
// the per-path diff is logged so the miss is attributable. Returns the
// rendered section and the fingerprint.
func (r *WorkflowReconciler) renderContextSectionCached(tenantID, projectID, rootNote string, paths []string, projectDir string) (string, string) {
	return renderContextSectionCached(globalPromptCache, r.log, tenantID, projectID, rootNote, paths, projectDir)
}

func renderContextSectionCached(cache *promptSectionCache, log interface{ Info(string, ...any) }, tenantID, projectID, rootNote string, paths []string, projectDir string) (string, string) {
	fp, stamps, err := contextfiles.Fingerprint(paths, projectDir)
	if err != nil || cache == nil {
		// Fingerprinting must never fail the prompt build — degrade to a
		// plain render (uncached).
		return contextfiles.RenderManifest(rootNote, paths, projectDir), fp
	}
	key := tenantID + "\x00" + projectID + "\x00" + fp
	if cached, ok := cache.get(key); ok {
		return cached, fp
	}
	section := contextfiles.RenderManifest(rootNote, paths, projectDir)
	scope := tenantID + "\x00" + projectID
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
