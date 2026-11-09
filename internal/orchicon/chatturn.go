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
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge implements the Ask chat-session capability, including
// the attachment-aware sender (parity with the opencode adapter — attachments ride the turn inline.
var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*NativeBridge)(nil)
var _ scheduler.ConversationHistoryPurger = (*NativeBridge)(nil)

// Attachment caps mirror the server-side turn validation (startConversationTurnOpts): the bridge
// enforces them too so direct interface callers get the same bounds.
const (
	askMaxAttachments           = 5
	askMaxAttachmentBytes       = 10 * 1024 * 1024
	askMaxAttachmentsTotalBytes = 20 * 1024 * 1024
)

// AskToolProvider supplies the Ask-time tool surface for native turns.
type AskToolProvider interface {
	// AskToolDefs returns the tool definitions offered to the model. It takes the TURN's context because the
	// mode boundary is part of the surface: a mode that may not do the work is not offered the tools that would.
	AskToolDefs(ctx context.Context) []ToolDef
	// ExecuteAskTool runs one tool call and returns its result text.
	ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error)
}

// chatBus is the per-turn SessionBus the native bridge feeds from its drain goroutine.
type chatBus struct {
	events chan scheduler.SessionEvent
	done   chan struct{}
	once   sync.Once

	// mu makes Close race-free against emit.
	mu sync.RWMutex

	// users counts the turns currently using this bus.
	users int
}

// chatBusCapacity is the per-turn event buffer.
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

// release gives up one turn's use of this bus, closing it only when no user remains.
func (b *chatBus) release() {
	b.mu.Lock()
	b.users--
	last := b.users <= 0
	b.mu.Unlock()
	if last {
		b.Close()
	}
}

// emit pushes one event onto the bus.
func (b *chatBus) emit(evt scheduler.SessionEvent) {
	// A closed bus means this turn was superseded: its events have no consumer, and sending on the
	// closed channel would panic the whole process.
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

// terminalEventKind reports whether losing this event would leave the collector unable to finish
// the turn.
func terminalEventKind(kind string) bool {
	return kind == "idle" || kind == "error"
}

// SetAskTools injects the Ask-time tool surface for native turns (the askorchicon product tools).
func (b *NativeBridge) SetAskTools(p AskToolProvider) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askTools = p
}

// SetAskHistoryDir enables disk persistence for Ask session histories (one JSON file per session
// under dir).
func (b *NativeBridge) SetAskHistoryDir(dir string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askHistoryDir = dir
}

// SetAskCompactNotice wires the durable record for an Ask compaction.
func (b *NativeBridge) SetAskCompactNotice(fn scheduler.AskCompactNoticeFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askCompactNotice = fn
}

// reportAskCompaction hands a completed compaction to the wired notice sink.
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
	// WithoutCancel: the notice describes work ALREADY DONE, so a turn that ends or is aborted
	// mid-record must not lose it — the same reasoning, and the same technique, as askUsageSink.
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

// askHistoryMaxBytes caps one persisted session file (image attachments are base64 data URLs — a
// long image-heavy conversation could otherwise grow the file without bound).
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

// sanitizeHistoryLocked runs the replay-repair sanitize and, when it actually REMOVES something,
// records that as an intended reduction for the persist that follows.
func (b *NativeBridge) sanitizeHistoryLocked(sessionID string, h []Message) []Message {
	out := sanitizeChatHistory(h)
	if len(out) < len(h) {
		b.markHistoryReductionLocked(sessionID,
			fmt.Sprintf("replay repair removed %d unpaired message(s)", len(h)-len(out)))
	}
	return out
}

// markHistoryReductionLocked declares that the NEXT persistence for this session is an INTENTIONAL
// lossy reduction, so the shrink guard reports it as intended rather than as data loss.
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

// messageFingerprint identifies a message's CONTENT, for the "no message may disappear" invariant
// above.
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

