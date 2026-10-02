package orchicon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge implements the Ask chat-session
// capability, including the attachment-aware sender (parity with the
// opencode adapter — attachments ride the turn inline, never silently
// dropped).
var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*NativeBridge)(nil)
var _ scheduler.ConversationHistoryPurger = (*NativeBridge)(nil)

// Attachment caps mirror the server-side turn validation
// (startConversationTurnOpts): the bridge enforces them too so direct
// interface callers get the same bounds.
const (
	askMaxAttachments           = 5
	askMaxAttachmentBytes       = 10 * 1024 * 1024
	askMaxAttachmentsTotalBytes = 20 * 1024 * 1024
)

// AskToolProvider supplies the Ask-time tool surface for native turns.
// It is implemented outside this package (the askorchicon tool registry
// owns the product tools; the server injects it via SetAskTools) so the
// provider substrate never imports the product layer.
type AskToolProvider interface {
	// AskToolDefs returns the tool definitions offered to the model. It takes the TURN's context because the
	// mode boundary is part of the surface: a mode that may not do the work is not offered the tools that would.
	AskToolDefs(ctx context.Context) []ToolDef
	// ExecuteAskTool runs one tool call and returns its result text.
	ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error)
}

// chatBus is the per-turn SessionBus the native bridge feeds from its drain
// goroutine. It is the adapter-neutral surface the askorchicon collector
// consumes; the bridge maps the provider's normalized TurnStream events onto
// the SessionEvent vocabulary (idle/error/delta/part).
type chatBus struct {
	events chan scheduler.SessionEvent
	done   chan struct{}
	once   sync.Once

	// mu makes Close race-free against emit. Close closes done FIRST (which stops
	// any NEW emit from starting a send) and only then takes the write lock —
	// which waits out every in-flight emit — before closing events. Without this
	// ordering a send can land on a channel that was just closed, which is a
	// PANIC, not an error: it took the whole serve process down. Observed: an
	// interjection superseded a turn, the superseded turn's deferred Close ran
	// while a tool call from the other turn was still emitting, and the process
	// died with "panic: send on closed channel" in executeToolCalls.
	//
	// Closing done first is what keeps this cheap: the blocking terminal send
	// below can never hold RLock indefinitely, so Lock cannot deadlock against a
	// consumer that has gone away.
	mu sync.RWMutex

	// users counts the turns currently using this bus. A bus is SHARED by
	// consecutive turns of a conversation: an interjection adopts the bus of the
	// turn it supersedes (chatBuses is keyed per conversation). Closing it when
	// any one turn finished therefore closed it out from under a still-running
	// turn, which panicked the process and ended the live turn's stream. The bus
	// now closes when the LAST user leaves. Guarded by mu.
	users int
}

// chatBusCapacity is the per-turn event buffer.
//
// It was 32, which is too small for the volume a real turn produces now that a
// tool call emits BOTH a start (tool_part) and a resolution (tool_result) on top
// of the deltas and parts: a twelve-round tool turn reaches ~34 events, i.e.
// straight past the old bound, and whether the terminal idle survived came down
// to how fast the consumer happened to drain (TestChatTurnClientManyToolRounds-
// Unbounded failed in isolation and passed under load, purely on that race).
//
// Sized with real headroom rather than at the observed edge: a turn that touches
// many files or runs many tools must not be able to fill this. An oversized
// buffer costs a few KB per in-flight turn; a too-small one costs a wedged turn.
// emit still protects the terminal events independently (see emit), so this is
// the first line of defence and not the only one.
const chatBusCapacity = 256

func newChatBus() *chatBus {
	return &chatBus{events: make(chan scheduler.SessionEvent, chatBusCapacity), done: make(chan struct{})}
}

func (b *chatBus) Events() <-chan scheduler.SessionEvent { return b.events }
func (b *chatBus) Done() <-chan struct{}                 { return b.done }
func (b *chatBus) Close() {
	b.once.Do(func() {
		close(b.done)   // 1. no NEW emit may begin a send
		b.mu.Lock()     // 2. wait out every in-flight emit
		close(b.events) // 3. now no sender can be inside emit
		b.mu.Unlock()
	})
}

// adopt registers one more turn using this bus. Paired with release: every
// adopting turn must release exactly once.
func (b *chatBus) adopt() {
	b.mu.Lock()
	b.users++
	b.mu.Unlock()
}

// release gives up one turn's use of this bus, closing it only when no user
// remains.
//
// It exists because the bus is shared per conversation: an interjection's turn
// can adopt the same bus as the turn it supersedes. Closing on the first turn
// to finish is what killed the process (a live turn's emit landed on the closed
// channel) and ended the live turn's stream. Refcounting is the ownership
// signal that "close only if I am still the registered bus" cannot provide:
// when turns share ONE bus, every one of them is the registered bus.
func (b *chatBus) release() {
	b.mu.Lock()
	b.users--
	last := b.users <= 0
	b.mu.Unlock()
	if last {
		b.Close()
	}
}

// emit pushes one event onto the bus. Never blocks for an ordinary signal; a
// TERMINAL signal waits for room instead of being dropped.
//
// WHY THE DISTINCTION EXISTS. The buffer is small (32) and the collector drains
// it concurrently, so the best-effort drop is fine for the high-volume,
// reconstructible signals (a delta, a part, a tool_result) — losing one costs a
// repaint, not a turn. It is NOT fine for the signal that ENDS the turn: an
// `idle` dropped because the buffer happened to be full leaves the collector
// waiting forever, and the turn wedges with no error anywhere. That was
// reachable only under a burst before; emitting a tool_result per tool call made
// it reachable in an ordinary multi-tool turn, and
// TestChatTurnClientManyToolRoundsUnbounded caught it ("no idle at turn end").
//
// Waiting is safe: the collector is actively draining, so a full buffer empties
// promptly. The done channel bounds the wait so a consumer that has gone away
// cannot hang this goroutine.
func (b *chatBus) emit(evt scheduler.SessionEvent) {
	// A closed bus means this turn was superseded: its events have no consumer,
	// and sending on the closed channel would panic the whole process.
	//
	// Liveness is re-checked under the lock Close takes, so this cannot race the
	// close: either we hold RLock across the send (and Close waits for us), or
	// Close has finished and the check sees it. The cheap unsynchronised check
	// first keeps the common path lock-light.
	select {
	case <-b.done:
		return
	default:
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	select {
	case <-b.done:
		return
	default:
	}

	select {
	case b.events <- evt:
		return
	default:
	}
	if !terminalEventKind(evt.Kind) {
		// Best-effort by design: dropped when the buffer is full.
		return
	}
	select {
	case b.events <- evt:
	case <-b.done:
	}
}

// terminalEventKind reports whether losing this event would leave the collector
// unable to finish the turn. `idle` ends a turn; `error` fails it. Everything
// else is progress reporting that a client can reconstruct or do without.
func terminalEventKind(kind string) bool {
	return kind == "idle" || kind == "error"
}

// SetAskTools injects the Ask-time tool surface for native turns (the
// askorchicon product tools). Nil (default) keeps the pre-tools behavior:
// the model answers from the system prompt with no tool calls.
func (b *NativeBridge) SetAskTools(p AskToolProvider) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askTools = p
}

// SetAskHistoryDir enables disk persistence for Ask session histories
// (one JSON file per session under dir). Empty disables persistence
// (memory-only). Wired from the server instance data dir.
func (b *NativeBridge) SetAskHistoryDir(dir string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askHistoryDir = dir
}

// SetAskCompactNotice wires the durable record for an Ask compaction. The server
// points it at the Ask service, which owns the transcript; leaving it nil keeps the
// adapter's own log as the only trace. Guarded by mu.
func (b *NativeBridge) SetAskCompactNotice(fn scheduler.AskCompactNoticeFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askCompactNotice = fn
}

// reportAskCompaction hands a completed compaction to the wired notice sink.
//
// It ALWAYS writes the log line first and treats the sink as best-effort: the
// compaction has already happened and cannot be undone, so a failed notice must not
// propagate as a turn failure. The one thing it must never do is fail silently AND
// unlogged, which is why the error is warned rather than dropped.
func (b *NativeBridge) reportAskCompaction(ctx context.Context, conversationID, sessionID, reason string, res scheduler.ChatCompaction) {
	if !res.Compacted {
		return // nothing collapsed — nothing worth a marker
	}
	b.mu.Lock()
	sink := b.askCompactNotice
	b.mu.Unlock()
	if sink == nil {
		return
	}
	// WithoutCancel: the notice describes work ALREADY DONE, so a turn that ends or
	// is aborted mid-record must not lose it — the same reasoning, and the same
	// technique, as askUsageSink.
	if err := sink(context.WithoutCancel(ctx), scheduler.AskCompactNotice{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Reason:         reason,
		Detail:         res.Detail,
		ArchivePath:    res.ArchivePath,
		TokensBefore:   res.TokensBefore,
		TokensAfter:    res.TokensAfter,
	}); err != nil {
		b.log.Warn("orchicon: Ask compaction notice was not recorded — the collapse itself stands, but the operator will not see it in the transcript",
			"session", sessionID, "conversation", conversationID, "reason", reason, "error", err)
	}
}

// askHistoryVersion versions the on-disk Ask history envelope.
const askHistoryVersion = 1

// askHistoryMaxBytes caps one persisted session file (image attachments
// are base64 data URLs — a long image-heavy conversation could otherwise
// grow the file without bound). It is a cap on what is WRITTEN, never a reason
// to write nothing: see marshalAskHistoryForStorage for what a history that
// exceeds it is reduced to, and why writing nothing was the worse answer.
const askHistoryMaxBytes = 16 * 1024 * 1024

