package antigravity

// session.go — ONE long-lived `agy acp` session, driven across N turns.
//
// THE SHAPE: spawn the child, handshake, open (or resume) a conversation, set the model, then loop over
// the turn queue. Each turn is one `session/prompt` that blocks until agy answers with a stopReason, while
// the update stream is drained by a pump goroutine feeding the mapper.
//
// THE FOUR BEHAVIOURS THAT ARE NOT OBVIOUS, AND WHY EACH IS WRITTEN THIS WAY:
//
//  1. INJECTION IS QUEUED, NOT PREEMPTIVE. agy defines no way to add a message to a running turn (docs
//     GAP TABLE: "not supported — must queue the message and send it after stopReason"). So a
//     mid-run message lands in `turns`, and the run loop delivers it as the next prompt the moment the
//     in-flight turn ends. The execution therefore does not finish while the queue is non-empty.
//
//  2. ABORT IS TWO-STAGE. `session/cancel` is a NOTIFICATION: it ends the turn with
//     stopReason:"cancelled" (recorded, cancel_midturn.jsonl). If agy does not honour it inside the grace
//     window — a wedged child, a command it cannot interrupt — the process is closed, which is the only
//     remaining way to stop the model.
//
//  3. COMPACTION IS A PROMPT. `/compact` is one of agy's own `available_commands` and is delivered as the
//     literal text of an ordinary turn (docs §3.10, compact.jsonl:22). It is queued at the FRONT of the
//     queue because a compaction request is about freeing context before the next piece of work, not
//     after it.
//
//  4. THE SESSION ID IS BOUND BEFORE THE CONVERSATION IS USED, not when an update mentions it. A resume
//     can be rejected by the single-writer check, and discovering that AFTER prompts have been written
//     would mean two clients already interleaved on one wire.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// abortGrace is how long a cancelled turn is given to end on its own before the process is closed. A var
// so a test can shorten it.
var abortGrace = 3 * time.Second

// compactPrompt is the literal slash command agy uses for in-place compaction (docs §3.10).
const compactPrompt = "/compact"

// queuedTurn is one turn waiting to be delivered.
type queuedTurn struct {
	text string
	// kind tags why the turn exists, for the transcript: "user" (an injected message), "compact".
	kind string
}

// session is ONE live agy session for ONE execution.
type session struct {
	b        *Bridge
	execID   string
	tenantID string
	manifest scheduler.ExecutionManifest
	cbs      scheduler.ExecutionCallbacks

	// client is the ACP client (one child process).
	client *acp.Client
	// conv is the ACP session (one agy conversation).
	conv *acp.Session
	// spec is the spawn spec actually used, for logs (its String redacts the API key).
	spec SpawnSpec
	// model is the bare model id the session drives.
	model string
	// resumed is the agy session id this session re-attached to, if any.
	resumed string

	mapper *Mapper
	policy *PolicyHandler

	mu       sync.Mutex
	finished bool
	aborted  bool
	abortWhy string
	// turns is the pending turn queue, in delivery order.
	turns []queuedTurn
	// cancelledSeq is the per-session transcript sequence.
	seq int64
	// sessionID is the agy conversation id once known.
	sessionID string

	// wake releases the run loop when a turn is enqueued while it is idle.
	wake chan struct{}
	// handled counts the updates the pump has finished mapping. It is the barrier's other half: an empty
	// events channel does not mean the last update has been MAPPED, only that it has been taken.
	handled atomic.Int64
}

// newSession builds the session and its mapper/policy for one execution.
func newSession(b *Bridge, execID, tenantID string, manifest scheduler.ExecutionManifest, cbs scheduler.ExecutionCallbacks) *session {
	s := &session{
		b:        b,
		execID:   execID,
		tenantID: tenantID,
		manifest: manifest,
		cbs:      cbs,
		resumed:  strings.TrimSpace(manifest.ContinueFromSessionID),
		wake:     make(chan struct{}, 1),
	}
	if cbs == nil {
		s.cbs = &followUpCallbacks{log: b.log, execID: execID}
	}
	s.policy = &PolicyHandler{
		ProjectDir:  manifest.ProjectDir,
		WorktreeDir: manifest.WorktreePath,
		PolicyPath:  DefaultPolicyPath(),
		Log:         b.log,
		OnViolation: func(tool, reason string) {
			s.recordError(fmt.Sprintf("antigravity blocked %s: %s", tool, reason))
		},
	}
	s.mapper = NewMapper(execID, s.cbs, MapperDeps{
		TenantID:      tenantID,
		Model:         "",
		ExecDir:       executionDir(manifest),
		Manifest:      manifest,
		FileEdits:     b.fileEdits,
		UsageRecorder: b.usageRecorder,
		BindSession:   s.bindSession,
		Record:        s.recordPart,
		OnViolation:   s.onMapperViolation,
		Log:           b.log,
	})
	return s
}

