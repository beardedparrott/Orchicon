package chat

// render_cache.go — ONE REMEMBERED RENDER PER ITEM, so a frame that changes one
// message pays for one message.
//
// The operator: "I had a conversation that had over 250 messages. Not only does it
// make the conversation incredibly slow, it makes the entire TUI unresponsive and I
// had to kill it multiple times. I am guessing that is because we always have the
// conversation technically up and live on every pane down near the composer box."
//
// Measured at that size before this existed (render_bench_test.go):
//
//	BenchmarkStripFrameIdle250       7.52 ms/frame   8.77 MB/frame   132,779 allocs
//	BenchmarkStripFrameStreaming250  7.45 ms/frame   8.79 MB/frame   132,981 allocs
//
// THE TWO NUMBERS BEING THE SAME IS THE BUG. An idle frame and a frame that changed
// one message cost the same, because the renderer had no idea what changed: the strip
// (chatpanel.go, called from baseView) repaints the whole conversation on EVERY frame
// of every tab while it is open, and RenderItems laid out every item from scratch —
// markdown, lipgloss and box drawing for all 250 — in order to draw the nine rows the
// strip can actually show. 132k allocations per frame is not a slow render; it is a
// collector being fed faster than it can collect, which is what "the entire TUI
// unresponsive" is.
//
// WHAT IS REMEMBERED, AND WHY IT IS SAFE. renderItem is a pure function of ONE item
// plus the width it is drawn at, the copy glyph, and whether its reasoning block is
// folded — so its output can be remembered against exactly those and cannot leak into
// a context it was not drawn for. The key is (scope, item key, width, glyph, folded)
// and the value carries a FINGERPRINT of the item; a hit requires the fingerprint to
// match as well, so an item that has CHANGED is re-rendered even though it keys the
// same. Correctness rests on the fingerprint — the rest of the key is what makes a
// hit likely, not what makes it right.
//
// ONE ENTRY PER ITEM PER SURFACE, rather than one per version of an item: an item
// growing by a delta per frame REPLACES its entry instead of accumulating one, so
// what a conversation costs here is bounded by how many items it has.

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
)

// renderCacheMaxEntries bounds the map. It is a MEMORY bound, not a correctness one:
// exceeding it drops every entry, which costs one full re-render and nothing else. A
// conversation with more items than this, or a terminal resized many times (the width
// is part of a key), is what it exists for; the alternative — an unbounded map of
// rendered segments — is how a cache becomes the leak it was meant to prevent.
const renderCacheMaxEntries = 4096

// RenderCache remembers rendered items, so a repaint only pays for what changed.
//
// It is safe for concurrent use. A NIL *RenderCache renders without remembering
// anything, so a caller that has none — a one-off render, a test — needs no branch of
// its own.
type RenderCache struct {
	mu      sync.Mutex
	entries map[renderCacheKey]renderCacheEntry

	// layouts counts the items this cache has actually laid out, i.e. its misses. It
	// exists so a test can assert that a frame which changed nothing re-rendered
	// NOTHING — the whole claim of this file, and one that is invisible from the
	// rendered output, since a cache that always missed would produce identical text.
	layouts int
}

// renderCacheKey identifies a remembered segment. Every part of it is something the
// render Varies with; nothing here is a fingerprint (see renderCacheEntry.sig).
type renderCacheKey struct {
	// scope is what the items belong to — the conversation — so two conversations'
	// items can never share an entry, even when a key repeats across them.
	scope string
	// key is the item's own identity within the conversation.
	key string
	// width is the pane width the segment was laid out at: wrapping and padding are
	// baked into a rendered segment, so a different width is a different render.
	width int
	// glyph is the copy affordance the item was drawn with. It is a parameter rather
	// than package state (see RenderItemsSpansWithCopy) and it changes the text.
	glyph string
	// folded is whether this item's reasoning body was drawn collapsed. The predicate
	// is a function of the item's key, so it resolves to one bool per item and can be
	// keyed.
	folded bool
}

type renderCacheEntry struct {
	// sig fingerprints the item the segment was rendered FROM. A hit requires it to
	// match, which is what keeps a changed item from being served its old form.
	sig uint64
	seg string
	// code and askOpts are the segment's line geometry. They are handed to the caller
	// ALIASED rather than copied — an allocation per item would be the cost this cache
	// exists to remove — and they are written by nobody: ItemSpan.Code and
	// ItemSpan.Options are read by the click paths and mutated by no one.
	code    []CodeSpan
	askOpts []AskOptionSpan
}

// NewRenderCache returns an empty cache.
func NewRenderCache() *RenderCache {
	return &RenderCache{entries: make(map[renderCacheKey]renderCacheEntry)}
}

