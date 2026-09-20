package askorchicon

import (
	"sync"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// turn_hub.go — per-conversation live-turn broadcast hub for Ask Orchicon.
//
// The dispatch stream (ChatStream / InterjectConversationTurn) owns a
// private streamEventCh fed by the detached collector's onStreamEvent. When
// that socket drops mid-reply, the collector keeps running server-side but
// nothing re-dials the live stream — bubbles freeze until the ListMessages
// poll resolves. The hub fixes the liveness half: every response published
// to the dispatch channel is ALSO published to the conversation's hub, and
// WatchTurnStream attaches a NEW subscriber to the in-flight turn's hub
// without dispatching — live TextChunk/ReasoningChunk/Heartbeat chunks
// resume appending to the SAME bubbles.
//
// Lifecycle: startConversationTurn creates (or replaces, on supersede) the
// hub entry alongside the turn registry entry; the collector removes it on
// finalize (success or error) so a watch on a finished turn gets NotFound
// and falls back to the poll. Buffering is bounded (64, matching the
// dispatch channel); a slow watcher drops events rather than parking the
// collector — completion still resolves via the poll.

// turnHub is one conversation's broadcast point.
type turnHub struct {
	mu   sync.Mutex
	subs map[uint64]chan *apiv1.ChatStreamResponse
	next uint64
}

// turnHubRegistry maps conversation id → live hub.
type turnHubRegistry struct {
	mu   sync.Mutex
	hubs map[string]*turnHub
}

func newTurnHubRegistry() *turnHubRegistry {
	return &turnHubRegistry{hubs: make(map[string]*turnHub)}
}

// create replaces any existing hub for convID with a fresh one (a supersede
// owns the conversation now — stale watchers drain on the closed channel
// and fall back to the poll) and returns it. Nil-receiver safe: Services
// built without New() (unit tests) get no hub rather than a panic — the
// poll remains the completion path.
func (r *turnHubRegistry) create(convID string) *turnHub {
	if r == nil {
		return nil
	}
	h := &turnHub{subs: make(map[uint64]chan *apiv1.ChatStreamResponse)}
	r.mu.Lock()
	if old, ok := r.hubs[convID]; ok {
		r.mu.Unlock()
		old.close()
		r.mu.Lock()
	}
	r.hubs[convID] = h
	r.mu.Unlock()
	return h
}

// get returns the live hub for convID, if any.
func (r *turnHubRegistry) get(convID string) (*turnHub, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hubs[convID]
	return h, ok
}

// remove drops the hub for convID (called by the collector on finalize)
// and closes it so watchers fall back to the poll.
func (r *turnHubRegistry) remove(convID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	h, ok := r.hubs[convID]
	if ok {
		delete(r.hubs, convID)
	}
	r.mu.Unlock()
	if ok {
		h.close()
	}
}

// publish fans one response out to every subscriber (non-blocking — a slow
// watcher drops the event; the poll is the durable fallback).
func (h *turnHub) publish(resp *apiv1.ChatStreamResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- resp:
		default:
		}
	}
}

// subscribe attaches a watcher; the returned channel closes when the hub
// does (turn finalized or superseded). The caller MUST call unsubscribe
// (or rely on close) to avoid leaking the entry.
func (h *turnHub) subscribe() (uint64, <-chan *apiv1.ChatStreamResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan *apiv1.ChatStreamResponse, 64)
	h.subs[id] = ch
	return id, ch
}

// unsubscribe detaches a watcher and closes its channel.
func (h *turnHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// close terminates every subscriber (turn finalized or superseded).
func (h *turnHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}