// bindSession claims the agy session id for this execution (the single-writer invariant).
func (s *session) bindSession(_ context.Context, sid string) {
	if !s.b.bindSessionID(sid, s.execID) {
		s.b.log.Error("antigravity: a live session already owns this agy session id — refusing the second writer",
			"session", sid, "execution", s.execID)
	}
	s.mu.Lock()
	s.sessionID = sid
	s.mu.Unlock()
}

// sessionIdentity is the agy session id this session is bound to (the id agy reported, else the resumed
// id). It is what ContinueSession hands back and what the runtime's session store records.
func (s *session) sessionIdentity() string {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	if sid != "" {
		return sid
	}
	if s.conv != nil {
		return s.conv.ID()
	}
	return s.resumed
}

// onMapperViolation turns a post-hoc boundary violation into a cancelled run: the tool call already
// happened, so the only honest response is to stop the session and fail the execution loudly rather than
// let it continue as if the policy had held.
func (s *session) onMapperViolation(tool, reason string) {
	s.b.log.Error("antigravity: cancelling the session after a policy escape",
		"execution", s.execID, "tool", tool, "reason", reason)
	s.abort("policy_violation: " + reason)
}

// abort cancels the turn and, after the grace window, closes the process.
func (s *session) abort(reason string) {
	s.mu.Lock()
	if s.aborted || s.finished {
		s.mu.Unlock()
		return
	}
	s.aborted = true
	s.abortWhy = reason
	conv := s.conv
	client := s.client
	s.mu.Unlock()

	if conv != nil {
		// `session/cancel` is the graceful half: agy ends the turn with stopReason "cancelled", which
		// carries the turn's usage. Failure is not fatal — the escalation below covers it.
		if err := conv.Cancel(); err != nil {
			s.b.log.Debug("antigravity: session/cancel failed; escalating to process shutdown",
				"execution", s.execID, "error", err)
		}
	}
	grace := abortGrace
	go func() {
		select {
		case <-time.After(grace):
		case <-s.wake:
			// The run loop released us: the turn ended without needing the escalation.
			return
		}
		s.mu.Lock()
		finished := s.finished
		s.mu.Unlock()
		if finished {
			return
		}
		s.b.log.Warn("antigravity: the cancelled turn did not end within the grace window — closing the agy process",
			"execution", s.execID, "grace", grace.String())
		if client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.Close(ctx)
		}
	}()
}

// SendTurn queues a mid-run message. It is delivered as the NEXT `session/prompt`, immediately after the
// in-flight turn reports its stopReason.
func (s *session) SendTurn(ctx context.Context, message string) error {
	text := strings.TrimSpace(message)
	if text == "" {
		return errors.New("antigravity: an injected message must not be empty")
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("antigravity execution %s has no live session (already finished)", s.execID)
	}
	s.turns = append(s.turns, queuedTurn{text: message, kind: "user"})
	s.mu.Unlock()

	s.recordPart(ctx, db.SessionPartUserMessage, map[string]any{"text": message})
	s.kick()
	return nil
}

// Compact queues agy's own `/compact` slash command at the FRONT of the turn queue.
//
// The provider/model/remainingScope arguments are accepted for interface parity with the claude bridge and
// are LOGGED, not acted on: agy compacts its own context (there is no scope for Orchicon to hand over), and
// inventing a use for them here would be a promise this adapter cannot keep.
func (s *session) Compact(ctx context.Context, provider, model, remainingScope string) error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("antigravity execution %s has no live session (already finished)", s.execID)
	}
	s.turns = append([]queuedTurn{{text: compactPrompt, kind: "compact"}}, s.turns...)
	s.mu.Unlock()

	s.b.log.Info("antigravity: compaction queued for the live session",
		"execution", s.execID, "provider", provider, "model", model, "remaining_scope", remainingScope)
	s.recordPart(ctx, db.SessionPartCompacted, map[string]any{
		"source": "antigravity_compact_command",
		"note":   "agy's own /compact slash command; remainingScope is informational for this adapter",
	})
	s.kick()
	return nil
}

