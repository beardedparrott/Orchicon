package claude

// consent_hook.go — the ADAPTER half of the Ask consent card.
//
// WHY THIS EXISTS, and why it is a socket rather than a verdict.
//
// The card never had a delivery path. MEASURED against the real CLI (2.1.289) in this adapter's own
// launch shape (`-p --input-format stream-json`, stdin held open, `permissions.ask: ["Bash"]`):
//
//	--permission-prompts host  ->  0 `can_use_tool` frames, `system permission_denied Bash`
//
// The CLI's own documentation says why: "'ask' decisions are terminal" unless the host presents a
// permission-prompt surface ("bare -p / SDK query() with no canUseTool"). So a `PreToolUse` hook
// returning `permissionDecision: "ask"` becomes a tool ERROR (#666's measurement), and the permission
// system's ask becomes a terminal DENIAL — the operator's "Claude requested permissions to use Bash,
// but you haven't granted it yet". No card could ever appear, whatever our hook said.
//
// The one surface that DOES work is the `PermissionRequest` hook: it fires (same measurement), and its
// output IS the decision — returning `{"behavior":"allow"}` ran the command. But its vocabulary is
// allow/deny with no "ask" arm, so the HOOK must decide. A hook is a blocking subprocess, which is
// exactly the shape a card needs: it holds the call open until the operator answers.
//
// WHAT IT LACKS IS A CHANNEL. The hook is a separate process; the card is raised in this one, on the
// session's bus. This file is that channel — a per-session unix socket. The hook asks, this side raises
// the card through the EXISTING consent path (the same `permission` bus event the can_use_tool path
// emits, so the precedence chain, the grants, the deny list and the card are unchanged), and the
// operator's answer goes back down the socket.
//
// FAIL CLOSED, ALWAYS. Every failure here — no socket, no answer, a teardown — ends in a DENY. A
// permission interface that could fail OPEN would be worse than no card at all, so the hook independently
// denies when it cannot reach this side (see RunHook), and this side denies on any error.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ConsentSockEnv is how the hook learns where to ask. It is set on the child's environment beside the
// mode file, because both are per-conversation facts the hook cannot otherwise discover.
const ConsentSockEnv = "ORCHICON_CLAUDE_CONSENT_SOCK"

// hookConsentWaitDefault bounds how long an ask may sit before it is DENIED.
//
// THE SAME 15 MINUTES THE NATIVE ADAPTER WAITS (internal/orchicon: nativeConsentWaitDefault), because
// the two are the same human decision and a shorter leash here would make the claude transport refuse
// calls the native one still allows. The hook's own `timeout` in the settings document is set ABOVE this
// (see AskHookTimeoutSeconds) so this side expires first and the hook still exits cleanly with a deny,
// rather than being killed mid-write.
const hookConsentWaitDefault = 15 * time.Minute

// AskHookTimeoutSeconds is the `PermissionRequest` hook's timeout in the settings document. It must
// EXCEED hookConsentWaitDefault, or the CLI kills the hook before this side can answer and the operator
// loses the card they were reading.
const AskHookTimeoutSeconds = 960 // 16 minutes

// askHookToolMatcher matches the tools the Ask profile sends to a card. It is DERIVED from
// AskPermissionToolNames — the same list the permission system prompts on — so the hooks cannot fire for
// a different set than the one that raises the ask. A tool named there but absent here would be prompted
// by the permission system with no hook to answer it: a terminal denial, which is the defect this file
// exists to close.
//
// It is a REGEX (claude matches hook matchers as regexes), so the names are alternated verbatim.
func askHookToolMatcher() string {
	return strings.Join(AskPermissionToolNames, "|")
}

