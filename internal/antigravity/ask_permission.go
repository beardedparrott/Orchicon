package antigravity

// ask_permission.go — THE CONSENT CARD, over ACP's `session/request_permission`.
//
// THIS IS THE HALF THAT MAKES AGY AN ASK ADAPTER RATHER THAN A WORKER ONE. A worker execution has nobody
// attached, so permissions.go answers every request from the platform's own rule set. An Ask
// conversation has an OPERATOR, so the same request becomes a CARD: the session raises the identical
// `permission` bus event the claude Ask path raises, blocks with NO TIMEOUT, and answers agy from
// whatever the operator chose.
//
// THE ORDER IS THE WHOLE DESIGN, and each step exists because putting it later is a real defect:
//
//  1. MODE GATE (modegate.go), BEFORE any card. A tool the turn's mode refuses never asks the operator
//     anything — the platform has already decided, and a card would be an invitation to override a
//     decision that is not the operator's to make per call. It is answered with the reject option and the
//     refusal text is queued for the model (see pendingNotice), because ACP carries no reason on a
//     rejection.
//
//  2. NEVER-ALLOW + BOUNDARY, still without a card. The claude Ask profile applies its never-allow class
//     and the project boundary in the hook before the operator is asked; this is the same rung, run
//     through the same shared rule set (claude.DecideTool / workerrestrict) rather than reimplemented.
//
//  3. THE CARD, for the tools claude's Ask profile actually asks about (askModeMayCard): writes, shell,
//     web and third-party MCP. A read or a grep is not a decision anybody needs to make.
//
//  4. THE ANSWER, mapped to an OPTION ID agy actually offered — never a literal. agy 1.5.13 offers
//     `allow_once`/`reject_once` with ids `allow`/`deny`, but the ids are agy's to choose, so the mapping
//     is by KIND (acp.AllowOptionID/RejectOptionID) and the offered id is echoed back.
//
// FAIL CLOSED, ALWAYS. No answer, a teardown, a cancelled context, an unknown decision string — every one
// of them answers REJECT. There is no branch in this file that returns an allow the operator did not give.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askPermissionHandler implements acp.Handler for ONE Ask conversation: the mode gate, the boundary, the
// card, and the operator's answer.
//
// IT IS PER-SESSION, not per-bridge: every field it reads (the mode, the project dir, the bus) belongs to
// one conversation, and a handler shared across conversations would need to re-discover which one it was
// answering for on every call.
type askPermissionHandler struct {
	// s is the conversation's session: the bus, the parked waits, the pending notice, and the teardown
	// signal all live there.
	s *askSession
	log *slog.Logger
}

// logger returns a usable logger.
func (h *askPermissionHandler) logger() *slog.Logger {
	if h == nil || h.log == nil {
		return slog.Default()
	}
	return h.log
}

