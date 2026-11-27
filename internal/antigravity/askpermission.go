package antigravity

// askpermission.go — THE CARDED PERMISSION HANDLER: agy's `session/request_permission` becomes the SAME
// `permission` bus event the claude Ask path emits, and the model BLOCKS until the operator answers.
//
// THE ORDER OF THE THREE STEPS IS THE WHOLE POLICY, and each one is a place a boundary could leak:
//
//  1. THE MODE GATE (askmodegate.go) runs FIRST, before a card exists. A tool the turn's mode refuses is
//     answered REJECT on the wire and never put to the operator as a question — because a question whose
//     "yes" the platform will not honour is worse than no question at all.
//  2. THE AUTO-ALLOW SET: reads and MCP tools are not consent decisions. agy never asks about a read in
//     practice, but a permission policy that consulted the operator on `view_file` would train them to
//     approve without reading, which is the failure mode consent UIs actually die of.
//  3. THE CARD, for the five work tools the claude Ask profile cards (bash/write/edit/websearch/webfetch).
//     It parks on a channel with NO TIMEOUT: the wait is a human, and the claude path's own default is 0
//     (no expiry, internal/claude/consent_hook.go). Every abnormal exit — teardown, a cancelled turn, a
//     connection that died — answers REJECT, so the wire never carries an allow the operator did not give.
//
// THE GRANT STORE IS NOT HERE. askorchicon owns it: it records an ALLOW_SESSION against the same card the
// claude path raises, and its `consentResponse` maps that to "once" for this adapter. So the only mapping
// this file does is "which option id does agy's own option list offer for that answer".

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askCardedTools is the set of canonical tools that become a card — claude's Ask profile set, in agy's
// canonical vocabulary (askmodegate.go translates the raw names).
//
// READS ARE DELIBERATELY ABSENT. `view_file`, `grep_search`, `list_dir` and the platform's own MCP tools are
// not consent decisions: the mode gate has already decided whether the turn may read at all, and a card on a
// read would only teach the operator to click through.
var askCardedTools = map[string]bool{
	"bash":      true,
	"write":     true,
	"edit":      true,
	"websearch": true,
	"webfetch":  true,
}

// askPermissionHandler is one Ask session's acp.Handler. It is constructed per session rather than per
// turn, because a permission request is served on its own goroutine while the turn streams and the session
// outlives the turn that raised it.
type askPermissionHandler struct {
	s *askSession
}

var _ acp.Handler = (*askPermissionHandler)(nil)

