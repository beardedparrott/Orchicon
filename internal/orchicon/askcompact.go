package orchicon

// askcompact.go — context compaction for the NATIVE Ask transport (scheduler.ChatCompactor).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge can compact Ask conversations.
var _ scheduler.ChatCompactor = (*NativeBridge)(nil)

const (
	// askCompactTailMessages is how many of the most recent messages are kept VERBATIM after the
	// summary, so the immediate conversational thread (and any in-flight work) survives the collapse.
	askCompactTailMessages = 6
	// askCompactMinMessages declines compaction on a short conversation: a
	// lossy collapse of a handful of messages costs detail and saves nothing.
	askCompactMinMessages = 12
	// askCompactBytesPerToken is the coarse chars->tokens divisor used ONLY to report a rough size in
	// detail.
	askCompactBytesPerToken = 4
)

// askCompactSummaryInstruction is the summarization prompt.
const askCompactSummaryInstruction = `You are compacting a long-running conversation so it can continue in a smaller context.

Produce a summary that lets the conversation resume with no loss of working state. Cover, in this order:

1. What the user is trying to achieve — the goal, in their terms.
2. What has been decided — decisions, constraints, and preferences the user stated.
3. What has been done — completed work, with concrete identifiers (file paths, branch names, commit ids, entity ids).
4. What is in flight — anything unfinished, and the exact next step.
5. Open questions or blockers — anything unresolved that needs the user.

Rules:
- Be specific and concrete. Preserve identifiers verbatim (paths, ids, names, numbers).
- Do NOT invent anything that is not in the transcript.
- Do NOT add pleasantries or meta-commentary about summarizing.
- Do NOT record which MODE the assistant was in, and do NOT record anything it declined to do because of its mode ("the assistant said it could not edit files"). Modes are applied FRESH to every message from the conversation's current mode setting, so a mode written here goes stale the moment the user switches — and this summary is REPLAYED on every later turn, which would make one old refusal permanent.
- Image content and tool output were dropped BEFORE you saw this transcript. If something important is clearly missing, note it under open questions rather than guessing.

Write the summary now.`

// CompactConversationSession implements scheduler.ChatCompactor for the native (sessionless)
// transport: reduce, summarize, then replace the history this adapter re-sends every turn.
func (b *NativeBridge) CompactConversationSession(ctx context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	sid := opts.SessionID
	if sid == "" && opts.ConversationID != "" {
		sid = scheduler.NativeSessionIDPrefix + opts.ConversationID
	}
	if sid == "" {
		return scheduler.ChatCompaction{}, errors.New("orchicon bridge: compaction requires a conversation or session id")
	}

	// Snapshot the history using the same load path a turn uses, so compaction
	// never operates on a different view than the one that gets re-sent.
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sid]...)
	if len(history) == 0 {
		history = append([]Message(nil), b.loadAskHistoryLocked(sid)...)
	}
	b.mu.Unlock()
	history = sanitizeChatHistory(history)

	if len(history) == 0 {
		return scheduler.ChatCompaction{Detail: "nothing to compact — this conversation has no history yet"}, nil
	}
	if len(history) < askCompactMinMessages {
		return scheduler.ChatCompaction{
			Detail: fmt.Sprintf("nothing to compact — only %d messages so far", len(history)),
		}, nil
	}

	// Stage 1: REDUCE. Deterministic, model-free, cannot fail.
	transcript, bytesBefore, bytesAfter := reduceAskHistory(history)
	if strings.TrimSpace(transcript) == "" {
		return scheduler.ChatCompaction{Detail: "nothing to compact — the history carries no readable text"}, nil
	}

	// The identifier ledger, extracted from the RAW history BEFORE the reduction above — because the
	// reduction is what drops the material the identifiers live in.
	ledger := askHistoryLedger(history)

	// Stage 2: SUMMARIZE on the conversation's own model.
	summary, err := b.summarizeTranscript(ctx, opts, transcript, ledger)
	if err != nil {
		return scheduler.ChatCompaction{}, fmt.Errorf("orchicon bridge: summarize conversation: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		// Leave the history untouched rather than replacing it with nothing.
		return scheduler.ChatCompaction{}, errors.New("orchicon bridge: the model returned an empty summary — history left untouched")
	}

	// The kept tail must be a valid history PREFIX — it can never begin on a
	// bare tool result, because nothing before it declares the call that result
	// answers, and providers reject the whole request:
	//
	//	Messages with role 'tool' must be a response to a preceding message
	//	with 'tool_calls'
	//
	// A blind slice at len-tail lands exactly there whenever the cut falls
	// inside a tool round. Observed LIVE on Ask conversation
	// 01M2C8VXFQY5ZE26PYBSNKA2CA: the last six messages were
	// [tool, assistant, tool, assistant, tool, assistant], so the leading tool
	// result's declaring assistant sat ONE index outside the kept window, the
	// compacted history led with an orphan, and every subsequent send 400'd. It
	// could not self-heal either — the native transport re-sends this history
	// in full on every turn, so the poison was replayed (and re-persisted)
	// forever.
	//
	// Walk the cut back over the leading tool results to the message that
	// declares them, keeping the round whole rather than splitting it — the
	// same "never orphan a use or a result" discipline the worker path's middle
	// eviction already follows (planMiddleEviction, compaction.go).
	cut := len(history) - askCompactTailMessages
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && history[cut].Role == RoleTool {
		cut--
	}
	tail := history[cut:]
	next := make([]Message, 0, len(tail)+1)
	next = append(next, Message{Role: RoleAssistant, Content: []Content{{Text: compactedHistoryMarker(summary, ledger)}}})
	next = append(next, tail...)
	// Composition is itself a replay boundary: the summary is a NEW assistant message and the tail is a
	// slice of the old history.
	next = sanitizeChatHistory(next)

	b.mu.Lock()
	// Archive the pre-collapse history beside the live file: the summary is lossy by design, so this
	// keeps the detail recoverable by hand.
	archivePath := b.archiveAskHistoryLocked(sid)
	b.chatHistory[sid] = next
	b.markHistoryReductionLocked(sid, "conversation compaction")
	b.persistAskHistoryLocked(sid)
	b.mu.Unlock()

	return scheduler.ChatCompaction{
		Compacted: true,
		Detail: fmt.Sprintf(
			"compacted %d messages into 1 summary + %d recent messages (transcript reduced from %s to %s before summarizing)",
			len(history), len(tail), humanBytes(bytesBefore), humanBytes(bytesAfter)),
		Summary:     summary,
		ArchivePath: archivePath,
	}, nil
}