// RequestPermission implements acp.Handler.
//
// IT NEVER RETURNS AN ERROR. An error makes the acp client answer with the reject option, which is what
// most of our failure paths produce anyway — but returning a DECISION keeps the reason available for the
// bus event the operator sees. A panic is recovered into a reject for the same reason: the wire fails
// closed either way, and a recovered panic does not take the reader's request path down with it.
func (h *askPermissionHandler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (acp.PermissionDecision, error) {
	defer func() {
		if r := recover(); r != nil {
			h.logger().Error("antigravity ask: permission handler panicked — answering REJECT (fail closed)",
				"tool", req.ToolCall.Name, "panic", fmt.Sprint(r))
		}
	}()
	if h == nil || h.s == nil {
		return rejectDecision(req.Options), nil
	}
	h.s.inflight.Add(1)
	defer h.s.inflight.Add(-1)

	tool := strings.TrimSpace(req.ToolCall.Name)
	input := agyInput(req.ToolCall.RawInput, req.ToolCall.Args)
	mode := h.s.turnMode()

	// --- 1. THE MODE GATE. No card, ever, for a refused tool. ------------------------------------
	if denied, refusal := askModeDenial(mode, tool, input); denied {
		h.logger().Warn("antigravity ask: REFUSED by the mode boundary — no card raised",
			"conversation", h.s.convID, "mode", mode, "tool", tool)
		// The operator sees the refusal INLINE (a tool_result, exactly as a refused call renders in the
		// claude Ask transcript), and the model is told in the next prompt: ACP carries no reason on a
		// reject, so without both halves the model would see an unexplained failure.
		h.s.emitRefusal(req.ToolCall, tool, refusal)
		h.s.queueNotice(refusal)
		return rejectDecision(req.Options), nil
	}

	// --- 2. NEVER-ALLOW + THE PROJECT BOUNDARY. Still no card: these are standing decisions. ------
	if refusal, refused := h.boundaryRefusal(tool, input); refused {
		h.logger().Warn("antigravity ask: REFUSED by the platform boundary — no card raised",
			"conversation", h.s.convID, "tool", tool, "reason", refusal)
		h.s.emitRefusal(req.ToolCall, tool, refusal)
		h.s.queueNotice(refusal)
		return rejectDecision(req.Options), nil
	}

	// --- 3. AUTO-ALLOW the tools nobody needs to be asked about. ----------------------------------
	if !askModeMayCard(tool) {
		allowID, ok := acp.AllowOptionID(req.Options)
		if !ok {
			// agy offered no allow at all. `cancelled` does not grant the call, which is the only safe
			// reading of "the agent did not offer the outcome we want".
			h.logger().Warn("antigravity ask: this call needs no card but agy offered no allow option — answering cancelled",
				"conversation", h.s.convID, "tool", tool)
			return acp.PermissionDecision{Cancelled: true}, nil
		}
		h.logger().Debug("antigravity ask: auto-allowed (no card needed)", "conversation", h.s.convID, "tool", tool)
		return acp.PermissionDecision{OptionID: allowID}, nil
	}

	// --- 4. THE CARD. Block until the operator answers, or the session goes. ----------------------
	answer, ok := h.s.awaitAsk(ctx, req, tool, input)
	if !ok {
		// NO ANSWER IS A REJECT. Teardown, a cancelled turn, or a superseded session all land here.
		h.logger().Info("antigravity ask: the ask was not answered — answering REJECT (fail closed)",
			"conversation", h.s.convID, "tool", tool)
		return rejectDecision(req.Options), nil
	}
	return mapAskAnswer(req.Options, answer), nil
}

// boundaryRefusal runs the claude Ask profile's standing rules — the never-allow command class, the
// protected roots, the operator's policy file and the project boundary — and reports whether this call is
// refused WITHOUT a card.
//
// IT CALLS claude.DecideToolForAsk, NOT A COPY. That function IS the Ask profile's decision (it is what
// claude's own PermissionRequest hook runs), so the two adapters cannot disagree about whether `rm -rf /`
// is a standing refusal. The agy-specific spelling translation is hookToolName/hookToolInput, which
// permissions.go already owns and the worker path already uses.
func (h *askPermissionHandler) boundaryRefusal(tool string, input map[string]any) (string, bool) {
	call := acp.ToolCallInfo{Name: tool, RawInput: jsonOf(input)}
	hook := claude.HookInput{
		ToolName:  hookToolName(tool),
		ToolInput: hookToolInput(tool, call),
	}
	verdict := claude.DecideToolForAsk(hook, h.s.workDir(), permpolicy.DefaultPath())
	// `Ask` is not a refusal: it is exactly the decision this handler now makes, by card.
	if verdict.Allow || verdict.Ask {
		return "", false
	}
	reason := strings.TrimSpace(verdict.Reason)
	if reason == "" {
		reason = "refused by the Orchicon Ask policy"
	}
	return reason, true
}

// rejectDecision picks the reject answer, or `cancelled` when agy offered no reject option.
func rejectDecision(opts []acp.PermissionOption) acp.PermissionDecision {
	if id, ok := acp.RejectOptionID(opts); ok {
		return acp.PermissionDecision{OptionID: id}
	}
	return acp.PermissionDecision{Cancelled: true}
}

// mapAskAnswer turns the operator's decision into an option id agy actually offered.
//
// THE ALLOW-ALWAYS ARM IS DEFENSIVE, NOT DEAD. askorchicon sends only "once" or "reject"
// (consent.go consentResponse), so "always" never arrives today — but a client that learns to send it must
// not silently lose the permanence it asked for, and agy may one day offer `allow_always`. When it is not
// offered, `allow_once` is the only honest downgrade: the call still runs, and the permanence is the
// CALLER's to record (the grants live in askorchicon's store, never here — the same division claude's Ask
// path documents).
func mapAskAnswer(opts []acp.PermissionOption, answer string) acp.PermissionDecision {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "once", "allow", "approve", "allowed", "user_temporary", "user_session":
		return acp.PermissionDecision{OptionID: allowOption(opts, false)}
	case "always", "permanent", "user_permanent":
		return acp.PermissionDecision{OptionID: allowOption(opts, true)}
	default:
		// "reject"/"deny"/"denied"/anything unrecognised/"" — all of them refuse. An unreadable answer is
		// not a licence to allow.
		return rejectDecision(opts)
	}
}

