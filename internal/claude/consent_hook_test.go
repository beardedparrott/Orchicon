package claude

// consent_hook_test.go — the card's delivery path.
//
// WHAT THIS PINS, and why it needed a new mechanism at all. MEASURED against the real CLI (2.1.289) in
// this adapter's launch shape:
//
//	--permission-prompts host + permissions.ask  ->  0 `can_use_tool` frames, `system permission_denied`
//	a PreToolUse verdict of "ask"                ->  a tool ERROR, not a card
//	a PermissionRequest hook returning allow     ->  the call RUNS
//
// So a PreToolUse verdict and the permission system's own ask are both TERMINAL, and the only surface that
// can gate a call on a human decision is a `PermissionRequest` hook — which cannot abstain, so it must
// block while the operator reads. These tests cover that loop end to end within the adapter, plus every
// failure path, because a permission channel that could fail OPEN would be worse than no card.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// consentHarness is a session serving its consent socket, with a subscribed bus so the test can see the
// ask the adapter raises.
func consentHarness(t *testing.T) (*Bridge, *askSession, scheduler.SessionBus) {
	t.Helper()
	h, _ := askHarness(t)
	ctx := context.Background()
	s := h.b.ensureAskSession("conv-consent")
	bus, err := h.b.Subscribe(ctx, "conv-consent")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(bus.Close)
	// The session id and its binding are what ReplyPermissionDecision resolves the session BY, exactly as
	// CreateConversationSession sets them — without them the answer RPC cannot find the ask to settle.
	s.mu.Lock()
	s.sid = uuid.NewString()
	s.bus = bus.(*askBus)
	s.liveCtx, s.liveCancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	h.b.bindAskSessionID(s.sid, s)
	t.Cleanup(func() { s.stopConsentSocket() })
	if err := s.serveConsentSocket(); err != nil {
		t.Fatalf("serveConsentSocket: %v", err)
	}
	if !s.AsksOverConsentSocket() {
		t.Fatal("the socket is not listening — a real hook would fail closed and the operator would see nothing")
	}
	return h.b, s, bus
}

// askOverSocket plays the hook's half: dial, send one request, read the reply.
func askOverSocket(t *testing.T, sock, tool string, input map[string]any) hookConsentReply {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the consent socket: %v", err)
	}
	defer conn.Close()
	raw, _ := json.Marshal(input)
	req, _ := json.Marshal(hookConsentRequest{Tool: tool, Input: raw})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	var reply hookConsentReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("parse the reply %q: %v", line, err)
	}
	return reply
}

// AN APPROVED CALL COMES BACK "allow" — the whole point of the mechanism.
//
// It also pins that the ask reaches the collector: the adapter must emit the SAME `permission` bus event
// the can_use_tool path emits, or the precedence chain, the grants and the card would all be bypassed and
// the operator would be approving something else.
func TestConsentSocketAllowsWhenTheOperatorApproves(t *testing.T) {
	b, s, bus := consentHarness(t)

	type result struct{ reply hookConsentReply }
	done := make(chan result, 1)
	go func() {
		done <- result{askOverSocket(t, s.consentSockPath(), "Bash", map[string]any{"command": "git status"})}
	}()

	ev := nextEvent(t, bus)
	if ev.Kind != "permission" {
		t.Fatalf("the adapter emitted %q, want a permission event — without it no card is raised", ev.Kind)
	}
	if ev.Tool != "Bash" {
		t.Errorf("the ask names tool %q, want Bash", ev.Tool)
	}
	if ev.Command != "git status" {
		t.Errorf("the ask's command is %q, want the call's own command — the card describes the real call", ev.Command)
	}
	if ev.PermissionID == "" {
		t.Fatal("the ask carries no id, so the operator's answer can never be correlated to it")
	}

	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case got := <-done:
		if got.reply.Behavior != "allow" {
			t.Fatalf("the hook was told %q, want allow — the operator approved and the call would still be refused", got.reply.Behavior)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never got an answer — it would sit until the CLI killed it while the operator believed they had approved")
	}
}

// A REFUSAL COMES BACK "deny", with a message the model can act on.
func TestConsentSocketDeniesWhenTheOperatorRefuses(t *testing.T) {
	b, s, bus := consentHarness(t)

	done := make(chan hookConsentReply, 1)
	go func() { done <- askOverSocket(t, s.consentSockPath(), "Write", map[string]any{"file_path": "/tmp/x"}) }()

	ev := nextEvent(t, bus)
	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "reject"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case reply := <-done:
		if reply.Behavior != "deny" {
			t.Fatalf("the hook was told %q, want deny", reply.Behavior)
		}
		if strings.TrimSpace(reply.Message) == "" {
			t.Error("a refusal carried no message — the model is left with a bare refusal and no way to proceed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never got the refusal")
	}
}