// persistAskHistoryLocked writes the session's history to disk (atomic tmp + rename). IT NEVER
// SILENTLY STOPS SAVING, which is what the oversize case used to do.
func (b *NativeBridge) persistAskHistoryLocked(sessionID string) {
	// THE SHRINK GUARD RUNS FIRST, before the persistence decisions below, because it protects the
	// session HISTORY rather than the FILE.
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
// FOUR SHAPES, IN ORDER OF FIDELITY, because the only wrong answer is writing
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
//  4. GZIP WITH TOOL PAYLOADS ELIDED, as the LAST resort. Step 3 is blind to the
//     shape that actually overflowed a live session: a base64 screenshot inside a
//     TOOL RESULT (obs-get-source-screenshot returns one), which is not an Image
//     content part at all — so images_dropped was 0 and step 3 reclaimed nothing.
//     Step 4 is the SAME pass the reactive path runs
//     (ReduceConversationHistoryForContext), reused rather than reimplemented,
//     so the two paths cannot disagree about what "too big" means.
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
		// STEP 4: ELIDE THE TOOL PAYLOADS TOO.
		elided, st := ReduceConversationHistoryForContext(reduced, 0)
		raw, err = marshalAskHistory(elided)
		if err != nil {
			b.log.Warn("orchicon: ask history marshal failed after eliding tool payloads", "session", sessionID, "error", err)
			return nil
		}
		if gz, err = gzipAskHistory(raw); err != nil {
			b.log.Warn("orchicon: ask history compress failed after eliding tool payloads", "session", sessionID, "error", err)
			return nil
		}
		if len(gz) > askHistoryMaxBytes {
			// AN ERROR, NOT A WARNING, and the consequence is spelled out: the file on disk is now STALE by
			// everything committed since the last successful persist.
			b.log.Error("orchicon: ask history is oversize even after dropping images and eliding tool payloads — the FILE ON DISK IS NOW STALE, and a restart will reload it and LOSE every message committed since the last successful persist",
				"session", sessionID, "gz_bytes", len(gz), "cap", askHistoryMaxBytes,
				"images_dropped", images, "tool_results_elided", st.ToolResultsElided,
				"tool_args_truncated", st.ToolArgsTruncated, "text_parts_truncated", st.TextPartsTruncated)
			return nil
		}
		b.log.Warn("orchicon: ask history persisted with tool payloads elided to stay within the persistence cap — every message and every tool pairing is preserved, the payloads are not",
			"session", sessionID, "gz_bytes", len(gz), "images_dropped", images,
			"tool_results_elided", st.ToolResultsElided, "tool_args_truncated", st.ToolArgsTruncated,
			"text_parts_truncated", st.TextPartsTruncated)
		return gz
	}
	b.log.Warn("orchicon: ask history persisted with image payloads dropped to stay within the persistence cap — text and tool context are preserved, the images are not",
		"session", sessionID, "gz_bytes", len(gz), "images_dropped", images)
	return gz
}

// askHistoryImageDroppedMarker replaces an image payload in a persisted history that had to be
// reduced.
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

// trimAskHistoryImages returns a copy of the history with every image data URL replaced by
// askHistoryImageDroppedMarker, plus the number of images replaced.
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

// loadAskHistoryLocked reads a persisted session history from disk (nil on a miss or any failure —
// a new conversation looks exactly like a lost file, and both correctly start empty).
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
func (b *NativeBridge) ToolsRunInProcess() bool { return true }

// CreateConversationSession implements scheduler.ChatTurnClient.
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

// PurgeConversationHistory implements scheduler.ConversationHistoryPurger: it discards a deleted
// conversation's durable Ask history — the in-memory map entry AND the persisted JSON file.
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
	// The pressure bookkeeping describes a conversation that no longer exists: drop it with the history
	// so a deleted conversation leaves no memory behind (nor a stale measurement that could fire.
	delete(b.askPromptTokens, sid)
	delete(b.askWindowTokens, sid)
	delete(b.askPricing, sid)
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