// RequestPermission decides one asked-for tool call. EVERY return path either sends the option agy offered
// for the operator's answer or the reject option; there is no path that answers allow on its own.
func (h *askPermissionHandler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (acp.PermissionDecision, error) {
	s := h.s
	if s == nil {
		return acp.PermissionDecision{}, acp.ErrNoHandler
	}

	raw := strings.TrimSpace(req.ToolCall.Name)
	canonical := canonicalToolName(raw)
	input := agyInput(req.ToolCall.RawInput, req.ToolCall.Args)
	mode := s.currentMode()

	// --- 1. THE MODE GATE, BEFORE ANY CARD ---------------------------------
	if denied, refusal := askModeGate(mode, raw, canonical); denied {
		s.b.askLog().Warn("antigravity ask: the mode refused a tool call — no card is raised",
			"conversation", s.convID, "mode", mode, "tool", raw, "canonical", canonical, "reason", refusal)
		// THE REFUSAL REACHES THE TRANSCRIPT, because ACP has no channel to tell agy WHY. Without this the
		// model retries the same call and the operator sees a turn that stalled for no stated reason.
		s.busEmit(askRefusalEvent(req.ToolCall.ToolCallID, raw, refusal))
		return rejectDecision(req.Options), nil
	}

	// --- 2. NOT A CONSENT DECISION ----------------------------------------
	if !askCardedTools[canonical] {
		return allowDecision(req.Options), nil
	}

	// --- 3. THE CARD ------------------------------------------------------
	askID := s.nextAskID()
	ask := &agyAsk{
		// BUFFERED(1) so a reply that arrives after the waiter gave up (a teardown race) is parked rather
		// than blocking the reply RPC.
		decision: make(chan string, 1),
		session:  s.sessionIDSnapshot(),
		options:  req.Options,
		tool:     raw,
	}
	s.registerAsk(ask, askID)

	command := askCommand(input)
	targets := askTargets(input, req.ToolCall.Locations)

	s.busEmit(scheduler.SessionEvent{
		Kind: "permission",
		// THE FIELD SET IS claude's hookPermissionEvent VERBATIM (internal/claude/consent_hook.go): the
		// consent layer, the card UI and the grant store all read these six and nothing else.
		PermissionID: askID,
		Tool:         raw,
		Command:      command,
		Targets:      targets,
		InputJSON:    askInputJSON(input),
	})
	s.b.askLog().Info("antigravity ask: raised a permission card",
		"conversation", s.convID, "ask", askID, "tool", raw, "command", command, "targets", targets)

	// --- 4. BLOCK, WITH NO TIMEOUT ---------------------------------------
	//
	// THREE WAYS OUT, and two of them are fail-closed: the operator's answer, the connection's context
	// (a torn-down child or a cancelled turn), and the session's own liveness context (CloseAsk, or the
	// conversation being torn down mid-ask). The claude path expires after 0 seconds by default — never —
	// and this one matches it deliberately.
	decision := ""
	select {
	case d := <-ask.decision:
		decision = d
	case <-ctx.Done():
		s.takeAsk(askID)
		s.b.askLog().Info("antigravity ask: the ask was abandoned with the turn — answering REJECT",
			"conversation", s.convID, "ask", askID, "reason", ctx.Err())
		return rejectDecision(req.Options), nil
	case <-s.liveDone():
		s.takeAsk(askID)
		s.b.askLog().Info("antigravity ask: the conversation was torn down while an ask was open — answering REJECT",
			"conversation", s.convID, "ask", askID)
		return rejectDecision(req.Options), nil
	}

	resolved, allow := resolveAskAnswer(decision, req.Options)
	if !allow && strings.HasPrefix(decision, orchicon.ConsentRefusedPrefix) {
		// THE CONSENT LAYER'S REFUSAL TEXT is the only explanation the operator gets, and agy gets none over
		// the wire — so it goes on the transcript as the tool's own failed result.
		s.busEmit(askRefusalEvent(req.ToolCall.ToolCallID, raw,
			"REFUSED BY THE PLATFORM: "+strings.TrimPrefix(decision, orchicon.ConsentRefusedPrefix)))
	} else if !allow {
		s.b.askLog().Info("antigravity ask: the operator denied the tool call",
			"conversation", s.convID, "ask", askID, "tool", raw, "decision", decision)
	}
	return acp.PermissionDecision{OptionID: resolved}, nil
}

// resolveAskAnswer maps a decision string onto an option id agy offered, and reports whether it was an
// allow.
//
// THE STRINGS ARE askorchicon's VOCABULARY, NOT OURS (internal/askorchicon/consent.go consentResponse):
// `once`, `reject`, and — for a future caller that records a grant itself — `always`/`permanent`. An
// UNRECOGNISED STRING IS A REJECT: a typo upstream must never read as consent.
func resolveAskAnswer(decision string, opts []acp.PermissionOption) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "once", "allow", "approve", "allow_once", "yes":
		if id, ok := allowOnceOptionID(opts); ok {
			return id, true
		}
		return rejectFallback(opts), false
	case "always", "permanent", "user_permanent", "allow_always", "session":
		// `allow_always` IS PREFERRED AND USUALLY UNREACHABLE: agy 1.5.13 offers only allow/reject once
		// (docs §3.3), and the session grant lives in askorchicon's store, not on agy's wire. Handled
		// defensively so a future build that offers `allow_always` gets it right.
		if id, ok := allowAlwaysOptionID(opts); ok {
			return id, true
		}
		if id, ok := allowOnceOptionID(opts); ok {
			return id, true
		}
		return rejectFallback(opts), false
	default:
		return rejectFallback(opts), false
	}
}