// askHistoryFilename sanitizes a session id into a safe file stem.
func askHistoryFilename(sessionID string) string {
	var sb strings.Builder
	for _, r := range sessionID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "session"
	}
	return sb.String()
}

// sanitizeHistoryLocked runs the replay-repair sanitize and, when it actually REMOVES something, records that
// as an intended reduction for the persist that follows.
//
// IT EXISTS TO KEEP THE LOSS GUARD HONEST. sanitizeChatHistory deliberately drops an orphaned tool result (a
// result whose call no preceding assistant message declares) and the messages left empty by that, because a
// provider rejects the shape. Those drops are a REPAIR, not data loss — but the guard cannot tell the two
// apart by looking at the count, so without this a legitimate repair would be reported as the data-loss bug,
// and a guard that cries wolf is a guard nobody reads. The repair is still REPORTED, just as intended, with
// its own reason.
func (b *NativeBridge) sanitizeHistoryLocked(sessionID string, h []Message) []Message {
	out := sanitizeChatHistory(h)
	if len(out) < len(h) {
		b.markHistoryReductionLocked(sessionID,
			fmt.Sprintf("replay repair removed %d unpaired message(s)", len(h)-len(out)))
	}
	return out
}

// markHistoryReductionLocked declares that the NEXT persistence for this session is an INTENTIONAL lossy
// reduction, so the shrink guard reports it as intended rather than as data loss. Call with b.mu held,
// immediately before persisting. Both reduction paths (context reduction, compaction) are lossy by design and
// both tell the operator; this is what keeps that distinction visible in the logs too.
func (b *NativeBridge) markHistoryReductionLocked(sessionID, reason string) {
	if b.askHistoryReduceReason == nil {
		b.askHistoryReduceReason = map[string]string{}
	}
	b.askHistoryReduceReason[sessionID] = reason
}

// checkHistoryLostMessagesLocked reports a session that LOST a message it previously held. Call with b.mu
// held, from the one choke point every history mutation passes (persistAskHistoryLocked).
//
// NO MESSAGE MAY DISAPPEAR FROM A SESSION, except by an announced reduction. That invariant is what the
// operator's "Orchicon is not usable if you can't have a session that remembers what it's doing" is asking
// for, and it is exactly what was violated silently for weeks:
//
//	commitChatHistory assigned `cur = working` — the committing turn's own snapshot-plus-output — so two
//	overlapping turns destroyed each other's messages. An interjection SUPERSEDES the running turn, so this
//	was the operator's normal path, and the damage looked like nothing: a run of consecutive USER messages in
//	the session file, because a user message rides the next turn's snapshot while a reply exists only in the
//	turn that produced it. The replies stayed in the transcript and on screen, so reading the session files by
//	hand was the only way to see it.
//
// THE COMPARISON IS BY MESSAGE IDENTITY, NOT BY COUNT, and that is the whole difference between a guard that
// works and one that does not. The replace SWAPS rather than truncates — H+[userA]+[userB] became
// H+[userA]+replyA, the same length with one message substituted — and the commit after it grew the history
// again. Neither a length check nor a per-role count check can see that; the missing fingerprint can.
//
// The report is at error level when nothing declared it, names the session and how many messages went, and
// says what it MEANS — the next person to see this line should not have to reconstruct the diagnosis to know
// that the model has lost work the operator can still read.
func (b *NativeBridge) checkHistoryLostMessagesLocked(sessionID string) {
	cur := b.chatHistory[sessionID]
	reason, intended := b.askHistoryReduceReason[sessionID]
	delete(b.askHistoryReduceReason, sessionID)

	now := make(map[uint64]int, len(cur))
	for _, m := range cur {
		now[messageFingerprint(m)]++
	}
	prev, seen := b.askHistorySeen[sessionID]
	if b.askHistorySeen == nil {
		b.askHistorySeen = map[string]map[uint64]int{}
	}
	b.askHistorySeen[sessionID] = now

	if intended {
		if seen {
			b.log.Info("orchicon: ask session history reduced as intended",
				"session", sessionID, "from", totalCount(prev), "to", len(cur), "reason", reason)
		}
		return
	}
	if !seen {
		return // first observation: nothing to compare against
	}
	lost := 0
	for fp, n := range prev {
		if now[fp] < n {
			lost += n - now[fp]
		}
	}
	if lost == 0 {
		return // the invariant held
	}
	b.log.Error("orchicon: ASK SESSION HISTORY LOST MESSAGES WITHOUT AN INTENTIONAL REDUCTION — the model has lost messages that are still in the transcript, so it will repeat work the operator can see it already did. This is the data-loss bug (see commitChatHistory); the session no longer matches the durable record.",
		"session", sessionID, "lost", lost, "before", totalCount(prev), "after", len(cur))
}