// NO ANSWER IS A DENIAL. The window closes, and the call does not run.
func TestConsentSocketDeniesWhenNobodyAnswers(t *testing.T) {
	t.Setenv("ORCHICON_CLAUDE_CONSENT_WAIT", "150ms")
	_, s, _ := consentHarness(t)

	reply := askOverSocket(t, s.consentSockPath(), "Bash", map[string]any{"command": "ls"})
	if reply.Behavior != "deny" {
		t.Fatalf("an unanswered ask returned %q, want deny — a permission that can fail OPEN is worse than no card", reply.Behavior)
	}
}

// NO BOUND BY DEFAULT, and the CLI's own timeout must not become the new cliff.
//
// The two halves of this are one decision: this side waits for the operator indefinitely (the same
// default as the native adapter — a card must not be taken from someone who stepped away), and the
// `timeout` written into the settings document is a BACKSTOP rather than a leash. It used to be 16
// minutes — one minute above a 15-minute consent wait — which, with the wait unbounded, would make the
// CLI the thing that kills the hook at sixteen minutes instead of fifteen: the same cliff, moved.
func TestConsentWaitIsUnboundedByDefaultAndTheHookTimeoutIsNotACliff(t *testing.T) {
	t.Setenv("ORCHICON_CLAUDE_CONSENT_WAIT", "") // the default: unset
	if got := hookConsentWait(); got != 0 {
		t.Fatalf("hookConsentWait() = %s with nothing configured, want no bound — the claude transport "+
			"would refuse calls the native one still allows, which is the divergence these two "+
			"constants exist to prevent", got)
	}
	// The settings-document timeout must leave room for an operator's absence, not for a minute more
	// than a timer that no longer exists.
	if AskHookTimeoutSeconds/3600 < 24 {
		t.Fatalf("the PermissionRequest hook timeout is %ds — with the adapter-side wait unbounded, a "+
			"short CLI timeout simply RELOCATES the expiry that took the card away", AskHookTimeoutSeconds)
	}
}