// allowOnceOptionID picks the allow-once answer: the explicit KIND first, then an id that reads like one.
//
// THE KIND IS PREFERRED OVER THE ID because agy's option ids are its own spelling (`allow`, `proceed_once`,
// …) while the kind is ACP's vocabulary, and a build that renamed its ids would otherwise turn every
// approval into a reject.
func allowOnceOptionID(opts []acp.PermissionOption) (string, bool) {
	for _, o := range opts {
		if o.Kind == acp.OptionKindAllowOnce {
			return o.OptionID, true
		}
	}
	for _, o := range opts {
		if o.Kind == acp.OptionKindAllowAlways {
			return o.OptionID, true
		}
	}
	return acp.AllowOptionID(opts)
}

// allowAlwaysOptionID picks an `allow_always` option, or reports false when agy offers none.
func allowAlwaysOptionID(opts []acp.PermissionOption) (string, bool) {
	for _, o := range opts {
		if o.Kind == acp.OptionKindAllowAlways {
			return o.OptionID, true
		}
	}
	return "", false
}

// allowDecision answers a request that is not a consent decision.
func allowDecision(opts []acp.PermissionOption) acp.PermissionDecision {
	if id, ok := allowOnceOptionID(opts); ok {
		return acp.PermissionDecision{OptionID: id}
	}
	// NO ALLOW OFFERED AT ALL. A zero decision would be treated as a handler error (which fails closed to
	// reject anyway); saying so explicitly keeps the log honest about which path ran.
	return rejectDecision(opts)
}

// rejectDecision picks the reject option, falling back to an explicit `cancelled` when agy offered none —
// the same two-step `PolicyHandler.reject` performs for the worker population (permissions.go).
func rejectDecision(opts []acp.PermissionOption) acp.PermissionDecision {
	if id, ok := acp.RejectOptionID(opts); ok {
		return acp.PermissionDecision{OptionID: id}
	}
	return acp.PermissionDecision{Cancelled: true}
}

// rejectFallback is rejectDecision for the code paths that must return an option id string.
func rejectFallback(opts []acp.PermissionOption) string {
	if id, ok := acp.RejectOptionID(opts); ok {
		return id
	}
	// "" IS NOT AN ALLOW: the acp conn treats an unoffered/empty option id as a handler error and answers
	// with the reject option (or `cancelled`), which is exactly the intent.
	return ""
}

// askCommand is the card's Command field: agy's own command line, from the call's arguments.
func askCommand(input map[string]any) string {
	if cmd := toolInputCommand(input); cmd != "" {
		return cmd
	}
	// An MCP tool carries no command; its name is the subject and the UI shows Tool. A best-effort
	// fallback to a generic "command" key keeps an operator MCP server that models one from losing it.
	return strings.TrimSpace(firstString(input, "command", "cmd", "CommandLine", "commandLine"))
}

// askTargets lists what the card is ABOUT: the files an agy tool names, and the URL or query a web tool
// names. It mirrors claude's askTargets (internal/claude/ask.go) — same ordering rule, same de-duplication.
//
// THE KEYS ARE agy's OWN. `TargetFile` is `write_to_file`/`replace_file_content`, `AbsolutePath` is
// `view_file`, `SearchPath`/`Query` for the search tools; a card that showed no target for a write would be
// a card asking the operator to approve an unnamed file.
func askTargets(input map[string]any, locations []acp.Location) []string {
	seen := make(map[string]bool, 8)
	out := make([]string, 0, 8)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, k := range []string{
		"TargetFile", "AbsolutePath", "Path", "File", "FilePath", "SearchPath", "DirectoryPath",
		"Directory", "Url", "URL", "Uri", "query", "Query", "SearchQuery",
	} {
		add(firstString(input, k))
	}
	for _, l := range locations {
		add(l.Path)
	}
	return out
}

// askInputJSON renders the call's arguments as the JSON text the consent layer parses. "" for an empty map,
// so an absent input is not "{}" (claude's jsonString makes the same choice for the same reason: the
// consent layer distinguishes "no arguments" from "an empty object named nothing").
func askInputJSON(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(b)
}

// sessionIDSnapshot reads the agy session id under the lock.
func (s *askSession) sessionIDSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

// liveDone exposes the session's liveness context as a channel, so a parked handler is released by
// teardown. A nil context yields a nil channel, which blocks forever — correct, because a session with no
// live context has nothing that could release it except the request's own context.
func (s *askSession) liveDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveCtx == nil {
		return nil
	}
	return s.liveCtx.Done()
}