// Subscribe implements scheduler.ChatTurnClient: it returns a fresh buffered SessionBus for the
// conversation's next turn and registers it under the conversation's session so SendTurnMessage
// feeds.
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

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers TRUE, because the native
// bridge now HAS a prompt queue of its own (askInjections, bridge.go).
func (b *NativeBridge) QueuesPrompts() bool { return true }

// enqueueInjection adds a mid-turn message to the session's prompt queue, reporting whether it did.
func (b *NativeBridge) enqueueInjection(sessionID, text string, attachments []scheduler.ChatAttachment) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.chatTurns[sessionID] == nil {
		return false, nil
	}
	content, err := askUserContent(text, attachments)
	if err != nil {
		// It IS an injection — a turn is live — so the caller must report the unusable content rather than
		// quietly starting a second turn.
		return true, err
	}
	if b.askInjections == nil {
		b.askInjections = map[string][]Message{}
	}
	b.askInjections[sessionID] = append(b.askInjections[sessionID], Message{Role: RoleUser, Content: content})
	return true, nil
}

// drainInjections moves every queued injection into the turn's replayable history and reports
// whether there was one.
func (b *NativeBridge) drainInjections(sessionID string, working *[]Message) bool {
	b.mu.Lock()
	pending := b.askInjections[sessionID]
	if len(pending) == 0 {
		b.mu.Unlock()
		return false
	}
	delete(b.askInjections, sessionID)
	b.mu.Unlock()
	*working = append(*working, pending...)
	return true
}

// takeOrphanInjections removes any injection still queued for a session whose turn has ENDED, so
// the caller can put it in context. THIS CLOSES THE ONE RACE THE DRAIN POINTS CANNOT.
func (b *NativeBridge) takeOrphanInjections(sessionID string) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := b.askInjections[sessionID]
	if len(pending) == 0 {
		return nil
	}
	delete(b.askInjections, sessionID)
	return pending
}

// SendTurnMessage implements scheduler.ChatTurnClient.
func (b *NativeBridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, nil)
}

// SendTurnMessageWithAttachments implements scheduler.SendTurnMessageWithAttachments (parity with
// the opencode adapter).
func (b *NativeBridge) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, attachments)
}

// askUserContent builds the user message content for a turn: the text plus one content element per
// attachment (image → Image data URL, UTF-8 text → fenced Text).
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
	// A MESSAGE THAT ARRIVES WHILE A TURN IS LIVE JOINS THAT TURN — IT DOES NOT START A SECOND ONE.
	if joined, jerr := b.enqueueInjection(sessionID, text, attachments); joined {
		if jerr != nil {
			return jerr
		}
		b.log.Info("orchicon: interjection joined the running turn",
			"session", sessionID, "conversation", conversationID)
		return nil
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

	// PROACTIVE CONTEXT GATE (askpressure.go): when the conversation's measured prompt size has crossed
	// the configured fraction of this model's live context window, compact BEFORE dispatching.
	b.maybeCompactForPressure(ctx, prov, conversationID, sessionID, modelRef, model)

	// Append the user message to the session's replayable history (committed under the lock so a
	// concurrent turn never double-appends).
	userContent, err := askUserContent(text, attachments)
	if err != nil {
		return err
	}
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sessionID]...)
	if len(history) == 0 {
		// Memory has nothing (fresh session or a server restart wiped it) — reseed from the persisted file
		// when present so the turn re-sends the full context instead of starting over.
		history = append([]Message(nil), b.loadAskHistoryLocked(sessionID)...)
	}
	// REPLAY BOUNDARY: regardless of where the history came from (this process, the persisted file, an
	// interrupted prior turn), what is sent to the provider is well-formed.
	history = b.sanitizeHistoryLocked(sessionID, history)
	history = append(history, Message{Role: RoleUser, Content: userContent})
	b.chatHistory[sessionID] = history
	b.persistAskHistoryLocked(sessionID)
	b.mu.Unlock()

	// Build the turn request: the accumulated history re-sent as full context (the sessionless
	// emulation), the system prompt as a cacheable block.
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
	// The turn context is derived from the request ctx so AbortConversationSession can cancel it
	// mid-flight (D7) — including the provider HTTP calls on every tool round.
	turnCtx, cancel := context.WithCancel(ctx)
	// Start the stream SYNCHRONOUSLY so a pre-stream failure surfaces as a send-accept failure (the
	// collector fails the turn) rather than a dropped first delta (D4).
	stream, err := b.startTurnWithContextRecovery(turnCtx, prov, &req, sessionID)
	if err != nil {
		cancel()
		return fmt.Errorf("orchicon bridge: start Ask turn: %w", err)
	}
	// A reduction (when one happened) replaced the replayed history.
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
	// This turn ADOPTS the bus, so it is not closed until every adopting turn is done — an interjection
	// shares the bus of the turn it supersedes.
	bus.adopt()
	b.mu.Unlock()

	go b.drainChatTurn(turnCtx, prov, bus, stream, req, conversationID, sessionID, history, b.askUsageSink(prov, tenantID, conversationID, sessionID, modelRef, providerID, model))

	// Return nil (accepted) BEFORE the drain goroutine emits, so the
	// collector observes every event with sent == true (D4).
	return nil
}