// summarizeTranscript runs ONE model turn over the reduced transcript and returns its text.
func (b *NativeBridge) summarizeTranscript(ctx context.Context, opts scheduler.CompactConversationOpts, transcript, ledger string) (string, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return "", errors.New("no tenant in context — cannot resolve the summarize provider")
	}
	providerID, model, ok := adapter.SplitForServe(opts.ModelRef)
	if !ok || providerID == "" || model == "" {
		return "", fmt.Errorf("model ref %q has no provider/model to summarize with", opts.ModelRef)
	}
	if b.resolver == nil {
		return "", errors.New("no provider resolver for the summarize turn")
	}
	prov, err := b.resolver.Get(ctx, tenantID, providerID)
	if err != nil {
		return "", fmt.Errorf("resolve provider: %w", err)
	}

	userText := "Here is the conversation transcript to summarize:\n\n" + transcript
	// The ledger is handed to the summarizer as well as carried verbatim alongside its answer: given
	// only the reduced transcript it could not know what an id refers.
	if strings.TrimSpace(ledger) != "" {
		userText += "\n\n" + askCompactLedgerInstruction + "\n\n" + ledger
	}
	req := TurnRequest{
		Model:        model,
		System:       []SystemBlock{{Text: askCompactSummaryInstruction, Cache: true}},
		Messages:     []Message{{Role: RoleUser, Content: []Content{{Text: &userText}}}},
		MaxTokens:    maxOutputTokens(),
		CacheControl: CacheControlSystemAndTools,
		// Same stable per-conversation id the turns use (providers that key
		// off x-opencode-session require it).
		SessionID: opts.ConversationID,
	}
	stream, err := prov.StreamTurn(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var out strings.Builder
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if delta, isText := evt.(TextDelta); isText {
			out.WriteString(delta.Text)
		}
	}
	return out.String(), nil
}