// totalCount sums a fingerprint multiset.
func totalCount(m map[uint64]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

// messageFingerprint identifies a message's CONTENT, for the "no message may disappear" invariant above. It
// covers every field a replayed history carries, so a message that was rewritten rather than removed also
// reads as gone — which is correct: the model can no longer see what it used to.
func messageFingerprint(m Message) uint64 {
	h := fnv.New64a()
	io.WriteString(h, string(m.Role))
	for _, c := range m.Content {
		switch {
		case c.Text != nil:
			io.WriteString(h, "\x01")
			io.WriteString(h, *c.Text)
		case c.Image != nil:
			io.WriteString(h, "\x02")
			io.WriteString(h, *c.Image)
		case c.ToolUse != nil:
			io.WriteString(h, "\x03")
			io.WriteString(h, c.ToolUse.ToolCallID)
			io.WriteString(h, c.ToolUse.Name)
			io.WriteString(h, c.ToolUse.ArgsJSON)
		case c.ToolResult != nil:
			io.WriteString(h, "\x04")
			io.WriteString(h, c.ToolResult.ToolCallID)
			io.WriteString(h, c.ToolResult.Content)
			if c.ToolResult.IsError {
				io.WriteString(h, "E")
			}
		}
	}
	return h.Sum64()
}

// persistAskHistoryLocked writes the session's history to disk (atomic
// tmp + rename). Callers must hold b.mu. Best-effort: every failure is a
// warn + return, never a turn failure.
//
// IT NEVER SILENTLY STOPS SAVING, which is what the oversize case used to do.
// The old shape returned early — writing nothing — the moment the serialized
// history passed askHistoryMaxBytes. That is a CLIFF, not a cap: the file on disk
// froze at the last snapshot that fit, so every turn after it was persisted
// nowhere at all, and the next restart reloaded a stale transcript. On a history
// that never fit in the first place there was no file, so a restart reached the
// model with only the system prompt's short history digest — a long conversation
// losing its context at the precise point it had the most to lose. The history is
// now always written: plain JSON while it fits, compressed when it does not, and
// with its image payloads replaced by explicit markers only if even the
// compressed form is over the cap. Every reduction is logged, so a persisted file
// is never a reduced one by silence.
func (b *NativeBridge) persistAskHistoryLocked(sessionID string) {
	// THE SHRINK GUARD RUNS FIRST, before the persistence decisions below, because it protects the session
	// HISTORY rather than the FILE: a history that lost messages has lost them whether or not this call
	// manages to write. Every mutation of b.chatHistory ends in a call here (dispatch, commit, context
	// reduction, compaction), so this is the one place that sees all of them.
	b.checkHistoryLostMessagesLocked(sessionID)
	dir := b.askHistoryDir
	if dir == "" {
		return
	}
	raw := b.marshalAskHistoryForStorage(sessionID, b.chatHistory[sessionID])
	if raw == nil {
		return // nothing writable — marshalAskHistoryForStorage has already logged why
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.log.Warn("orchicon: ask history dir create failed", "dir", dir, "error", err)
		return
	}
	path := filepath.Join(dir, askHistoryFilename(sessionID)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		b.log.Warn("orchicon: ask history write failed", "session", sessionID, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		b.log.Warn("orchicon: ask history rename failed", "session", sessionID, "error", err)
	}
}

// marshalAskHistoryForStorage serializes a session history for persistence and
// returns the bytes to write, or nil when nothing can be written (already
// logged).
//
// THREE SHAPES, IN ORDER OF FIDELITY, because the only wrong answer is writing
// nothing:
//
//  1. PLAIN JSON while it fits under the cap. This is the format every earlier
//     release wrote and can still read, so an ordinary conversation's file is
//     unchanged and a rollback to an older binary keeps its history.
//  2. GZIP when plain JSON would exceed the cap. The envelope is the same — only
//     the framing differs, which is why loadAskHistoryLocked detects it by magic
//     bytes rather than by the version field. gzip is very effective on this
//     content (long JSON, repetitive tool payloads, base64 image data), so a
//     history that would not fit uncompressed fits whole, and NOTHING is lost.
//     An older binary cannot read this shape — but an older binary would have
//     written nothing at this size anyway, so a rollback is no worse off.
//  3. GZIP WITH IMAGE PAYLOADS DROPPED, as the last resort. Images were 46% of
//     the bytes on the over-limit conversation that motivated the pressure gate,
//     so dropping them is what brings a genuinely enormous history back under the
//     cap — and every affected image is replaced by an explicit marker rather
//     than vanishing, so the surviving transcript still says one was there.
func (b *NativeBridge) marshalAskHistoryForStorage(sessionID string, history []Message) []byte {
	raw, err := marshalAskHistory(history)
	if err != nil {
		b.log.Warn("orchicon: ask history marshal failed", "session", sessionID, "error", err)
		return nil
	}
	if len(raw) <= askHistoryMaxBytes {
		return raw
	}
	if gz, err := gzipAskHistory(raw); err == nil && len(gz) <= askHistoryMaxBytes {
		b.log.Info("orchicon: ask history compressed to fit the persistence cap",
			"session", sessionID, "json_bytes", len(raw), "gz_bytes", len(gz), "cap", askHistoryMaxBytes)
		return gz
	}
	reduced, images := trimAskHistoryImages(history)
	raw, err = marshalAskHistory(reduced)
	if err != nil {
		b.log.Warn("orchicon: ask history marshal failed after dropping image payloads", "session", sessionID, "error", err)
		return nil
	}
	gz, err := gzipAskHistory(raw)
	if err != nil {
		b.log.Warn("orchicon: ask history compress failed after dropping image payloads", "session", sessionID, "error", err)
		return nil
	}
	if len(gz) > askHistoryMaxBytes {
		// AN ERROR, NOT A WARNING, and the consequence is spelled out: the file on disk is now STALE by
		// everything committed since the last successful persist, and the load path only consults the file when
		// the in-memory history is EMPTY — i.e. exactly after a restart. So this state means "a restart loses
		// every message since then", silently, which is the failure mode this whole guard exists to stop.
		b.log.Error("orchicon: ask history is oversize even after dropping image payloads — the FILE ON DISK IS NOW STALE, and a restart will reload it and LOSE every message committed since the last successful persist",
			"session", sessionID, "gz_bytes", len(gz), "cap", askHistoryMaxBytes, "images_dropped", images)
		return nil
	}
	b.log.Warn("orchicon: ask history persisted with image payloads dropped to stay within the persistence cap — text and tool context are preserved, the images are not",
		"session", sessionID, "gz_bytes", len(gz), "images_dropped", images)
	return gz
}

// askHistoryImageDroppedMarker replaces an image payload in a persisted history
// that had to be reduced. A marker rather than a silent removal: the transcript a
// restart reloads still records that an image was part of the turn.
const askHistoryImageDroppedMarker = "[an image was shared here — its data was dropped when this history was persisted, to keep the file within its storage cap]"

// marshalAskHistory marshals the versioned on-disk envelope.
func marshalAskHistory(history []Message) ([]byte, error) {
	return json.Marshal(map[string]any{"version": askHistoryVersion, "messages": history})
}

// gzipAskHistory compresses a marshalled envelope.
func gzipAskHistory(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipAskHistory reverses gzipAskHistory.
func gunzipAskHistory(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// trimAskHistoryImages returns a copy of the history with every image data URL
// replaced by askHistoryImageDroppedMarker, plus the number of images replaced.
// The copy shares no mutable content with the input, so the LIVE in-memory
// history keeps its images — only what is written to disk is reduced.
func trimAskHistoryImages(history []Message) ([]Message, int) {
	out := make([]Message, len(history))
	dropped := 0
	for i, m := range history {
		out[i] = m
		if len(m.Content) == 0 {
			continue
		}
		content := make([]Content, len(m.Content))
		copy(content, m.Content)
		for j := range content {
			if content[j].Image == nil {
				continue
			}
			marker := askHistoryImageDroppedMarker
			content[j].Image = nil
			content[j].Text = &marker
			dropped++
		}
		out[i].Content = content
	}
	return out, dropped
}

// loadAskHistoryLocked reads a persisted session history from disk (nil on
// a miss or any failure — a new conversation looks exactly like a lost
// file, and both correctly start empty). Callers must hold b.mu.
//
// It reads BOTH persisted shapes: plain JSON (what every release before the
// storage-cap fix wrote, and what still gets written while a history fits) and
// gzip (what an oversize history is compressed into). The framing is detected by
// the gzip magic bytes rather than by the envelope's version, so neither shape
// needs a version bump and a file written by either release loads.
func (b *NativeBridge) loadAskHistoryLocked(sessionID string) []Message {
	dir := b.askHistoryDir
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, askHistoryFilename(sessionID)+".json"))
	if err != nil {
		return nil
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		plain, derr := gunzipAskHistory(raw)
		if derr != nil {
			b.log.Warn("orchicon: ask history decompress failed — starting empty", "session", sessionID, "error", derr)
			return nil
		}
		raw = plain
	}
	var env struct {
		Version  int       `json:"version"`
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != askHistoryVersion {
		b.log.Warn("orchicon: ask history unreadable — starting empty", "session", sessionID, "error", err)
		return nil
	}
	return env.Messages
}

// askToolsLocked returns the injected tool definitions (nil when no
// provider is set). Callers must hold b.mu.
func (b *NativeBridge) askToolsLocked(ctx context.Context) []ToolDef {
	if b.askTools == nil {
		return nil
	}
	return b.askTools.AskToolDefs(ctx)
}

// ToolsRunInProcess implements scheduler.InProcessToolRunner.
//
// THE NATIVE BRIDGE RUNS THE MODEL'S TOOLS HERE. The file/shell suite (bash, read, write, batch_*, …) is
// executed in this process by orchicon.HostTools, and bash carries its own hard deadline
// (bashTimeoutDefault 120s, bashTimeoutMax 600s) enforced by exec.CommandContext — so a slow shell command is
// a RUNNING one, not a wedged one, and its tool result always arrives to close the call.
//
// Ask Orchicon reads this to stop judging those calls by silence (see scheduler.InProcessToolRunner). It is a
// declaration about WHERE a call runs, not a promise that a tool can never hang: a call this bridge cannot
// finish is bounded by the turn's own reply window instead of being healed by a session recycle.
func (b *NativeBridge) ToolsRunInProcess() bool { return true }

// CreateConversationSession implements scheduler.ChatTurnClient. The native
// transports are sessionless, so this returns a synthetic session id and
// initializes an empty in-memory history under it. No server-side session
// exists; the history is re-sent as full context per turn (D2).
func (b *NativeBridge) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	if conversationID == "" {
		return "", errors.New("orchicon bridge: create conversation session requires a conversation id")
	}
	sid := "orchicon-ask:" + conversationID
	b.mu.Lock()
	if b.chatHistory == nil {
		b.chatHistory = map[string][]Message{}
	}
	if _, ok := b.chatHistory[sid]; !ok {
		b.chatHistory[sid] = nil
	}
	b.mu.Unlock()
	return sid, nil
}

// PurgeConversationHistory implements scheduler.ConversationHistoryPurger:
// it discards a deleted conversation's durable Ask history — the in-memory
// map entry AND the persisted JSON file — because nothing else reclaims them.
// The native adapter is sessionless, so the history file is the only on-disk
// artifact of a conversation; without this, every deleted conversation leaked
// its file forever (observed: 19 conversation files totalling 19MB, several of
// them multi-MB).
//
// Idempotent: a missing map entry or missing file is a successful no-op. The
// caller treats any error as advisory (logged, never failing the delete RPC),
// since the durable DB record is already gone by the time this runs.
func (b *NativeBridge) PurgeConversationHistory(_ context.Context, conversationID, sessionID string) error {
	sid := sessionID
	if sid == "" && conversationID != "" {
		// Sessionless adapters mint their own synthetic id; resolve it so a
		// conversation row without a persisted session id still purges.
		sid = scheduler.NativeSessionIDPrefix + conversationID
	}
	if sid == "" {
		return nil
	}
	b.mu.Lock()
	delete(b.chatHistory, sid)
	delete(b.askHistorySeen, sid)
	delete(b.askHistoryReduceReason, sid)
	// The pressure bookkeeping describes a conversation that no longer exists:
	// drop it with the history so a deleted conversation leaves no memory behind
	// (nor a stale measurement that could fire a gate on a NEW conversation that
	// happened to reuse the session id).
	delete(b.askPromptTokens, sid)
	delete(b.askWindowTokens, sid)
	dir := b.askHistoryDir
	b.mu.Unlock()
	if dir == "" {
		return nil // memory-only history: nothing persisted to reclaim
	}
	path := filepath.Join(dir, askHistoryFilename(sid)+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("orchicon bridge: purge ask history %s: %w", path, err)
	}
	// Sweep the atomic-write temp file too: a crash mid-persist can leave it
	// behind, and a deleted conversation must leave nothing on disk.
	_ = os.Remove(path + ".tmp")
	return nil
}

// Subscribe implements scheduler.ChatTurnClient: it returns a fresh buffered
// SessionBus for the conversation's next turn and registers it under the
// conversation's session so SendTurnMessage feeds it. The bus is fed by the
// drain goroutine started in SendTurnMessage and closed when the turn ends.
func (b *NativeBridge) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	bus := newChatBus()
	b.mu.Lock()
	if b.chatBuses == nil {
		b.chatBuses = map[string]*chatBus{}
	}
	b.chatBuses[conversationID] = bus
	b.mu.Unlock()
	return bus, nil
}

// SendTurnMessage implements scheduler.ChatTurnClient. It appends the user
// message to the session's in-memory history, resolves the provider from the
// model_ref through the registry credential path, and streams ONE turn via
// Provider.StreamTurn. The stream is started SYNCHRONOUSLY (a pre-stream
// failure — auth/connect — returns the error so the collector fails the turn
// as a send-accept failure); the drain goroutine then maps events onto the
// bus and returns nil (accepted). This ordering guarantees the collector's
// `sent` guard never drops the first delta (D4).
func (b *NativeBridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, nil)
}

// SendTurnMessageWithAttachments implements
// scheduler.SendTurnMessageWithAttachments (parity with the opencode
// adapter): images ride as image data-URL parts (every native wire
// consumes data URLs) and UTF-8 text files inline as fenced text parts —
// one POST, no upload-path dependency. Non-image, non-UTF8 binaries have
// no native wire shape and fail loudly naming the file (never silently
// dropped); those need the opencode adapter's document handling.
func (b *NativeBridge) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, attachments)
}