// allowOption resolves the approving option id, preferring an always-kind when one is offered AND the
// answer asked for it.
func allowOption(opts []acp.PermissionOption, preferAlways bool) string {
	if preferAlways {
		for _, o := range opts {
			if o.Kind == acp.OptionKindAllowAlways {
				return o.OptionID
			}
		}
	}
	if id, ok := acp.AllowOptionID(opts); ok {
		return id
	}
	// No allow offered at all: "" makes the acp client fail closed on the reject option, which is the only
	// honest answer to "the agent did not offer the outcome we want".
	return ""
}

// awaitAsk raises the card and blocks for the operator's answer.
//
// THE WAIT HAS NO DEFAULT BOUND, matching claude (hookConsentWaitDefault = 0 = none): this is a human
// decision and a timer that expires it would refuse work the operator was about to approve.
// ORCHICON_ANTIGRAVITY_CONSENT_WAIT exists for an operator who wants a shorter leash and for tests; the
// session's own lifetime bounds it regardless, so a teardown can never leave a card parked forever.
func (s *askSession) awaitAsk(ctx context.Context, req acp.PermissionRequest, tool string, input map[string]any) (string, bool) {
	permID := s.nextAskID()
	ch := make(chan string, 1)
	s.askMu.Lock()
	s.waits[permID] = ch
	s.askMu.Unlock()
	defer func() {
		s.askMu.Lock()
		delete(s.waits, permID)
		s.askMu.Unlock()
	}()

	// THE EVENT IS THE SAME SHAPE claude's Ask path emits (hookPermissionEvent, consent_hook.go:265):
	// the operator's card is built from it by a collector that knows nothing about agy.
	s.busEmit(scheduler.SessionEvent{
		Kind:         "permission",
		PermissionID: permID,
		Tool:         tool,
		Command:      toolInputCommand(input),
		Targets:      askPermissionTargets(tool, input, req.ToolCall),
		InputJSON:    jsonString(input),
	})

	// THE WINDOW IS OPTIONAL AND THE DEFAULT IS NONE, so `expired` is often a nil channel — a nil
	// channel blocks forever in a select, which is exactly "no bound".
	var expired <-chan time.Time
	if d := askConsentWait(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		expired = timer.C
	}

	select {
	case answer := <-ch:
		return answer, true
	case <-expired:
		s.log.Info("antigravity ask: the consent window closed with no answer — refusing the call",
			"conversation", s.convID, "tool", tool)
		return "", false
	case <-ctx.Done():
		// The acp turn's own context: a session cancel or a teardown.
		return "", false
	case <-s.done:
		// The conversation's session ended while the card was open.
		return "", false
	}
}

// askPermissionTargets lists what an ask is ABOUT, mirroring claude's askTargets: the paths a path-carrying tool
// names, the URL a fetch names, the query a search names, and — for an MCP tool — the tool itself.
//
// THE WEB KEYS ARE LOAD-BEARING. `read_url_content`/`search_web` name no path, so with only the path keys
// the card would arrive with EMPTY targets and the operator would be asked to approve a fetch without
// being told which page. agy's spellings are Capitalised (`CommandLine`, `TargetFile`, `Url`, `query`),
// which is why the lookups go through firstString's case-insensitive table rather than map indexing.
func askPermissionTargets(tool string, input map[string]any, call acp.ToolCallInfo) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}
	for _, k := range []string{
		"TargetFile", "target_file", "Path", "path", "file_path", "AbsolutePath", "absolute_path",
		"Url", "url", "URI", "uri", "query", "Query", "SearchPath", "search_path",
	} {
		add(firstString(input, k))
	}
	for _, l := range call.Locations {
		add(l.Path)
	}
	// AN MCP TOOL NAMES ITSELF AS THE TARGET: there is no path, and `mcp_github_create_issue` with no
	// target tells the operator what is being consented to only if the name travels.
	if isMCPTool(tool) && len(out) == 0 {
		add(tool)
	}
	return out
}

// jsonOf re-marshals a decoded input map so a helper that takes json.RawMessage can read it.
func jsonOf(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