// reduceAskHistory flattens a full history into a text-only transcript for summarization, returning
// it plus the before/after byte sizes.
func reduceAskHistory(history []Message) (string, int, int) {
	var before, after int
	var b strings.Builder
	for _, m := range history {
		role := "USER"
		switch m.Role {
		case RoleAssistant:
			role = "ASSISTANT"
		case RoleTool:
			role = "TOOL"
		}
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				before += len(*c.Text)
				t := strings.TrimSpace(*c.Text)
				if t == "" {
					continue
				}
				b.WriteString(role + ": " + t + "\n")
				after += len(t)
			case c.Image != nil:
				before += len(*c.Image)
				line := role + ": [an image was shared or shown here — image content dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			case c.ToolUse != nil:
				before += len(c.ToolUse.ArgsJSON)
				line := role + ": [called the tool " + c.ToolUse.Name + " — arguments dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			case c.ToolResult != nil:
				before += len(c.ToolResult.Content)
				status := "returned output"
				if c.ToolResult.IsError {
					status = "FAILED"
				}
				line := role + ": [a tool call " + status + " — output dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			}
		}
	}
	return b.String(), before, after
}

// compactedHistoryMarker frames a summary as the single assistant message that replaces the
// collapsed history.
func compactedHistoryMarker(summary, ledger string) *string {
	s := "[Earlier conversation compacted to save context. Summary of everything before this point:]\n\n" +
		summary +
		"\n\n[End of compacted summary. The messages after this one are the most recent turns, verbatim.]"
	if strings.TrimSpace(ledger) != "" {
		s += "\n\n" + askCompactLedgerMarkerFrame + "\n\n" + ledger
	}
	return &s
}

// askCompactLedgerMarkerFrame introduces the verbatim ledger inside the compacted history.
const askCompactLedgerMarkerFrame = "[Identifiers carried verbatim out of the collapsed history. These were extracted from the tool calls and tool results the summary above no longer contains, so prefer them over the summary's own wording when the two disagree:]"

// askCompactLedgerInstruction introduces the ledger to the SUMMARIZER.
const askCompactLedgerInstruction = `A list of identifiers that appeared VERBATIM in the transcript follows. Note that some of the transcript — tool arguments, tool results and images — was dropped before you saw it, so this list contains facts you cannot otherwise recover. Use it to attribute identifiers to their subject wherever the transcript supports it, and make sure the identifiers that matter for continuing the work are named in your summary. Do NOT simply copy the list out: it is carried verbatim alongside your summary already, so your job is to say what each identifier is for.`

// archiveAskHistoryLocked copies the live history file to a timestamped sibling before compaction
// replaces it, returning the path it wrote (empty when there was nothing to archive or the write.
func (b *NativeBridge) archiveAskHistoryLocked(sessionID string) string {
	dir := b.askHistoryDir
	if dir == "" {
		return ""
	}
	base := filepath.Join(dir, askHistoryFilename(sessionID)+".json")
	raw, err := os.ReadFile(base)
	if err != nil {
		return "" // no live file yet — nothing to archive
	}
	dst := fmt.Sprintf("%s.compacted-%s.bak", base, time.Now().UTC().Format("20060102T150405"))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		b.log.Warn("orchicon: ask history archive failed", "session", sessionID, "error", err)
		return ""
	}
	return dst
}

// humanBytes renders a byte count for a user-facing notice.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// --- the identifier ledger -------------------------------------------------
//
// The collapse loses context in TWO places, and the summary prompt only addresses
// the first. Stage 1 (reduceAskHistory) replaces tool arguments, tool results and
// images with one-line markers BEFORE the summarizer sees anything — measured on
// the over-limit conversation that motivated the pressure gate, tool results were
// 37% of the bytes and images 46%, against 3% of actual text. Nearly every concrete
// identifier a continuing conversation needs lives in exactly that dropped
// material: the file paths in a read/edit call's arguments, the work item and
// execution ids inside a tool's JSON result. The summary instruction asks the model
// to "preserve identifiers verbatim (paths, ids, names, numbers)", but it cannot
// preserve what it was never shown, and asking harder does not fix that.
//
// So the identifiers are extracted BEFORE the reduction — deterministically, with no
// model call, from the raw history — and they ride two ways:
//
//   - into the summarize request, so the summary can ATTRIBUTE them ("work item
//     01M33R… is the crash fix") rather than listing tokens it cannot explain;
//   - verbatim into the compacted history itself, so a summary that quietly omits
//     one cannot lose it. The verbatim list is the safety net; the attribution is
//     the value.
//
// What is deliberately NOT extracted: prose, decisions and narrative. Summarizing
// those is what the model call is for, and a deterministic extractor that tried
// would produce a worse paraphrase than the summary it accompanies.