// kick wakes the run loop when a turn is queued while it is idle.
func (s *session) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// popTurn takes the next queued turn WITHOUT blocking.
//
// IT MUST NOT BLOCK, and that is the whole reason it is shaped this way: a turn that ends with nothing
// queued IS the end of the execution. Blocking for "one more message" would leave every ordinary run hanging
// until its wall-clock budget expired, and a session that never ends is indistinguishable from a wedged one.
// The queue is read once at the turn boundary, which is exactly the window in which an injected message can
// be delivered (agy defines no way to add one to a running turn).
func (s *session) popTurn() (queuedTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.turns) == 0 {
		return queuedTurn{}, false
	}
	t := s.turns[0]
	s.turns = s.turns[1:]
	return t, true
}

// QueuedTurns reports how many turns are waiting. The run loop uses it to decide whether a completed turn
// ends the execution.
func (s *session) QueuedTurns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}

// recordPart persists one durable transcript part.
func (s *session) recordPart(ctx context.Context, kind string, payload map[string]any) {
	if s.b.sessionStore == nil {
		return
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	body, _ := json.Marshal(payload)
	part := db.SessionPart{
		ExecutionID: s.execID,
		TenantID:    s.tenantID,
		Seq:         seq,
		Kind:        kind,
		Payload:     body,
		CreatedAt:   time.Now(),
	}
	if err := s.b.sessionStore(ctx, s.execID, s.tenantID, []db.SessionPart{part}); err != nil {
		s.b.log.Warn("antigravity: record session part failed",
			"execution", s.execID, "kind", kind, "error", err)
	}
}

// recordError persists a durable error part so a refusal or a policy escape is readable after the run.
func (s *session) recordError(message string) {
	s.recordPart(context.Background(), db.SessionPartError, map[string]any{"message": message})
}

// finish emits the terminal callbacks exactly once.
func (s *session) finish(ctx context.Context, ok bool, errMsg string, retErr error) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	out := s.mapper.Output()
	s.mu.Unlock()
	// Release a pending abort escalation: the turn is over, so there is nothing left to kill.
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if retErr != nil && errMsg == "" {
		errMsg = retErr.Error()
	}
	s.b.log.Info("antigravity: session finished",
		"execution", s.execID, "ok", ok, "error", errMsg, "session", s.sessionIdentity())
	s.cbs.OnResult(ctx, s.execID, ok, out, errMsg)
}

// isFinished reports whether the terminal callbacks have already fired.
func (s *session) isFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// abortReason is the reason an abort was requested, for the failure message.
func (s *session) abortReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortWhy
}

// stderrTail renders the LAST stderr lines agy emitted, for a failure message.
func (s *session) stderrTail() string {
	if s.client == nil {
		return ""
	}
	lines := s.client.Stderr()
	if len(lines) == 0 {
		return ""
	}
	// Only the tail: a child that logged a lot before failing buries the cause otherwise.
	const keep = 5
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return "; stderr: " + strings.Join(lines, " | ")
}

// authMessage maps an error onto the actionable auth instruction when it is an auth failure, else returns
// the error's own text.
//
// EVERY error path goes through this, not only the authenticate result: agy defers its credential check to
// the first model call (docs §3.12), so a signed-out host surfaces as a prompt error or a stderr line, and a
// mapper that only watched `authenticate` would turn the one actionable failure into a generic one.
func authMessage(err error) string {
	if err == nil {
		return ""
	}
	if msg, ok := adapter.AntigravityAuthFailure(err.Error()); ok {
		return msg
	}
	var authErr *acp.AuthError
	if errors.As(err, &authErr) {
		return authErr.Message
	}
	return err.Error()
}

// modelIDFor resolves the agy model id this session should set.
//
// It returns "" when no model was configured, which means KEEP agy's default — that is different from an
// unknown model and must not be treated as one.
func modelIDFor(m scheduler.ExecutionManifest) (string, error) {
	ref := strings.TrimSpace(m.ModelRef)
	if ref == "" {
		ref = strings.TrimSpace(m.DefaultModelRef)
	}
	if ref == "" {
		return "", nil
	}
	id, err := parseModelRefLoose(ref)
	if err != nil {
		return "", fmt.Errorf("antigravity: model ref %q: %w", ref, err)
	}
	return strings.TrimSpace(id), nil
}

// stopReasonMessage renders the failure message for a turn that ended without an end_turn.
func stopReasonMessage(stop string) string {
	switch strings.TrimSpace(stop) {
	case "", acp.StopReasonEndTurn:
		return ""
	case acp.StopReasonCancelled:
		return "execution cancelled"
	default:
		return fmt.Sprintf("antigravity turn ended with stopReason %q", stop)
	}
}

// loggableSpec renders the spawn spec for a log line without any environment VALUE.
func loggableSpec(spec SpawnSpec) slog.Value { return spec.LogValue() }