// THE HOOK FAILS CLOSED when it cannot reach the adapter at all: no channel, a dead socket, or a malformed
// reply must each DENY, and must still write a decision so the CLI is not left to its own devices.
func TestPermissionRequestHookFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"no channel configured", ""},
		{"a channel that does not exist", filepath.Join(t.TempDir(), "nope.sock")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			code := runPermissionRequestHook(
				HookInput{HookEventName: "PermissionRequest", ToolName: "Bash", ToolInput: map[string]any{"command": "rm -rf /"}},
				func(k string) string {
					if k == ConsentSockEnv {
						return tc.env
					}
					return ""
				},
				&out,
			)
			if code != 0 {
				t.Fatalf("exit %d, want 0 — a hook that exits non-zero leaves the CLI to its own flow", code)
			}
			var doc struct {
				HookSpecificOutput struct {
					HookEventName string `json:"hookEventName"`
					Decision      *struct {
						Behavior string `json:"behavior"`
						Message  string `json:"message"`
					} `json:"decision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
				t.Fatalf("unparseable hook output %q: %v", out.String(), err)
			}
			if doc.HookSpecificOutput.HookEventName != "PermissionRequest" {
				t.Errorf("hookEventName = %q, want PermissionRequest", doc.HookSpecificOutput.HookEventName)
			}
			if doc.HookSpecificOutput.Decision == nil {
				t.Fatal("no decision was written — the CLI decides alone on a call nobody approved")
			}
			if doc.HookSpecificOutput.Decision.Behavior != "deny" {
				t.Fatalf("behavior = %q, want deny — a permission interface must never fail open", doc.HookSpecificOutput.Decision.Behavior)
			}
		})
	}
}

// AND THE HOOK SAYS "allow" ON THE WIRE when the adapter approves — the byte shape the CLI was measured to
// honour, checked through the real hook function rather than by hand.
func TestPermissionRequestHookAllowsOnApproval(t *testing.T) {
	_, s, bus := consentHarness(t)

	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		runPermissionRequestHook(
			HookInput{HookEventName: "PermissionRequest", ToolName: "Bash", ToolInput: map[string]any{"command": "git log"}},
			func(k string) string {
				if k == ConsentSockEnv {
					return s.consentSockPath()
				}
				return ""
			},
			&out,
		)
		done <- out.String()
	}()

	ev := nextEvent(t, bus)
	if err := s.b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case got := <-done:
		if !strings.Contains(got, `"behavior":"allow"`) {
			t.Fatalf("the hook wrote %q, want a PermissionRequest allow — this exact shape was measured to run the call", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never returned")
	}
}

// A HOOK ASK MUST NOT BE ANSWERED WITH A can_use_tool FRAME. The CLI is waiting on the hook, not on a
// control request, so writing a frame would answer nothing and the operator's approval would be silently
// lost. This pins that ReplyPermissionDecision routes a parked hook ask down the socket instead.
func TestReplyPermissionDecisionRoutesHookAsksNotControlFrames(t *testing.T) {
	b, s, bus := consentHarness(t)

	done := make(chan hookConsentReply, 1)
	go func() { done <- askOverSocket(t, s.consentSockPath(), "Bash", map[string]any{"command": "ls"}) }()

	ev := nextEvent(t, bus)
	if !strings.HasPrefix(ev.PermissionID, "hook-") {
		t.Fatalf("the ask id %q is not a hook ask id — the routing cannot tell the two apart", ev.PermissionID)
	}
	// The session has no live child, so a control frame COULD not be written: if the decision were routed
	// the old way this would error, and the hook would hang.
	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case reply := <-done:
		if reply.Behavior != "allow" {
			t.Fatalf("hook reply = %q, want allow", reply.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hook ask was not resolved through the socket")
	}
	// And it is consumed: a second reply for the same id must not be mistaken for a parked hook ask.
	if w := s.takeHookWait(ev.PermissionID); w != nil {
		t.Error("the hook wait was not consumed by the decision — a second reply could resolve it twice")
	}
}

// THE SETTINGS DOCUMENT REGISTERS THE CARD'S ONLY DELIVERY PATH, and the matcher is DERIVED from the tools
// the permission system prompts on. A name in one list and not the other is a call the permission system
// asks about with no hook to answer it — a terminal denial, which is the defect this whole change closes.
func TestAskProfileRegistersThePermissionRequestHook(t *testing.T) {
	raw, err := BuildSettings(PermissionOptions{Profile: ProfileAskEnvValue, HookBinary: "/bin/orchicon"})
	if err != nil || raw == "" {
		t.Fatalf("BuildSettings: %v", err)
	}
	var doc struct {
		Permissions map[string]any `json:"permissions"`
		Hooks       map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the settings document is not valid JSON: %v", err)
	}
	entries, ok := doc.Hooks["PermissionRequest"]
	if !ok || len(entries) == 0 {
		t.Fatal("the Ask profile registers NO PermissionRequest hook — the permission system would ask and nothing would answer, which is the terminal denial the operator sees as 'you haven't granted it yet'")
	}
	matcher := entries[0].Matcher
	for _, tool := range AskPermissionToolNames {
		if !strings.Contains(matcher, tool) {
			t.Errorf("the PermissionRequest matcher %q does not cover %q, which permissions.ask prompts on — that call would be denied with no card", matcher, tool)
		}
	}
	if len(entries[0].Hooks) == 0 || entries[0].Hooks[0].Command == "" {
		t.Fatal("the PermissionRequest entry names no command")
	}
	// THE TIMEOUT MUST OUTLAST THE WAIT, or the CLI kills the hook mid-read while the operator is looking
	// at the card.
	if got := time.Duration(entries[0].Hooks[0].Timeout) * time.Second; got <= hookConsentWait() {
		t.Errorf("the hook timeout is %s, which does not exceed the consent wait %s — the CLI would kill the hook before we could answer it", got, hookConsentWait())
	}
	// AND THE WORKER PROFILE GETS NONE: it is non-interactive, so a blocking hook there could only refuse.
	worker, err := BuildSettings(PermissionOptions{Profile: ProfileWorkerEnvValue, HookBinary: "/bin/orchicon"})
	if err != nil {
		t.Fatalf("worker BuildSettings: %v", err)
	}
	if strings.Contains(worker, "PermissionRequest") {
		t.Error("the WORKER profile registers a PermissionRequest hook — a headless worker has nobody to answer it")
	}
}

// Teardown removes the socket, so a later hook cannot find a listener that answers nothing (this session is
// retired) and block on a card that will never be raised.
func TestTeardownRemovesTheConsentSocket(t *testing.T) {
	_, s, _ := consentHarness(t)
	path := s.consentSockPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the socket is not there to begin with: %v", err)
	}
	s.stopConsentSocket()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket survived teardown (%v) — a hook could find it and block", err)
	}
	if s.AsksOverConsentSocket() {
		t.Error("the session still reports a live consent socket after teardown")
	}
}