// askUserContent builds the user message content for a turn: the text plus
// one content element per attachment (image → Image data URL, UTF-8 text →
// fenced Text). Caps mirror the server-side validation.
func askUserContent(text string, attachments []scheduler.ChatAttachment) ([]Content, error) {
	var content []Content
	if text != "" {
		t := text
		content = append(content, Content{Text: &t})
	}
	if len(attachments) > askMaxAttachments {
		return nil, fmt.Errorf("orchicon bridge: too many attachments (%d, max %d)", len(attachments), askMaxAttachments)
	}
	total := 0
	for _, a := range attachments {
		if len(a.Data) == 0 {
			continue
		}
		if len(a.Data) > askMaxAttachmentBytes {
			return nil, fmt.Errorf("orchicon bridge: attachment %q too large (max 10MB)", a.Name)
		}
		total += len(a.Data)
		if total > askMaxAttachmentsTotalBytes {
			return nil, fmt.Errorf("orchicon bridge: attachments too large (max 20MB total)")
		}
		mime := a.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		if strings.HasPrefix(mime, "image/") {
			u := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
			content = append(content, Content{Image: &u})
			continue
		}
		if utf8.Valid(a.Data) {
			name := a.Name
			if name == "" {
				name = "attachment"
			}
			fenced := "--- attachment: " + name + " (" + mime + ") ---\n" + string(a.Data)
			content = append(content, Content{Text: &fenced})
			continue
		}
		return nil, fmt.Errorf("orchicon bridge: attachment %q (%s) is a binary document the native wires cannot carry — use an opencode-adapter model for binary documents", a.Name, mime)
	}
	if len(content) == 0 {
		t := text
		content = append(content, Content{Text: &t})
	}
	return content, nil
}

func (b *NativeBridge) dispatchTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	if sessionID == "" {
		return errors.New("orchicon bridge: send turn requires a session id (create the conversation session first)")
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return errors.New("orchicon bridge: no tenant in context — cannot resolve the Ask provider")
	}
	providerID, model, ok := adapter.SplitForServe(modelRef)
	if !ok || providerID == "" || model == "" {
		return fmt.Errorf("orchicon bridge: Ask model ref %q has no provider/model", modelRef)
	}
	if b.resolver == nil {
		return errors.New("orchicon bridge: no provider resolver for the Ask turn")
	}
	prov, err := b.resolver.Get(ctx, tenantID, providerID)
	if err != nil {
		return fmt.Errorf("orchicon bridge: resolve Ask provider: %w", err)
	}

	// PROACTIVE CONTEXT GATE (askpressure.go): when the conversation's measured
	// prompt size has crossed the configured fraction of this model's live
	// context window, compact BEFORE dispatching — so the turn never reaches the
	// provider over-limit in the first place. Compaction rewrites the history
	// this turn is about to send, so it must run before the snapshot below.
	// Best-effort: a failure or a disarmed gate (no live window hint, no
	// measurement yet) leaves the turn untouched, and the reactive path
	// (askreduce.go) remains the backstop.
	b.maybeCompactForPressure(ctx, prov, conversationID, sessionID, modelRef, model)

	// Append the user message to the session's replayable history (committed
	// under the lock so a concurrent turn never double-appends). The
	// content carries the text plus any attachments (images as data URLs,
	// text files fenced) so follow-ups and tool rounds replay them.
	userContent, err := askUserContent(text, attachments)
	if err != nil {
		return err
	}
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sessionID]...)
	if len(history) == 0 {
		// Memory has nothing (fresh session or a server restart wiped
		// it) — reseed from the persisted file when present so the turn
		// re-sends the full context instead of starting over.
		history = append([]Message(nil), b.loadAskHistoryLocked(sessionID)...)
	}
	// REPLAY BOUNDARY: regardless of where the history came from (this
	// process, the persisted file, an interrupted prior turn), what is sent to
	// the provider is well-formed — and the repair is persisted, so a session
	// poisoned by a dangling tool call heals permanently instead of 400ing on
	// every subsequent turn.
	history = b.sanitizeHistoryLocked(sessionID, history)
	history = append(history, Message{Role: RoleUser, Content: userContent})
	b.chatHistory[sessionID] = history
	b.persistAskHistoryLocked(sessionID)
	b.mu.Unlock()

	// Build the turn request: the accumulated history re-sent as full context
	// (the sessionless emulation), the system prompt as a cacheable block,
	// and the Ask tool surface when one is injected (SetAskTools). Without
	// tools the model can only answer from the system prompt's project
	// context — with them it can query, read, and act like the host-serve
	// path.
	b.mu.Lock()
	tools := b.askToolsLocked(ctx)
	b.mu.Unlock()
	req := TurnRequest{
		Model: model,
		System: []SystemBlock{
			{Text: system, Cache: true},
		},
		Messages:     history,
		Tools:        tools,
		MaxTokens:    maxOutputTokens(),
		CacheControl: CacheControlSystemAndTools,
		// Stable per-conversation session id for OpenCode Zen/Go (D1): the
		// provider requires x-opencode-session per conversation.
		SessionID: conversationID,
	}
	// The turn context is derived from the request ctx so
	// AbortConversationSession can cancel it mid-flight (D7) — including
	// the provider HTTP calls on every tool round.
	turnCtx, cancel := context.WithCancel(ctx)
	// Start the stream SYNCHRONOUSLY so a pre-stream failure surfaces as a
	// send-accept failure (the collector fails the turn) rather than a
	// dropped first delta (D4). A provider context-window overflow is the one
	// failure with a deterministic remedy: reduce the replayed history and
	// retry (askreduce.go). Without that, a long conversation is permanently
	// wedged — every subsequent turn re-sends the same oversized history and
	// 400s (observed live on a 1574-message Ask session at ~1.02M tokens).
	stream, err := b.startTurnWithContextRecovery(turnCtx, prov, &req, sessionID)
	if err != nil {
		cancel()
		return fmt.Errorf("orchicon bridge: start Ask turn: %w", err)
	}
	// A reduction (when one happened) replaced the replayed history. Drain
	// against what the provider actually ACCEPTED, so the session's working
	// context matches the prompt it saw.
	history = req.Messages

	// Drain the stream on a goroutine, mapping events onto the bus.
	b.mu.Lock()
	if b.chatTurns == nil {
		b.chatTurns = map[string]context.CancelFunc{}
	}
	b.chatTurns[sessionID] = cancel
	bus := b.chatBuses[conversationID]
	if bus == nil {
		bus = newChatBus()
		b.chatBuses[conversationID] = bus
	}
	// This turn ADOPTS the bus, so it is not closed until every adopting turn is
	// done — an interjection shares the bus of the turn it supersedes. Paired
	// with the drain's release().
	bus.adopt()
	b.mu.Unlock()

	go b.drainChatTurn(turnCtx, prov, bus, stream, req, conversationID, sessionID, history, b.askUsageSink(tenantID, conversationID, sessionID, modelRef, providerID, model))

	// Return nil (accepted) BEFORE the drain goroutine emits, so the
	// collector observes every event with sent == true (D4).
	return nil
}