// hookConsentRequest is one ask, hook -> adapter.
type hookConsentRequest struct {
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// hookConsentReply is the decision, adapter -> hook.
type hookConsentReply struct {
	Behavior string `json:"behavior"` // "allow" | "deny"
	Message  string `json:"message,omitempty"`
}

// hookAsk is one parked hook ask: the channel its decision arrives on.
type hookAsk struct {
	decision chan string
	tool     string
}

// consentSockDir is where consent sockets live: a SHORT, per-user directory, deliberately NOT under the
// ask directory.
//
// A unix socket path is capped near 108 bytes (`sun_path`), and `bind` fails with EINVAL past it — the
// ask directory cannot be used for this, because its length is NOT OURS: `ORCHICON_CLAUDE_ASK_ROOT` is an
// operator setting and a test harness's temp dir is ~120 bytes by itself. Deriving the socket from it made
// bind fail, and the failure mode is the quiet one that matters: the hook fails CLOSED, so the operator
// loses the card and the call is refused, with only a log line to say why.
//
// The per-user subdirectory is what keeps a SHARED /tmp safe: the socket is owner-only (0600) inside a
// 0700 directory, so another user cannot connect to it and answer a permission on this operator's behalf.
func consentSockDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("orchicon-consent-%d", os.Getuid()))
}

// consentSockPath is this session's socket: a FIXED-LENGTH name derived from the conversation id, so the
// path is the same length for every conversation whatever the id contains.
func (s *askSession) consentSockPath() string {
	sum := sha256.Sum256([]byte(s.convID))
	return filepath.Join(consentSockDir(), fmt.Sprintf("%x.sock", sum[:8]))
}

// serveConsentSocket starts the listener. Safe to call more than once: a live listener is reused, so a
// respawn does not leave the hook pointing at a dead socket.
func (s *askSession) serveConsentSocket() error {
	s.hookMu.Lock()
	if s.hookLn != nil {
		s.hookMu.Unlock()
		return nil
	}
	s.hookMu.Unlock()

	dir := consentSockDir()
	// 0700: the directory is the first half of the socket's access control (the socket itself is 0600), and
	// it lives in a world-writable temp dir.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("claude ask: consent socket dir: %w", err)
	}
	path := s.consentSockPath()
	// A socket left by a previous process would make Listen fail with "address already in use"; it is
	// ours by construction (same conversation), and the live listener check above already ruled out a
	// live one here.
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("claude ask: consent socket listen: %w", err)
	}
	// The socket is the consent surface: owner-only, beside the session's own 0755 directory.
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Default().Warn("claude ask: could not restrict the consent socket permissions", "error", err)
	}

	s.hookMu.Lock()
	s.hookLn = ln
	s.hookMu.Unlock()

	go s.acceptConsentConns(ln)
	return nil
}

// stopConsentSocket closes the listener and unlinks it. Called from teardown, so a retired session
// leaves no socket for a hook to find and block on.
func (s *askSession) stopConsentSocket() {
	s.hookMu.Lock()
	ln := s.hookLn
	s.hookLn = nil
	s.hookMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	_ = os.Remove(s.consentSockPath())
}

// acceptConsentConns serves asks until the listener closes.
//
// ONE CONNECTION PER ASK, each handled on its own goroutine: the hook connects, sends one request, blocks
// for one reply, and disconnects. That keeps the protocol trivial and means a hook killed mid-ask cannot
// disturb the next one.
func (s *askSession) acceptConsentConns(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // closed by stopConsentSocket
		}
		go s.serveConsentConn(conn)
	}
}