// drainChatTurn reads the provider's TurnStream(s) and maps events onto the SessionBus vocabulary
// (D3): TextDelta → delta(text) + accumulate into the part buffer.
func (b *NativeBridge) drainChatTurn(ctx context.Context, prov Provider, bus *chatBus, stream TurnStream, req TurnRequest, conversationID, sessionID string, history []Message, usageSink func(context.Context, Usage)) {
	defer bus.release()
	defer func() {
		// THE TURN IS OVER, so it can no longer drain a note that arrived in the last microseconds.
		orphans := b.takeOrphanInjections(sessionID)
		b.mu.Lock()
		delete(b.chatTurns, sessionID)
		b.mu.Unlock()
		if len(orphans) > 0 {
			// NOT DELIVERED, AND SAID SO.
			b.mu.Lock()
			b.chatHistory[sessionID] = append(b.chatHistory[sessionID], orphans...)
			// Persisted under the SAME lock, through the one choke point every history mutation uses, so a
			// restart replays the note instead of dropping it.
			b.persistAskHistoryLocked(sessionID)
			b.mu.Unlock()
			b.log.Warn("orchicon: an interjection arrived after the turn's last round — carried into the next turn's context",
				"session", sessionID, "messages", len(orphans))
		}
	}()

	// working is the turn's replayable history: it starts as the snapshot committed in SendTurnMessage
	// (prior turns + this user message) and grows with every assistant message and tool result until.
	working := append([]Message(nil), history...)
	// reply accumulates EVERY round's assistant text into one consolidated reply (the final part).
	var reply strings.Builder
	// roundReply holds only the CURRENT round's text (for history replay,
	// which needs one assistant message per round).
	var roundReply strings.Builder
	// reasoning accumulates the WHOLE turn's thinking, for the transcript part (emitTurnParts).
	var reasoning strings.Builder
	// roundReasoning accumulates only the CURRENT round's thinking, because that is what the round's
	// assistant message must carry back to the provider.
	var roundReasoning strings.Builder

	// emitTurnParts publishes what the turn accumulated as COMPLETED parts.
	emitTurnParts := func() {
		if r := strings.TrimSpace(reasoning.String()); r != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "reasoning", Text: r})
		}
		if t := strings.TrimSpace(reply.String()); t != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
		}
	}
	// The collector builds the persisted record ONLY from part events, so these parts are what land in
	// the DB.
	finishTurn := func() {
		emitTurnParts()
		bus.emit(scheduler.SessionEvent{Kind: "idle"})
		b.commitChatHistory(sessionID, history, working)
	}

	// startRound rebuilds the request from `working` and opens the next provider call, reporting false
	// when the turn must end (the failure has already been reported and committed).
	startRound := func() (TurnStream, bool) {
		req.Messages = append([]Message(nil), working...)
		next, err := prov.StreamTurn(ctx, req)
		if err != nil {
			// COMMIT WHAT THE TURN ALREADY PRODUCED BEFORE LEAVING.
			if errors.Is(err, context.Canceled) {
				b.commitChatHistory(sessionID, history, working)
				return nil, false
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			b.commitChatHistory(sessionID, history, working)
			return nil, false
		}
		return next, true
	}

	for round := 0; ; round++ {
		roundDone, calls, usage, aborted := b.drainOneRound(ctx, bus, stream, &roundReply, &reasoning, &roundReasoning)
		// Report the round's REAL usage (never estimated).
		if usageSink != nil {
			usageSink(ctx, usage)
		}
		_ = stream.Close()
		if aborted {
			// Abort (D7): the turn was cancelled — finalize without COMMITTING.
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
		roundThought := roundReasoning.String()
		roundReply.Reset()
		roundReasoning.Reset()
		if roundText != "" {
			if reply.Len() > 0 {
				reply.WriteString("\n\n")
			}
			reply.WriteString(roundText)
		}
		if !roundDone {
			// Stream ended without a Finish event — close out the turn so the collector persists what arrived
			// (pre-existing behavior for a provider that ends without a terminal signal).
			b.appendAssistantText(&working, roundText)
			finishTurn()
			return
		}
		if len(calls) == 0 {
			b.appendAssistantText(&working, roundText)
			// AN INTERJECTION THAT ARRIVED WHILE THIS ROUND WAS GENERATING MUST NOT BE LEFT UNANSWERED.
			if b.drainInjections(sessionID, &working) {
				next, ok := startRound()
				if !ok {
					return
				}
				stream = next
				continue
			}
			finishTurn()
			return
		}
		// Tool round: record the assistant's text (if any) plus the tool uses, execute every call, and
		// continue the turn with the results.
		b.appendAssistantTurn(&working, roundText, calls, roundThought)
		// The tool loop is unbounded by round count: it continues while the model keeps issuing tool calls
		// and terminates naturally when a round returns none (the len(calls) == 0 → finishTurn() path.
		b.executeToolCalls(ctx, bus, &working, calls, conversationID)
		// A NOTE THAT ARRIVED DURING THE TOOL ROUND JOINS THE CONTEXT BEFORE THE NEXT MODEL CALL — the
		// tightest boundary available, because a tool round IS one provider call.
		b.drainInjections(sessionID, &working)
		next, ok := startRound()
		if !ok {
			return
		}
		stream = next
	}
}

// drainOneRound consumes one provider stream until its Finish event (or an error/early end),
// live-emitting deltas onto the bus and accumulating the reply text.
func (b *NativeBridge) drainOneRound(ctx context.Context, bus *chatBus, stream TurnStream, reply, reasoning, roundReasoning *strings.Builder) (roundDone bool, calls []ToolCall, usage Usage, aborted bool) {
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
			roundReasoning.WriteString(e.Text)
			bus.emit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", Text: e.Text, IsReasoning: true})
		case ToolCall:
			bus.emit(scheduler.SessionEvent{Kind: "tool_part", Type: "tool", Text: e.Name})
			calls = append(calls, e)
		case StreamError:
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: e.Err.Error()})
			return false, nil, Usage{}, false
		case Finish:
			// The round's REAL provider usage rides the Finish event.
			return true, calls, e.Usage, false
		}
	}
}