// drainChatTurn reads the provider's TurnStream(s) and maps events onto the
// SessionBus vocabulary (D3): TextDelta → delta(text) + accumulate into the
// part buffer; ReasoningDelta → delta(reasoning); ToolCall → tool_part +
// execute against the injected Ask tools and continue the turn with the
// results (agentic loop, unbounded by round count); StreamError → error;
// Finish → emit one part(text) with the accumulated reply, then idle. On
// completion the full working history (assistant texts, tool uses and tool
// results — not just the final text) replaces the session's history so a
// follow-up re-sends the complete context.
func (b *NativeBridge) drainChatTurn(ctx context.Context, prov Provider, bus *chatBus, stream TurnStream, req TurnRequest, conversationID, sessionID string, history []Message, usageSink func(context.Context, Usage)) {
	defer bus.release()
	defer func() {
		b.mu.Lock()
		delete(b.chatTurns, sessionID)
		b.mu.Unlock()
	}()

	// working is the turn's replayable history: it starts as the snapshot
	// committed in SendTurnMessage (prior turns + this user message) and
	// grows with every assistant message and tool result until commit.
	working := append([]Message(nil), history...)
	// reply accumulates EVERY round's assistant text into one consolidated
	// reply (the final part). Round text is never emitted as its own part:
	// a committed intermediate part would show up as a separate bubble that
	// overlaps the live delta stream (and, if the model re-states its
	// preamble, duplicates it).
	var reply strings.Builder
	// roundReply holds only the CURRENT round's text (for history replay,
	// which needs one assistant message per round).
	var roundReply strings.Builder
	var reasoning strings.Builder

	// emitTurnParts publishes what the turn accumulated as COMPLETED parts. It is split out
	// so the ABORT path can publish the same thing: an aborted turn has no idle (it did not
	// complete) but its work is just as real, and discarding it was the other half of the
	// data-loss bug — the collector's stall path aborts the session, this path returned
	// without emitting anything, and the whole turn's text and reasoning went with it.
	emitTurnParts := func() {
		if r := strings.TrimSpace(reasoning.String()); r != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "reasoning", Text: r})
		}
		if t := strings.TrimSpace(reply.String()); t != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
		}
	}
	// finishTurn emits the accumulated REASONING and then the consolidated reply, each as
	// ONE completed part, then idle, and commits the working history. The collector builds
	// the persisted record ONLY from part events, so these parts are what land in the DB.
	//
	// REASONING IS EMITTED AS A COMPLETED PART, exactly like text, and that symmetry is
	// the fix rather than a style choice. It used to be streamed as DELTAS ONLY and never
	// finalized, so it had no durable form at all: the collector's reasoning slice is built
	// from parts, its live reasoning tail is RESET by every completed text part, and an
	// aborted or stalled turn emits no part at all. The operator's report was exactly that
	// — "anything you were currently typing (mostly in thought) goes away" — because the
	// thinking existed only as deltas and deltas are not durable.
	//
	// REASONING FIRST: thinking precedes the answer, and the collector appends both to
	// ordered slices, so the transcript reads in the order the model produced them.
	finishTurn := func() {
		emitTurnParts()
		bus.emit(scheduler.SessionEvent{Kind: "idle"})
		b.commitChatHistory(sessionID, history, working)
	}

	for round := 0; ; round++ {
		roundDone, calls, usage, aborted := b.drainOneRound(ctx, bus, stream, &roundReply, &reasoning)
		// Report the round's REAL usage (never estimated). Emitted per ROUND
		// because each round is one provider call — so the newest sample's prompt
		// size is exactly the context pressure a gate needs, and a multi-round
		// turn does not collapse into a single misleading total.
		if usageSink != nil {
			usageSink(ctx, usage)
		}
		_ = stream.Close()
		if aborted {
			// Abort (D7): the turn was cancelled — finalize without COMMITTING.
			//
			// WITHOUT COMMITTING, NOT WITHOUT PUBLISHING. This used to `return` bare, on the
			// reasoning that the collector's own Stop path had already cancelled its context
			// so nobody was listening. That is true for a user Stop and FALSE for the case
			// that actually hurt: the STALL monitor aborts the session while the collector is
			// still live, and the turn then finalized with empty text and empty reasoning
			// while the operator had been watching both stream. Everything the round produced
			// is still in hand here, so it is published as completed parts (no idle — this
			// turn did not complete).
			//
			// Folding roundReply in first: drainOneRound accumulates the CURRENT round into
			// it and the merge into reply happens below this check, so on abort the round's
			// text exists only there.
			if rt := roundReply.String(); rt != "" {
				if reply.Len() > 0 {
					reply.WriteString("\n\n")
				}
				reply.WriteString(rt)
			}
			emitTurnParts()
			// AND COMMIT IT, so the SESSION agrees with the TRANSCRIPT.
			//
			// THIS IS THE SECOND HALF OF THE SAME DATA-LOSS BUG, and it is the one the operator kept
			// reporting as the model "losing its brain": "The model is constantly losing its brain. It
			// doesn't know it's already done things and then tries to do them again."
			//
			// The publish above fixed the VISIBLE half — an aborted turn hands over the work it produced,
			// so the collector persists it and every client renders it. But this path then returned
			// WITHOUT committing, deliberately ("the turn was cancelled — finalize without COMMITTING"),
			// and that left the two views of the conversation PERMANENTLY DISAGREEING:
			//
			//   * the TRANSCRIPT (and therefore the operator, and the GUI, and the TUI) contains the
			//     aborted reply — it is durable, it is on screen;
			//   * the SESSION does not. The next turn re-sends b.chatHistory (SendTurnMessage: "the
			//     accumulated history re-sent as full context"), which was never told about the reply, so
			//     the model is handed a history in which its own last words were never spoken.
			//
			// A model that cannot see what it just said repeats it, re-asks what it just asked, and
			// re-does work it already did — exactly the reported symptom, and the operator can watch it
			// happen because they are reading the transcript the session is not.
			//
			// WHY ABORT IS THE COMMON CASE HERE, not an edge: the collector aborts on a STALL, on STOP, and
			// on every SUPERSEDE. Interjecting is the operator's normal way to steer a running turn, and a
			// prod log shows one interjection superseding a turn every few minutes while this was being
			// diagnosed.
			//
			// Only THIS round's text is appended: earlier rounds are already in `working` via
			// appendAssistantText on the tool-round path, and re-adding `reply` would duplicate them. A
			// dangling tool call cannot leak in, because commitChatHistory sanitizes what it stores.
			if rt := strings.TrimSpace(roundReply.String()); rt != "" {
				b.appendAssistantText(&working, rt)
			}
			b.commitChatHistory(sessionID, history, working)
			return
		}
		roundText := roundReply.String()
		roundReply.Reset()
		if roundText != "" {
			if reply.Len() > 0 {
				reply.WriteString("\n\n")
			}
			reply.WriteString(roundText)
		}
		if !roundDone {
			// Stream ended without a Finish event — close out the turn so
			// the collector persists what arrived (pre-existing behavior
			// for a provider that ends without a terminal signal).
			b.appendAssistantText(&working, roundText)
			finishTurn()
			return
		}
		if len(calls) == 0 {
			b.appendAssistantText(&working, roundText)
			finishTurn()
			return
		}
		// Tool round: record the assistant's text (if any) plus the tool
		// uses, execute every call, and continue the turn with the results.
		// Tool results are part of the replayable history (unlike the
		// pre-tools turn, which committed text only).
		b.appendAssistantTurn(&working, roundText, calls)
		// The tool loop is unbounded by round count: it continues while the
		// model keeps issuing tool calls and terminates naturally when a
		// round returns none (the len(calls) == 0 → finishTurn() path above).
		// A pathological model that loops tool calls forever is bounded by
		// the time gates, not a round count: the reply window
		// (askReplyWindow(), default 30m, ORCHICON_ASK_REPLY_WINDOW), the
		// turn TTL sweeper (askTurnMaxAge(), default 31m,
		// ORCHICON_ASK_TURN_MAX_AGE — internal/askorchicon/chat.go), and the
		// stall monitor (tenant stall settings).
		b.executeToolCalls(ctx, bus, &working, calls, conversationID)
		req.Messages = append([]Message(nil), working...)
		next, err := prov.StreamTurn(ctx, req)
		if err != nil {
			// COMMIT WHAT THE TURN ALREADY PRODUCED BEFORE LEAVING. This is the abort path's own principle
			// ("everything the round produced is still in hand here, so it is published") applied to the place
			// it was still missing. By this point `working` holds every COMPLETED tool round — the assistant's
			// text, its tool calls and their results — and returning bare DISCARDS all of it, leaving the
			// session with the operator's message followed by nothing.
			//
			// IT IS THE SUPERSEDE PATH, AND IT IS NOT RARE. An interjection cancels the running turn
			// mid-loop, and the cancellation surfaces HERE — as a context.Canceled from the NEXT round's
			// StreamTurn — not through drainOneRound's aborted flag. So it bypassed the abort path's commit
			// entirely, and the abort-path fix (which this file already carried) could not help.
			//
			// MEASURED, on the operator's own conversation, on a binary that already had that abort-path fix:
			// a turn produced 123 chars of text and ran FOUR tools, the operator interjected, and the session
			// ended up holding two consecutive user messages with no trace of the turn between them — the work
			// was in the transcript and on the operator's screen, and invisible to the model.
			//
			// AND NO GUARD CAN SEE THIS ONE. The loss guard compares the session against what it last held, so
			// it catches a message that DISAPPEARS. This message never arrived — an omission, not a
			// disappearance — which is why the guard stayed silent while the work was lost.
			if errors.Is(err, context.Canceled) {
				b.commitChatHistory(sessionID, history, working)
				return
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			b.commitChatHistory(sessionID, history, working)
			return
		}
		stream = next
	}
}

// drainOneRound consumes one provider stream until its Finish event (or an
// error/early end), live-emitting deltas onto the bus and accumulating the
// reply text. It returns roundDone (a Finish event arrived), the complete
// tool calls issued this round, and aborted (the turn context was
// cancelled).
func (b *NativeBridge) drainOneRound(ctx context.Context, bus *chatBus, stream TurnStream, reply, reasoning *strings.Builder) (roundDone bool, calls []ToolCall, usage Usage, aborted bool) {
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil, Usage{}, true
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			return false, nil, Usage{}, false
		}
		if !ok {
			return false, nil, Usage{}, false
		}
		switch e := evt.(type) {
		case TextDelta:
			reply.WriteString(e.Text)
			bus.emit(scheduler.SessionEvent{Kind: "delta", Type: "text", Text: e.Text})
		case ReasoningDelta:
			reasoning.WriteString(e.Text)
			bus.emit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", Text: e.Text, IsReasoning: true})
		case ToolCall:
			bus.emit(scheduler.SessionEvent{Kind: "tool_part", Type: "tool", Text: e.Name})
			calls = append(calls, e)
		case StreamError:
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: e.Err.Error()})
			return false, nil, Usage{}, false
		case Finish:
			// The round's REAL provider usage rides the Finish event. Surfaced to
			// the caller so the Ask path can attribute it to the conversation
			// (never estimated — see askUsageSink).
			return true, calls, e.Usage, false
		}
	}
}

// askUsageSink builds the per-round usage reporter for one Ask turn, or nil
// when no recorder is wired (Ask then records no usage, matching the worker
// path's behaviour under a nil recorder).
//
// The sample is attributed to the CONVERSATION via SessionID rather than to an
// execution: a chat turn has no execution/task/project row (mirroring the
// opencode Ask path, askorchicon.recordTurnUsage). That attribution is also what
// makes the rows prunable — DeleteConversation de-links usage by conversation id
// (db.ClearUsageSessionIDs) instead of deleting it, because usage_records is the
// tenant's real spend ledger and Cost Explorer/Telemetry roll up from it.
//
// Before this, a native Ask turn recorded NOTHING: the native per-turn usage
// sink was wired only on the worker-execution path (bridge.go, emitTurnUsage),
// which is bound to an execution row. That left the Ask path with no measurable
// prompt size at all.
func (b *NativeBridge) askUsageSink(tenantID, conversationID, sessionID, modelRef, provider, model string) func(context.Context, Usage) {
	if b.usageRecorder == nil {
		return nil
	}
	return func(ctx context.Context, u Usage) {
		// A genuinely empty round is dropped (parity with emitTurnUsage): a
		// provider that reported nothing is not a zero-cost sample.
		if u.InputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 &&
			u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.CostUSD == 0 {
			return
		}
		b.recordAskPromptTokens(sessionID, promptOccupancy(u))
		// context.WithoutCancel: this is real usage that has already been paid
		// for, so a turn that ends (or is aborted) mid-record must not lose it.
		_ = b.usageRecorder(context.WithoutCancel(ctx), scheduler.UsageRecord{
			TenantID:         tenantID,
			Provider:         provider,
			Model:            model,
			PromptTokens:     u.InputTokens,
			CacheReadTokens:  u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
			CompletionTokens: u.OutputTokens,
			ReasoningTokens:  u.ReasoningTokens,
			CostUSD:          u.CostUSD,
			AdapterKind:      adapter.AdapterKind(modelRef),
			SessionID:        conversationID,
		})
	}
}