func (s *askSession) serveConsentConn(conn net.Conn) {
	defer conn.Close()

	// A HARD READ DEADLINE, so a hook that connects and says nothing cannot pin this goroutine. It is
	// generous because the hook writes its request immediately.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		slog.Default().Warn("claude ask: could not read the consent request from the hook", "error", err)
		writeConsentReply(conn, hookConsentReply{Behavior: "deny", Message: "the consent request could not be read"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var req hookConsentRequest
	if err := json.Unmarshal(line, &req); err != nil {
		slog.Default().Warn("claude ask: malformed consent request from the hook", "error", err)
		writeConsentReply(conn, hookConsentReply{Behavior: "deny", Message: "the consent request could not be parsed"})
		return
	}
	if strings.TrimSpace(req.Tool) == "" {
		writeConsentReply(conn, hookConsentReply{Behavior: "deny", Message: "the consent request named no tool"})
		return
	}

	decision, ok := s.awaitHookConsent(req)
	if !ok {
		writeConsentReply(conn, hookConsentReply{
			Behavior: "deny",
			Message:  "this call was not approved (no answer arrived before the window closed), so it did not run. Nothing is permanently denied: retry it and it will ask again.",
		})
		return
	}
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "once", "allow", "approve", "always", "permanent", "user_permanent":
		writeConsentReply(conn, hookConsentReply{Behavior: "allow"})
	default:
		writeConsentReply(conn, hookConsentReply{
			Behavior: "deny",
			Message:  "the operator refused this call, so it did not run. Do not retry it; ask what they would prefer instead.",
		})
	}
}

func writeConsentReply(conn net.Conn, reply hookConsentReply) {
	b, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write(append(b, '\n'))
}

// awaitHookConsent parks an ask and blocks for the operator's decision.
//
// IT RAISES THE CARD THROUGH THE SAME BUS EVENT THE can_use_tool PATH EMITS, deliberately: the collector's
// precedence chain (never-allow, the policy, the grants, FULLSEND), the card's wording and the reply RPC
// all key off that event, so a hook ask and a control-request ask are the same ask as far as every other
// layer is concerned. This is why the change is a TRANSPORT, not a second consent implementation.
//
// ok is false when no decision arrived — the window, a teardown, or the turn ending — and the caller
// turns that into a DENY.
func (s *askSession) awaitHookConsent(req hookConsentRequest) (string, bool) {
	askID := "hook-" + s.nextHookAskID()

	w := &hookAsk{decision: make(chan string, 1), tool: req.Tool}
	s.hookMu.Lock()
	if s.hookWaits == nil {
		s.hookWaits = make(map[string]*hookAsk)
	}
	s.hookWaits[askID] = w
	s.hookMu.Unlock()
	defer func() {
		s.hookMu.Lock()
		delete(s.hookWaits, askID)
		s.hookMu.Unlock()
	}()

	// The event carries the ARGUMENTS, so the card's target and command come from the SAME extraction the
	// can_use_tool path uses — one description of an ask, not two that can drift.
	input := map[string]any{}
	if len(req.Input) > 0 {
		_ = json.Unmarshal(req.Input, &input)
	}
	ev := StreamEvent{ControlToolName: req.Tool, ControlInput: input}
	s.busEmit(hookPermissionEvent(askID, req.Tool, ev, req.Input))

	// The wait is bounded by BOTH the consent window and the session's own lifetime: a teardown must
	// release the hook rather than leave it blocked until the CLI kills it.
	ctx := s.liveCtx
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(hookConsentWait())
	defer timer.Stop()

	select {
	case d := <-w.decision:
		return d, true
	case <-timer.C:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// hookPermissionEvent builds the `permission` bus event for a hook ask, in the SAME shape emitAsk
// produces for a can_use_tool request.
func hookPermissionEvent(askID, tool string, ev StreamEvent, rawInput json.RawMessage) scheduler.SessionEvent {
	input := jsonString(ev.ControlInput)
	if strings.TrimSpace(input) == "" || input == "null" {
		input = string(rawInput)
	}
	return scheduler.SessionEvent{
		Kind:         "permission",
		PermissionID: askID,
		Tool:         tool,
		Command:      stringInput(ev.ControlInput, "command"),
		Targets:      askTargets(ev),
		InputJSON:    input,
	}
}

// takeHookWait removes and returns a parked hook ask, if the id names one.
//
// ReplyPermissionDecision consults this FIRST: a hook ask is answered by writing a decision down the
// socket, not by a control_response frame (the CLI is not parked on one — it is waiting on the hook).
// Getting that wrong would send a frame for a request id the CLI never issued, and the hook would sit
// until it timed out with the operator believing they had approved the call.
func (s *askSession) takeHookWait(permissionID string) *hookAsk {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	w, ok := s.hookWaits[permissionID]
	if !ok {
		return nil
	}
	delete(s.hookWaits, permissionID)
	return w
}

// nextHookAskID mints an ask id unique within this session.
func (s *askSession) nextHookAskID() string {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.hookAskSeq++
	return fmt.Sprintf("%d", s.hookAskSeq)
}

// hookConsentWait resolves the window, with an env override for a test (and for an operator who wants a
// shorter leash).
func hookConsentWait() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_CLAUDE_CONSENT_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return hookConsentWaitDefault
}

// AsksOverConsentSocket reports whether this session is serving the consent socket, for a test that must
// not silently pass when nothing was listening.
func (s *askSession) AsksOverConsentSocket() bool {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.hookLn != nil
}