var (
	// askLedgerEntityID matches Orchicon's own entity ids: 26 characters of Crockford base32 (0-9 and
	// A-Z, minus I, L, O and U — the alphabet every id in this codebase is minted.
	askLedgerEntityID = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`)
	// askLedgerPath matches an absolute path of at least two segments.
	askLedgerPath = regexp.MustCompile(`(?:^|[\s"'(\[=:,])(/[A-Za-z0-9._@%+-]+(?:/[A-Za-z0-9._@%+-]+)+)`)
	// askLedgerGitObject finds CANDIDATE git object names; ledgerGitObject filters
	// them. The filter is what separates a commit from a count.
	askLedgerGitObject = regexp.MustCompile(`[0-9a-f]{7,40}`)
)

// askLedgerMaxPerKind caps each list in the ledger.
const askLedgerMaxPerKind = 40

// askHistoryLedger scans the RAW history and returns the identifier ledger. An
// empty string means nothing identifiable was found — the caller carries nothing.
func askHistoryLedger(history []Message) string {
	ids := newLedgerSet(askLedgerMaxPerKind)
	paths := newLedgerSet(askLedgerMaxPerKind)
	objects := newLedgerSet(askLedgerMaxPerKind)
	tools := newLedgerSet(askLedgerMaxPerKind)

	for _, m := range history {
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				scanLedgerText(*c.Text, ids, paths, objects)
			case c.ToolUse != nil:
				// The tool's NAME is known exactly — no guessing — so it is recorded
				// from the field rather than scraped from the JSON.
				tools.add(c.ToolUse.Name)
				scanLedgerText(c.ToolUse.ArgsJSON, ids, paths, objects)
			case c.ToolResult != nil:
				scanLedgerText(c.ToolResult.Content, ids, paths, objects)
			}
		}
	}

	var b strings.Builder
	b.WriteString(ids.render("Entity ids", false))
	b.WriteString(paths.render("File paths", false))
	b.WriteString(objects.render("Git objects (commits), by name", false))
	// Tools carry their call counts: how often a tool was used is itself working
	// state ("bash 14, edit 3" says what the session was doing).
	b.WriteString(tools.render("Tools invoked", true))
	return strings.TrimRight(b.String(), "\n")
}

// scanLedgerText records every identifier-shaped token in one string.
func scanLedgerText(text string, ids, paths, objects *ledgerSet) {
	if text == "" {
		return
	}
	for _, m := range askLedgerEntityID.FindAllString(text, -1) {
		ids.add(m)
	}
	for _, m := range askLedgerPath.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			paths.add(m[1])
		}
	}
	for _, m := range askLedgerGitObject.FindAllString(text, -1) {
		if ledgerGitObject(m) {
			objects.add(m)
		}
	}
}

// ledgerGitObject reports whether a hex token is a git object name rather than a number or an
// ordinary word.
func ledgerGitObject(tok string) bool {
	if len(tok) < 7 || len(tok) > 40 {
		return false
	}
	var letter, digit bool
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'f':
			letter = true
		default:
			return false
		}
	}
	return letter && digit
}

// ledgerSet is an order-preserving, de-duplicating, capped value set that counts repeats.
type ledgerSet struct {
	order []string
	seen  map[string]int
	cap   int
	extra int
}

func newLedgerSet(cap int) *ledgerSet {
	return &ledgerSet{seen: map[string]int{}, cap: cap}
}

// add records a value once and counts every repeat.
func (s *ledgerSet) add(v string) {
	if v == "" {
		return
	}
	if n, ok := s.seen[v]; ok {
		s.seen[v] = n + 1
		return
	}
	s.seen[v] = 1
	if len(s.order) >= s.cap {
		s.extra++
		return
	}
	s.order = append(s.order, v)
}

// render is the set as one labelled line, or "" when it is empty — an empty
// section would be noise in a prompt that must stay small.
func (s *ledgerSet) render(label string, withCounts bool) string {
	if len(s.order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.order))
	for _, v := range s.order {
		// A count is shown whenever the caller asked for counts, INCLUDING 1: for the tools line "bash (1)"
		// is the point (how much a tool was used is working state).
		if withCounts {
			parts = append(parts, fmt.Sprintf("%s (%d)", v, s.seen[v]))
			continue
		}
		parts = append(parts, v)
	}
	line := label + ": " + strings.Join(parts, ", ")
	if s.extra > 0 {
		line += fmt.Sprintf(" (+%d more, not listed)", s.extra)
	}
	return line + "\n"
}