// executeToolCalls runs one round's tool calls against the injected Ask
// tools and appends one tool-result message per call to working. Calls are
// never dropped: with no tool provider injected the result records the
// misconfiguration as an error so the model can explain instead of
// hanging. A tool execution failure is recorded as an error tool result (the
// model sees it and can recover), never as a turn failure.
//
// conversationID is the ledger owner id for every row the Ask file-edit hook
// writes: the server builds that hook with db.FileEditOwnerAskConversation, so
// a completed mutating call ledgers (ask_conversation, <conversationID>) — the
// exact tuple both clients' Ask diff panes query. This is what puts LIVE
// per-edit rows in the pane during the turn; without it the pane only fills
// after the turn from the git reconciler.
func (b *NativeBridge) executeToolCalls(ctx context.Context, bus *chatBus, working *[]Message, calls []ToolCall, conversationID string) {
	b.mu.Lock()
	tools := b.askTools
	b.mu.Unlock()
	// seen guards against a provider/decoder echo issuing the same call
	// twice in one round: a second result for one call_id makes the wire
	// reject the turn as a duplicate function_call_output. First
	// occurrence wins.
	seen := map[string]bool{}
	for _, c := range calls {
		if c.ToolCallID != "" {
			if seen[c.ToolCallID] {
				continue
			}
			seen[c.ToolCallID] = true
		}
		args := c.ArgsJSON
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		var out string
		var toolErr error
		if tools == nil {
			toolErr = errors.New("orchicon bridge: no Ask tool provider injected — cannot execute tool " + c.Name)
		} else if isAskUserTool(c.Name) {
			// THE PAUSE. ask_user no longer records-and-returns: it BLOCKS here, the
			// collector raises the question as a card, and the operator's ANSWER
			// becomes this call's result. So the model resumes the turn holding what
			// the operator actually said, instead of being told the question was
			// "recorded" and ending the turn.
			//
			// That is the whole difference the operator asked for: "no other
			// chatting should be going on if a question is asked. You should pause
			// to resume until the user has answered."
			ans := b.awaitUserAnswer(ctx, bus, c, args)
			switch {
			case ans == "":
				toolErr = errors.New("ask_user was not answered — the question expired unanswered, so it did not run. Ask it again if it is still needed")
			case strings.HasPrefix(ans, ConsentRefusedPrefix):
				// A LAYER REFUSAL, NOT AN ANSWER — the question half of consentDenialError.
				//
				// WITHOUT THIS the refusal arrived as `out`, i.e. as a SUCCESSFUL tool result carrying an
				// internal error sentence, which told the model the operator had said that sentence and told
				// every client the operator had ANSWERED. Nothing was shown and nobody was asked: the reason
				// (a malformed call, refused by the tool's own validator before a card existed) is the only thing
				// the model needs, and it has to arrive as an ERROR or the model treats it as content.
				reason := strings.TrimPrefix(ans, ConsentRefusedPrefix)
				toolErr = fmt.Errorf("%s — this question was NOT asked and the operator did NOT answer it, so this result is not their words. Correct the call (the reason above says what was wrong) and ask again", reason)
			default:
				out = ans
			}
		} else if consentGatedTool(c.Name) {
			// ASK BEFORE ACTING. The collector owns the decision — the
			// precedence chain, the deny list, the session grants — so this only
			// raises the ask and waits, exactly as the opencode adapter's serve
			// blocks for the same collector. A "once" proceeds; anything else
			// (a denial, a refusal, or silence past the wait) means the call does
			// NOT run and the model is told why.
			if d := b.awaitConsentPermission(ctx, bus, c, args); d != "once" {
				toolErr = consentDenialError(c.Name, d)
			} else {
				out, toolErr = tools.ExecuteAskTool(ctx, c.Name, args)
			}
		} else {
			out, toolErr = tools.ExecuteAskTool(ctx, c.Name, args)
		}
		content := out
		isErr := false
		if toolErr != nil {
			content = toolErr.Error()
			isErr = true
		}
		*working = append(*working, Message{Role: RoleTool, Content: []Content{{
			ToolResult: &ContentToolResult{ToolCallID: c.ToolCallID, Content: content, IsError: isErr},
		}}})
		// Diff-pipeline ledger hook (native Ask file-edit gap): a COMPLETED
		// mutating call ledgers its ground-truth engine payload under the Ask
		// conversation's owner tuple, so the live per-edit rows are queryable
		// DURING the turn (both clients already query this exact tuple).
		// Failed calls carry no ground truth and never ledger (parity with the
		// execution funnel in loop.go, which fires only after a nil error).
		// The hook owns its own error posture (best-effort); nil = no Ask
		// ledger. Read under mu because the server may wire it concurrently.
		if toolErr == nil {
			b.mu.Lock()
			hook := b.askFileEditHook
			b.mu.Unlock()
			if hook != nil {
				var inputMap map[string]any
				if err := json.Unmarshal([]byte(args), &inputMap); err != nil || inputMap == nil {
					inputMap = map[string]any{}
				}
				// Owner id = the conversation id; execDir is "" because the
				// engine payload is exact and needs no plane-side observer
				// (which also makes the non-git / no-project-dir case work).
				hook(ctx, conversationID, tenant.FromContext(ctx), "", c.Name, inputMap, out)
			}
		}
		// Emit the RESOLUTION as an adapter-neutral typed event. Without this the
		// bus carried only the start (tool_part, name alone), so a consumer could
		// never learn the call's arguments or its outcome: the Ask tool ledger
		// kept the "{}" placeholder forever and every call read back as
		// "aborted", which is why an ask_user card had no question or options to
		// draw. Both facts are in hand exactly here.
		bus.emit(scheduler.SessionEvent{
			Kind:       "tool_result",
			Type:       "tool",
			ToolCallID: c.ToolCallID,
			ToolName:   c.Name,
			ArgsJSON:   args,
			Output:     content,
			IsError:    isErr,
		})
	}
}

// appendAssistantText appends one assistant text message to working when
// text is non-blank.
func (b *NativeBridge) appendAssistantText(working *[]Message, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	t := text
	*working = append(*working, Message{Role: RoleAssistant, Content: []Content{{Text: &t}}})
}

// appendAssistantTurn appends one assistant message carrying the round's
// text (when non-blank) plus its tool uses, then clears the consumed text
// from reply via the caller's Reset.
func (b *NativeBridge) appendAssistantTurn(working *[]Message, text string, calls []ToolCall) {
	var content []Content
	if strings.TrimSpace(text) != "" {
		t := text
		content = append(content, Content{Text: &t})
	}
	seenUse := map[string]bool{}
	for _, c := range calls {
		if c.ToolCallID != "" {
			if seenUse[c.ToolCallID] {
				continue
			}
			seenUse[c.ToolCallID] = true
		}
		args := c.ArgsJSON
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		content = append(content, Content{ToolUse: &ContentToolUse{ToolCallID: c.ToolCallID, Name: c.Name, ArgsJSON: args}})
	}
	if len(content) == 0 {
		return
	}
	*working = append(*working, Message{Role: RoleAssistant, Content: content})
}

// danglingToolResultOutput is the explicit tool result attached in place of a
// result that never arrived (the turn ended abnormally between the assistant's
// tool call and its result).
const danglingToolResultOutput = "tool call aborted — the turn ended before this tool returned a result"

// hasDanglingToolCalls reports whether a provider-bound history contains an
// assistant message with a tool use that no tool-role message answers. Such a
// history is exactly what providers reject with:
//
//	No tool output found for function call <id>.
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following
//	tool_calls message)
func hasDanglingToolCalls(messages []Message) bool {
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role != RoleTool {
			continue
		}
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
				answered[c.ToolResult.ToolCallID] = true
			}
		}
	}
	for _, m := range messages {
		if m.Role != RoleAssistant {
			continue
		}
		for _, c := range m.Content {
			if c.ToolUse != nil && !answered[c.ToolUse.ToolCallID] {
				return true
			}
		}
	}
	return false
}