// askUsageSink builds the per-round usage reporter for one Ask turn, or nil when no recorder is
// wired (Ask then records no usage, matching the worker path's behaviour under a nil recorder).
func (b *NativeBridge) askUsageSink(prov Provider, tenantID, conversationID, sessionID, modelRef, provider, model string) func(context.Context, Usage) {
	if b.usageRecorder == nil {
		return nil
	}
	return func(ctx context.Context, u Usage) {
		// PRICE THE ROUND BEFORE DECIDING IT IS EMPTY.
		if p := b.resolveAskPricing(ctx, prov, sessionID, model); p != nil {
			u.CostUSD = p.CostFor(u)
		}
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

// executeToolCalls runs one round's tool calls against the injected Ask tools and appends one
// tool-result message per call to working.
func (b *NativeBridge) executeToolCalls(ctx context.Context, bus *chatBus, working *[]Message, calls []ToolCall, conversationID string) {
	b.mu.Lock()
	tools := b.askTools
	b.mu.Unlock()
	// seen guards against a provider/decoder echo issuing the same call twice in one round: a second
	// result for one call_id makes the wire reject the turn as a duplicate function_call_output.
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
			// THE PAUSE.
			ans := b.awaitUserAnswer(ctx, bus, c, args)
			switch {
			case ans == "":
				toolErr = errors.New("ask_user was not answered — the question expired unanswered, so it did not run. Ask it again if it is still needed")
			case strings.HasPrefix(ans, ConsentRefusedPrefix):
				// A LAYER REFUSAL, NOT AN ANSWER — the question half of consentDenialError.
				reason := strings.TrimPrefix(ans, ConsentRefusedPrefix)
				toolErr = fmt.Errorf("%s — this question was NOT asked and the operator did NOT answer it, so this result is not their words. Correct the call (the reason above says what was wrong) and ask again", reason)
			default:
				out = ans
			}
		} else if consentGatedTool(c.Name) {
			// ASK BEFORE ACTING.
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
		// Diff-pipeline ledger hook (native Ask file-edit gap): a COMPLETED mutating call ledgers its
		// ground-truth engine payload under the Ask conversation's owner tuple.
		if toolErr == nil {
			b.mu.Lock()
			hook := b.askFileEditHook
			b.mu.Unlock()
			if hook != nil {
				var inputMap map[string]any
				if err := json.Unmarshal([]byte(args), &inputMap); err != nil || inputMap == nil {
					inputMap = map[string]any{}
				}
				// Owner id = the conversation id; execDir is "" because the engine payload is exact and needs no
				// plane-side observer (which also makes the non-git / no-project-dir case work).
				hook(ctx, conversationID, tenant.FromContext(ctx), "", c.Name, inputMap, out)
			}
		}
		// Emit the RESOLUTION as an adapter-neutral typed event.
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

// appendAssistantTurn appends one assistant message carrying the round's text (when non-blank) plus
// its tool uses, then clears the consumed text from reply via the caller's Reset.
func (b *NativeBridge) appendAssistantTurn(working *[]Message, text string, calls []ToolCall, reasoning string) {
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
		// A round can be PURE reasoning with no text and no tool call. That is still a message the provider
		// expects the reasoning for, so it is kept rather than dropped.
		if strings.TrimSpace(reasoning) == "" {
			return
		}
		content = append(content, Content{Text: strPtrEmpty()})
	}
	if r := strings.TrimSpace(reasoning); r != "" {
		content[0].Reasoning = &r
	}
	*working = append(*working, Message{Role: RoleAssistant, Content: content})
}

// strPtrEmpty returns a pointer to the empty string, for a message that carries reasoning but no text.
func strPtrEmpty() *string { s := ""; return &s }

// danglingToolResultOutput is the explicit tool result attached in place of a result that never
// arrived (the turn ended abnormally between the assistant's tool call and its result).
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
	// declared is the set of call ids an assistant message has ALREADY declared at this point in the
	// walk.
	declared := map[string]bool{}
	for _, m := range messages {
		if m.Role == RoleTool {
			kept := make([]Content, 0, len(m.Content))
			for _, c := range m.Content {
				// A tool-role message carries tool results.
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

// nativeConsentWaitDefault is the DEFAULT wait for a permission decision, and it is NO BOUND: with
// nothing configured, a card waits for the operator until they answer it or the turn ends.
const nativeConsentWaitDefault = time.Duration(0)

// nativeConsentWait resolves the wait.
func nativeConsentWait() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_ASK_CONSENT_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return nativeConsentWaitDefault
}

// permWait is one in-flight consent wait: the channel the decision arrives on.
// Buffered(1) so the replier never blocks on a waiter that has already given up.
type permWait struct{ decision chan string }

// nextPermID mints an ask id that is UNIQUE ACROSS PROCESS LIVES, not merely unique within one.
//
// "UNIQUE PER BRIDGE" WAS THE BUG, and it was invisible until the evidence was read out of the database.
// The counter was per-process and started at zero, so every plane restart re-minted the SAME ids for
// different asks. Measured on one conversation after eleven restarts in a day:
//
//	native-ask-1 | ask, fullsend_approved, user_PERMISSION_CHOICE_ALLOW_ONCE, user_PERMISSION_CHOICE_ALLOW_SESSION
//	native-ask-2 | ask, fullsend, session_grant, user_PERMISSION_CHOICE_ALLOW_ONCE, …
//	native-ask-3 | ask, fullsend, session_grant, user_PERMISSION_CHOICE_ALLOW_ONCE
//
// One id, incompatible histories. The consequence is a settle/re-arm loop in EVERY client, because the
// transcript ledger is the settle authority and it is keyed by this id:
//
//  1. an old ask id was ANSWERED, so the ledger holds permission.user_…ALLOW_ONCE under that id;
//  2. a restart re-mints the same id for a NEW ask, which discovery reports as open;
//  3. both clients settle the new card from the OLD recorded outcome (settleFromLedger in the GUI,
//     SettledAsks in the TUI), and both then re-arm it from the open-ask list;
//  4. at poll cadence that is settle → re-arm → settle: the flicker.
//
// The operator saw it in the TUI first and then in both, which is exactly the shape of this bug: the TUI
// has always re-armed a settled id, and the GUI only started doing so when applyAskChunk learned to
// re-arm. Before that the GUI silently refused to redraw and showed a dead card instead — the same
// collision, wearing a quieter symptom.
//
// A ULID makes the id unique across restarts, so a new ask can never inherit a previous ask's recorded
// outcome. That is the fix; re-arming and ledger-settling are both correct and both stay.
func (b *NativeBridge) nextPermID() string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	b.permSeq++
	// The prefix is kept for log readability ("native-ask-…" groups the native bridge's asks); the ID
	// itself is globally unique.
	return fmt.Sprintf("native-ask-%s", db.NewID())
}

// ConsentReadOnlyTools are the host-suite tools that NEVER ASK.
var ConsentReadOnlyTools = []string{
	"read", "batch_read", "grep", "batch_grep", "list", "glob", "todoread", "todowrite",
}

// ConsentMutatingTools are the host-suite tools that DO ask before they run.
var ConsentMutatingTools = []string{
	"write", "edit", "batch_write", "bash",
}

// consentGatedTool reports whether a native Ask tool call must be APPROVED before it runs.
func consentGatedTool(name string) bool {
	// AN OPAQUE MCP TOOL IS NOT CONSENT-GATED. This used to gate one in EVERY mode, Iteration
	// included, on the reasoning that a third-party tool's name reveals nothing about whether it
	// acts — so the operator approved each call.
	//
	// THE OPERATOR OVERRULED THAT, and the product case is the stronger argument: "I didn't even
	// think MCP was supposed to have a card. If it's added in scope it should just have access tbh.
	// I don't want cards for that." An MCP server is in scope only because the operator attached it
	// to this conversation or its project, so the approval ALREADY HAPPENED — at configuration time,
	// deliberately. Re-asking per call is asking them to re-decide a standing decision, which is the
	// same noise the policy deny list is exempt from above.
	//
	// WHAT STILL GOVERNS AN OPAQUE MCP TOOL, so this is a narrowing of consent and NOT an absence of
	// governance:
	//
	//   - THE MODE BOUNDARY, which is a SEPARATE layer and is untouched here. askmode.MayExecute sends
	//     an opaque MCP tool to its mode's MayAct, so Brainstorm and Quick Work still refuse it — and
	//     the refusal is enforced at BOTH ends of the native path (native_tools.go filters what is
	//     OFFERED, and refuses again at EXECUTE). That boundary never ran through consent, which is
	//     why removing this arm cannot open it.
	//   - the platform's own `mcp__orchicon__*` tools remain governed by the mode table alone, exactly
	//     as before (they were already exempt here).
	//
	// The observed symptom this also removes: the operator has never seen an MCP card, while write and
	// question cards render fine — and one of this session's own GitHub MCP calls came back
	// "approval for mcp__…__list_pull_requests expired unanswered", i.e. the ask WAS raised and reached
	// no card, so the call failed for a decision nobody was ever offered. Allowing the class removes
	// that failure outright rather than leaving it to a rendering path I have not yet proven.
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

// awaitConsentPermission raises a permission ask for one tool call and blocks until the collector
// decides it.
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

	// A configured bound is the ONLY thing that can expire this wait.
	var expired <-chan time.Time
	if d := nativeConsentWait(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case d := <-w.decision:
		return d
	case <-expired:
		// Silence is a DENIAL for this call (fail closed) — never an approval.
		return consentExpired
	case <-ctx.Done():
		// The turn was cancelled (Stop / supersede / TTL): give up the wait
		// immediately rather than holding the goroutine for the full window.
		return consentCancelled
	}
}

// isAskUserTool reports whether a tool is the clarifying-question tool.
func isAskUserTool(name string) bool {
	return name == "ask_user" || name == "orchicon_ask_user"
}

// awaitUserAnswer raises a clarifying question and blocks until the operator answers it.
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
		// The ARGUMENTS ride along: the collector parses them with the SAME validator the tool uses, so a
		// malformed question is refused with the tool's own error rather than parked as an unanswerable.
		bus.emit(scheduler.SessionEvent{
			Kind:         "question",
			PermissionID: askID,
			Tool:         c.Name,
			InputJSON:    args,
		})
	}

	// Same rule as the permission wait above: only a CONFIGURED bound can time a question
	// out, and with none the arm is never taken — the answer is awaited for as long as it takes.
	var expired <-chan time.Time
	if d := nativeConsentWait(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case ans := <-w.decision:
		return ans
	case <-expired:
		return ""
	case <-ctx.Done():
		return ""
	}
}

// The outcomes awaitConsentPermission reports.
const (
	// consentExpired — the wait ran out with no answer. Not a refusal: the same
	// call will be asked again if it is retried.
	consentExpired = "expired"
	// consentCancelled — the turn ended while the ask was outstanding.
	consentCancelled = "cancelled"
	// IT MATTERS MOST UNDER FULLSEND: with the permission PROMPT waived, a policy denial and the
	// never-allow class are the ONLY refusals left.
	ConsentRefusedPrefix = "refused: "
)

// consentDenialError words what happened to a call that was not approved, in the terms the MODEL
// needs to decide whether to try again.
func consentDenialError(tool, decision string) error {
	// A LAYER REFUSAL, not an operator decision — and the distinction is the whole point of spelling it
	// out.
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
func turnContribution(history, working []Message) []Message {
	if len(history) == 0 || len(working) < len(history) {
		return working
	}
	return working[len(history):]
}

// AbortConversationSession implements scheduler.ChatTurnClient: it cancels the in-flight turn's
// context so the model stops generating now (D7).
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

// ReplyPermission implements scheduler.ChatTurnClient.
func (b *NativeBridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	// The collector's NO-CONSENT-HANDLE fallback.
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: it delivers the collector's decision
// to the adapter call that is BLOCKED on it.
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