// Render is RenderItemsSpansWithCopy through a cache.
//
// scope names what the items belong to (the conversation id): items are only ever
// rendered as part of a conversation, and two conversations can hold items with the
// same key. copyGlyph is "" for a surface where a click copies nothing.
func (c *RenderCache) Render(scope string, items []ChatItem, maxWidth int, copyGlyph string, fold func(key string) bool) (string, []ItemSpan) {
	if c == nil {
		return renderItems(items, maxWidth, fold, copyGlyph, nil, "")
	}
	return renderItems(items, maxWidth, fold, copyGlyph, c, scope)
}

// get returns the remembered segment for an item, when the entry is there AND was
// rendered from an item identical to this one.
func (c *RenderCache) get(scope, key string, width int, glyph string, folded bool, sig uint64) (string, []CodeSpan, []AskOptionSpan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[renderCacheKey{scope: scope, key: key, width: width, glyph: glyph, folded: folded}]
	if !ok || e.sig != sig {
		return "", nil, nil, false
	}
	return e.seg, e.code, e.askOpts, true
}

// put remembers a segment, replacing whatever this item had before.
func (c *RenderCache) put(scope, key string, width int, glyph string, folded bool, sig uint64, seg string, code []CodeSpan, askOpts []AskOptionSpan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= renderCacheMaxEntries {
		c.entries = make(map[renderCacheKey]renderCacheEntry)
	}
	c.entries[renderCacheKey{scope: scope, key: key, width: width, glyph: glyph, folded: folded}] = renderCacheEntry{
		sig: sig, seg: seg, code: code, askOpts: askOpts,
	}
	c.layouts++
}

// layoutsForTest reports how many items this cache has laid out. A frame whose items
// are unchanged must not move it.
func (c *RenderCache) layoutsForTest() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.layouts
}

// renderSig fingerprints EVERY field of an item that its render reads.
//
// IT MUST COVER ALL OF THEM, and this is the one place this cache can be wrong: a
// field left out means an item's rendered form is remembered from a state the item is
// no longer in, and the transcript quietly shows something stale. Two things keep that
// honest — the list below is written to match renderItem (and copyTextFor, which the
// spans read), and TestCachedRenderMatchesUncachedRender drives a mutation of each
// field through a warm cache and compares against the cacheless render. So an omission
// fails a test rather than a screenshot.
//
// FNV-1a over the fields, with a separator after each string so that two adjacent
// fields cannot be shuffled into the same bytes ("ab","c" must not equal "a","bc").
func renderSig(it ChatItem) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	str := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	num := func(n int64) {
		binary.LittleEndian.PutUint64(buf[:], uint64(n))
		h.Write(buf[:])
	}
	flag := func(b bool) {
		if b {
			h.Write([]byte{1})
			return
		}
		h.Write([]byte{0})
	}

	str(string(it.Kind))
	str(it.Key)
	str(it.Text)
	str(it.Source)
	str(it.Phase)
	str(it.AskID)
	flag(it.Live)
	num(it.At)

	// The user's band carries its attachment markers.
	for _, a := range it.Attachments {
		str(a)
	}

	if t := it.Tool; t != nil {
		str(t.ID)
		str(t.ToolName)
		str(t.Input)
		str(t.Output)
		num(t.At)
	}

	// An artifact row draws its name, type and the first line of its content.
	str(it.Name)
	str(it.Type)
	str(it.Content)

	// A session row draws its transport identity.
	str(it.SessionID)
	str(it.ServeURL)
	str(it.AdapterKind)

	if a := it.Ask; a != nil {
		str(a.Question)
		flag(a.AllowOther)
		flag(a.Answered)
		str(a.AnswerText)
		// The refused state is drawn too (a record instead of a card, and a different word), so it is part of
		// the fingerprint: a card whose refusal landed between two frames must repaint.
		flag(a.Refused)
		str(a.RefusalText)
		// The free-text row's live state: an open input row and what is typed in it are both part
		// of the drawn card.
		flag(a.Drafting)
		str(a.Draft)
		for _, o := range a.Options {
			str(o.Label)
			str(o.Description)
		}
	}

	if c := it.Consent; c != nil {
		// The ask the card draws from.
		a := c.Ask
		str(a.ID)
		str(a.ConvID)
		str(a.Tool)
		str(a.Target)
		str(a.Directory)
		str(string(a.Kind))
		str(a.Question)
		str(a.Summary)
		str(a.DeniedBy)
		flag(a.AllowOther)
		for _, o := range a.Options {
			str(o)
		}
		// The card's live state: the highlight, whether it is settled and how, and the
		// free-text row the operator may be typing into.
		num(int64(c.Sel))
		str(string(c.Decision))
		str(c.Choice)
		str(c.Note)
		flag(c.OtherMode)
		str(c.OtherInput)
	}

	return h.Sum64()
}