// sanitizeChatHistory returns a copy of the provider-bound history in which
// every tool call and every tool result is PAIRED — in BOTH directions. This is
// the REPLAY-BOUNDARY invariant for the native Ask transport, whose history is
// re-sent in full on every turn (D2): whatever the session accumulated (an
// interrupted turn, a tool that never returned, a switch to a different model,
// a compaction that cut a tool round in half), what leaves for the provider is
// always well-formed.
//
// Forward (assistant → tool): a call whose result is missing gets an explicit
// aborted result (so the model still learns the call happened and did not
// return), and a call with no id — which can never be matched to a result — is
// dropped, along with the assistant message when nothing else in it remains.
//
// Backward (tool → assistant): a tool result whose call was never DECLARED by a
// preceding assistant message is dropped, along with the tool message when
// nothing else in it remains. That is the shape which wedges a conversation
// outright, because no repair can invent a call for it, and providers reject
// the whole request:
//
//	Messages with role 'tool' must be a response to a preceding message with
//	'tool_calls'
//
// Observed live after a compaction kept the last askCompactTailMessages messages
// verbatim and the cut fell inside a tool round: the kept tail began on a tool
// result whose assistant tool use had been collapsed away, so every subsequent
// send 400'd. Dropping the orphan here heals such a session on its NEXT turn,
// because dispatchTurnMessage persists the repaired history back onto it.
func sanitizeChatHistory(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role != RoleTool {
			continue
		}
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
				answered[c.ToolResult.ToolCallID] = true
			}
		}
	}

	out := make([]Message, 0, len(messages))
	// declared is the set of call ids an assistant message has ALREADY declared
	// at this point in the walk. A tool result may only answer a call that
	// PRECEDES it — the backward half of the invariant.
	declared := map[string]bool{}
	for _, m := range messages {
		if m.Role == RoleTool {
			kept := make([]Content, 0, len(m.Content))
			for _, c := range m.Content {
				// A tool-role message carries tool results. A stray non-result
				// part has no valid provider shape here, and a result whose call no
				// preceding assistant message declares is the ORPHAN shape
				// providers reject ("Messages with role 'tool' must be a response
				// to a preceding message with 'tool_calls'"). Both are dropped.
				if c.ToolResult == nil || c.ToolResult.ToolCallID == "" || !declared[c.ToolResult.ToolCallID] {
					continue
				}
				kept = append(kept, c)
			}
			if len(kept) == 0 {
				// Every result in it is orphaned: the message itself goes away.
				continue
			}
			out = append(out, Message{Role: RoleTool, Content: kept})
			continue
		}
		if m.Role != RoleAssistant {
			out = append(out, m)
			continue
		}
		var content []Content
		var missing []string
		seen := map[string]bool{}
		for _, c := range m.Content {
			if c.ToolUse == nil {
				content = append(content, c)
				continue
			}
			id := c.ToolUse.ToolCallID
			if id == "" {
				// Unaddressable call: replaying it is the bare tool_calls shape
				// the provider rejects, and no result could ever match it.
				continue
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			declared[id] = true
			content = append(content, c)
			if !answered[id] {
				missing = append(missing, id)
			}
		}
		if len(content) == 0 {
			// Only unaddressable tool uses: the message itself goes away.
			continue
		}
		out = append(out, Message{Role: RoleAssistant, Content: content})
		for _, id := range missing {
			out = append(out, Message{Role: RoleTool, Content: []Content{{
				ToolResult: &ContentToolResult{ToolCallID: id, Content: danglingToolResultOutput, IsError: true},
			}}})
		}
	}
	return out
}

// --- consent: the native half of the permission card ---

// nativeConsentWaitDefault bounds how long an Ask turn waits for a permission
// decision before treating silence as a DENIAL.
//
// DENY, NOT PROCEED: the operator chose fail-closed, and an unanswered ask must
// never become an approval.
//
// FIFTEEN MINUTES, and the number is a lesson rather than a guess. It was ten, and
// this adapter's first version cut it to TWO on the reasoning that "a missed card
// should cost little". That was wrong in a way that made the feature unusable: a
// human has to NOTICE the card, click into it, arrow to a row and press Enter, and
// every one of those steps is slower than two minutes — so accepts arrived after
// the wait had already given up and were silently dropped, while the server's own
// registry still held the ask and answered the click `applied: true`. The operator
// saw "accepted" and the call stayed denied.
//
// The cliff is a workaround for "nobody is watching", and the PAUSE (ask_user
// blocking, so the card is the end of the turn rather than a side channel) is the
// real answer: a turn that waits for a human is waiting legitimately. Until then
// this stays generous, because a long wait costs nothing while the turn is open
// and a short one costs the whole feature.
const nativeConsentWaitDefault = 15 * time.Minute

// nativeConsentWait resolves the wait, with an env override for testing and for
// an operator who wants a shorter leash.
func nativeConsentWait() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_CONSENT_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return nativeConsentWaitDefault
}

// permWait is one in-flight consent wait: the channel the decision arrives on.
// Buffered(1) so the replier never blocks on a waiter that has already given up.
type permWait struct{ decision chan string }

// nextPermID mints an ask id. Unique per bridge, which is all the correlation
// needs: the collector keys its registry by conversation and ask id.
func (b *NativeBridge) nextPermID() string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	b.permSeq++
	return fmt.Sprintf("native-ask-%d", b.permSeq)
}

// ConsentReadOnlyTools are the host-suite tools that NEVER ASK.
//
// The name is about consequence, not about reading: a read cannot change anything,
// and todowrite writes SESSION STATE rather than the filesystem. Both cannot touch
// anything a policy protects, so a card for either is a card for nothing in
// particular — and a gate that asks for nothing in particular is one people learn to
// click through without reading.
//
// EXPORTED, AND PAIRED WITH ConsentMutatingTools, so the split can be checked
// against askorchicon's hostSuiteToolNames from a test. A new host-suite tool must
// be classified HERE, deliberately, or that test fails — which is what makes this
// fail-closed across a package boundary instead of relying on whoever adds the
// tool remembering.
var ConsentReadOnlyTools = []string{
	"read", "batch_read", "grep", "batch_grep", "list", "glob", "todoread", "todowrite",
}

// ConsentMutatingTools are the host-suite tools that DO ask before they run.
//
// todowrite is NOT here, and that is a correction rather than an omission: it was
// classified mutating "for completeness", and the first time the policy broke, every
// todo update asked for approval. It writes SESSION STATE, not the filesystem — it
// cannot touch anything a policy protects — so gating it adds a card for an action
// that changes nothing outside the turn. Reads are free for the same reason.
var ConsentMutatingTools = []string{
	"write", "edit", "batch_write", "bash",
}

// consentGatedTool reports whether a native Ask tool call must be APPROVED before
// it runs.
//
// FAIL-CLOSED FOR THE HOST SUITE. It used to name only write/edit/batch_write/bash
// — a denylist, so any mutating host tool added later would have run with NO
// consent at all, silently. It is now derived from the classification above,
// inverted: a host-suite tool asks unless it is one of the known read-only ones.
//
// PRODUCT tools (ask_user, list_projects, …) are NOT gated: they work on Orchicon's
// own data rather than the filesystem, and gating ask_user would mean asking
// permission to ask a question. The classification above covers the host suite,
// and the cross-package test keeps that boundary honest as the suite grows.
func consentGatedTool(name string) bool {
	// AN OPAQUE MCP TOOL IS CONSENT-GATED IN EVERY MODE, Iteration included. It is a third-party
	// tool the platform cannot classify: the name reveals nothing about whether it acts, so the
	// operator approves each call. This is the SECOND half of the mode policy (see internal/askmode):
	// the mode table decides WHETHER a mode may offer/execute an opaque MCP tool at all, and consent
	// gates it even where the mode may. The platform's OWN `mcp__orchicon__*` tools are exempt — the
	// mode table governs them, and they are the platform's own data surface.
	if askmode.IsOpaqueMCPTool(name) {
		return true
	}
	for _, n := range ConsentReadOnlyTools {
		if n == name {
			return false
		}
	}
	for _, n := range ConsentMutatingTools {
		if n == name {
			return true
		}
	}
	// Neither list: a product tool. It does not touch the filesystem, so the
	// file/shell gate does not apply.
	return false
}

// awaitConsentPermission raises a permission ask for one tool call and blocks
// until the collector decides it.
//
// THE SHAPE MIRRORS THE opencode ADAPTER, deliberately. There the serve blocks
// and our collector decides; here the ADAPTER blocks and the SAME collector
// decides, over the same SessionEvent vocabulary and the same
// ReplyPermissionDecision reply. So the consent core — the precedence chain, the
// card, the session grants, the deny list, the never-allow class, the expiry — is
// shared rather than reimplemented, and neither adapter is the reference dialect.
//
// It makes NO policy decision here: it carries the action (tool + argument JSON,
// from which the collector derives the target and the command the same way it
// does for an MCP-style ask) and waits. "once" means proceed; anything else means
// the call does not run, and the value says WHICH of the three situations it was —
// see the outcome constants.
//
// A TIMEOUT IS NOT A REFUSAL, AND NOTHING IS PERMANENT. It writes no grant, no deny
// entry and no once-target; the only sticky state in this system is the operator's
// policy file and an explicit ALLOW_SESSION grant. So a retried call is a NEW call,
// which asks again and can be approved — the denial is per-call, by construction.
func (b *NativeBridge) awaitConsentPermission(ctx context.Context, bus *chatBus, c ToolCall, args string) string {
	askID := b.nextPermID()
	w := &permWait{decision: make(chan string, 1)}

	b.permMu.Lock()
	if b.permWaits == nil {
		b.permWaits = map[string]*permWait{}
	}
	b.permWaits[askID] = w
	b.permMu.Unlock()
	defer func() {
		b.permMu.Lock()
		delete(b.permWaits, askID)
		b.permMu.Unlock()
	}()

	if bus != nil {
		bus.emit(scheduler.SessionEvent{
			Kind:         "permission",
			PermissionID: askID,
			Tool:         c.Name,
			InputJSON:    args,
		})
	}

	timer := time.NewTimer(nativeConsentWait())
	defer timer.Stop()
	select {
	case d := <-w.decision:
		return d
	case <-timer.C:
		// Silence is a DENIAL for this call (fail closed) — never an approval.
		//
		// "expired" rather than "reject" so the caller can say something USEFUL: the
		// operator did not refuse, nobody answered, and the same call WILL be asked
		// again if it is retried (a retry is a new call, hence a new ask and a new
		// card — a timeout writes no permanent state). The two used to be
		// indistinguishable, so the model was told "not approved" either way and had
		// no reason to ask again.
		return consentExpired
	case <-ctx.Done():
		// The turn was cancelled (Stop / supersede / TTL): give up the wait
		// immediately rather than holding the goroutine for the full window.
		return consentCancelled
	}
}

// isAskUserTool reports whether a tool is the clarifying-question tool. The name
// is bare on the native transport (see askorchicon.hostSuiteToolNames' sibling
// comment: the product registry is keyed by bare name), and prefixed on the MCP
// side — so both spellings match.
func isAskUserTool(name string) bool {
	return name == "ask_user" || name == "orchicon_ask_user"
}

// awaitUserAnswer raises a clarifying question and blocks until the operator
// answers it. The ANSWER is returned, and the caller returns it as the tool result.
//
// SAME MECHANICS AS awaitConsentPermission, deliberately: the bus carries the
// question to the collector, the collector's registry holds it, the reply RPC
// delivers the answer, and ReplyPermissionDecision wakes this wait. The two differ
// only in what flows back — a permission decision string, or the operator's words.
//
// An empty return means NO ANSWER (the window expired, or the turn was cancelled);
// the caller turns that into a tool error so the model is not left believing a
// question was answered when it was not.
func (b *NativeBridge) awaitUserAnswer(ctx context.Context, bus *chatBus, c ToolCall, args string) string {
	askID := b.nextPermID()
	w := &permWait{decision: make(chan string, 1)}

	b.permMu.Lock()
	if b.permWaits == nil {
		b.permWaits = map[string]*permWait{}
	}
	b.permWaits[askID] = w
	b.permMu.Unlock()
	defer func() {
		b.permMu.Lock()
		delete(b.permWaits, askID)
		b.permMu.Unlock()
	}()

	if bus != nil {
		// The ARGUMENTS ride along: the collector parses them with the SAME
		// validator the tool uses, so a malformed question is refused with the
		// tool's own error rather than parked as an unanswerable card.
		bus.emit(scheduler.SessionEvent{
			Kind:         "question",
			PermissionID: askID,
			Tool:         c.Name,
			InputJSON:    args,
		})
	}

	timer := time.NewTimer(nativeConsentWait())
	defer timer.Stop()
	select {
	case ans := <-w.decision:
		return ans
	case <-timer.C:
		return ""
	case <-ctx.Done():
		return ""
	}
}

// The outcomes awaitConsentPermission reports. "once" proceeds; every other value
// means the call did NOT run, and they are distinct because the OPERATOR's
// situation differs and the model is told about it.
const (
	// consentExpired — the wait ran out with no answer. Not a refusal: the same
	// call will be asked again if it is retried.
	consentExpired = "expired"
	// consentCancelled — the turn ended while the ask was outstanding.
	consentCancelled = "cancelled"
	// ConsentRefusedPrefix marks a decision that REFUSED the call and carried the REASON
	// with it — a policy DENY, the never-allow binary class, or a policy that could not be
	// read. It is a distinct shape because it is a distinct situation: the OPERATOR never
	// saw this call, so reporting it as their refusal is a false statement about them —
	// and the exact one this repo has been corrected on before (the malformed-policy
	// incident, where the model was told "the operator denied you" about a config error).
	//
	// The consent layer already computes the reason precisely (it names the deny entry,
	// or the class that refuses); before this, the collector sent a bare "reject" to the
	// bridge and LOGGED the reason, so the one party who needed it — the model, deciding
	// whether to try again — was the one party who never got it.
	//
	// IT MATTERS MOST UNDER FULLSEND: with the permission PROMPT waived, a policy denial
	// and the never-allow class are the ONLY refusals left, so every refusal a fullsend
	// turn meets would otherwise be attributed to an operator who was never asked.
	ConsentRefusedPrefix = "refused: "
)

// consentDenialError words what happened to a call that was not approved, in the
// terms the MODEL needs to decide whether to try again. The medium is a tool error,
// so it is read by the model rather than the operator — and "not approved" for
// every case (an explicit refusal, an unanswered ask, a cancelled turn) is what
// stops a model retrying when retrying is exactly what the operator wants.
func consentDenialError(tool, decision string) error {
	// A LAYER REFUSAL, not an operator decision — and the distinction is the whole point
	// of spelling it out. The reason names the rule that refused the call, so the model can
	// work around it rather than asking the operator to lift a denial they never made.
	if reason, ok := strings.CutPrefix(decision, ConsentRefusedPrefix); ok {
		return fmt.Errorf("%s — this call did not run. The OPERATOR did not refuse it and was never asked: an Orchicon permission rule refused it. Do not retry it unchanged; choose a different approach, or ask the operator to change that rule if you believe it is wrong", reason)
	}
	switch decision {
	case consentExpired:
		return fmt.Errorf("approval for %s expired unanswered — nothing was approved, so this call did not run. Nothing is permanently denied: retry the call if it is still needed, and it will ask again", tool)
	case consentCancelled:
		return fmt.Errorf("the turn was cancelled while awaiting approval for %s — this call did not run", tool)
	default:
		return fmt.Errorf("the operator denied %s — this call did not run. Do not retry it; ask them what they would prefer instead", tool)
	}
}

// commitChatHistory APPENDS the turn's own messages to the session's in-memory history (user message,
// assistant texts, tool uses and tool results), so a follow-up re-sends the complete context.
// Best-effort: when the turn produced no new messages the stored history is left untouched; when the stored
// entry was reset mid-turn (session recreated) the turn's snapshot seeds it.
//
// IT APPENDS, AND IT MUST NOT REPLACE. It used to assign `cur = working`, i.e. overwrite the session with
// THIS turn's snapshot-plus-output — and that silently DESTROYED the work of any other turn that had written
// to the session since this one dispatched. Supersede is exactly that shape, and supersede is how this
// operator steers a running turn:
//
//	step                                        session history
//	A dispatched, running                       H + [userA]
//	operator interjects ⇒ B dispatched          H + [userA] + [userB]      (B's snapshot)
//	A cancelled ⇒ A commits its own working     H + [userA] + replyA        ← [userB] destroyed
//	B completes ⇒ B commits its own working     H + [userA] + [userB] + replyB   ← replyA destroyed
//
// Last writer wins and the loser's messages are gone. The operator's own conversation shows the
// fingerprint: their four superseded turns left FIVE CONSECUTIVE user messages in the session history with
// no reply between them, because a user message rides the NEXT turn's snapshot (so it survives) while a
// reply exists only in the turn that produced it (so it does not). Every one of those turns was in the
// transcript, on screen, and in the database — and invisible to the model, which is the whole of their
// "Orchicon is not usable if you can't have a session that remembers what it's doing".
//
// The fix is not a merge heuristic: a turn's contribution is exactly the tail it produced past its own
// snapshot (drainChatTurn seeds `working` from `history` and only ever appends to it), so appending that tail
// to whatever the session holds NOW preserves both turns' messages whatever order they commit in.
func (b *NativeBridge) commitChatHistory(sessionID string, history, working []Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(working) == 0 {
		return
	}
	// THIS TURN'S OWN MESSAGES — everything it produced past the snapshot it dispatched from.
	tail := turnContribution(history, working)
	cur := b.chatHistory[sessionID]
	if len(cur) == 0 && len(history) > 0 {
		// The entry was reset mid-turn; seed from the turn snapshot so the user message is not lost
		// before appending what the turn produced.
		cur = append([]Message(nil), history...)
	}
	cur = append(cur, tail...)
	// Never persist a dangling tool call: a turn that ended between a call and its result must not poison
	// the session for the next provider.
	cur = b.sanitizeHistoryLocked(sessionID, cur)
	b.chatHistory[sessionID] = cur
	b.persistAskHistoryLocked(sessionID)
}

// turnContribution returns the messages a turn ADDED to the snapshot it dispatched from.
//
// `working` is `history` plus what the turn produced (drainChatTurn seeds it that way and only appends), so
// the contribution is the tail. A `working` that is somehow SHORTER than its snapshot is treated as wholly
// new rather than indexed past its end — that can only ever add messages to the session, never drop one.
func turnContribution(history, working []Message) []Message {
	if len(history) == 0 || len(working) < len(history) {
		return working
	}
	return working[len(history):]
}

// AbortConversationSession implements scheduler.ChatTurnClient: it cancels
// the in-flight turn's context so the model stops generating now (D7). Safe
// no-op for unknown sessions.
func (b *NativeBridge) AbortConversationSession(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	cancel, ok := b.chatTurns[sessionID]
	b.mu.Unlock()
	if !ok {
		return nil // safe no-op for unknown/finished sessions
	}
	cancel()
	return nil
}

// ReplyPermission implements scheduler.ChatTurnClient. Native Ask turns are
// text-only (no tools, no permission.asked), so this is never invoked in
// practice; it returns an actionable error rather than silently swallowing an
// approval (D6).
func (b *NativeBridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	// The collector's NO-CONSENT-HANDLE fallback. It is reached only when the
	// turn has no consent core at all (a bare attempt), which is the one case
	// the old auto-approve existed for — so it approves, as its name says, and
	// never silently: the ordinary path answers through ReplyPermissionDecision.
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: it delivers the
// collector's decision to the adapter call that is BLOCKED on it.
//
// THIS IS THE NATIVE HALF OF THE PERMISSION CARD. It used to be a documented
// no-op, and the reason given was that "a native Ask turn has no permission
// channel". That was true when the native path executed no tools; it executes
// bash and writes constantly now, so the comment had outlived the fact. The
// channel is the wait a call parks on in awaitConsentPermission.
//
// A decision for an ask nobody is waiting on (the wait timed out, or the turn
// already finalised) is NOT an error: it is simply moot, and the caller can do
// nothing about it. Reporting one would poison a turn that is otherwise fine.
func (b *NativeBridge) ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error {
	b.permMu.Lock()
	w := b.permWaits[permissionID]
	b.permMu.Unlock()
	if w == nil {
		return nil
	}
	select {
	case w.decision <- decision:
	default:
		// Buffered(1) and single-writer: a second decision for the same ask is
		// dropped rather than blocking this caller on a full channel.
	}
	return nil
}
