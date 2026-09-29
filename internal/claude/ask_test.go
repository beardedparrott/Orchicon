package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askHarness wires a Bridge that spawns the shared fakeProc, so the whole Ask
// transport runs in-process with no real Anthropic spend.
func askHarness(t *testing.T) (*harness, **fakeProc) {
	t.Helper()
	var current *fakeProc
	h := newHarness(t, func() *fakeProc {
		current = newFakeProc()
		return current
	})
	h.b.SetAskRoot(t.TempDir())
	// Hand back a pointer-to-pointer so a test can read the proc the transport
	// spawned without the factory's assign-on-call being lost.
	slot := &current
	return h, slot
}

// nextEvent reads one event from the bus with a deadline, so a mapper that stops
// emitting fails the test instead of hanging it.
func nextEvent(t *testing.T, bus scheduler.SessionBus) scheduler.SessionEvent {
	t.Helper()
	select {
	case ev, ok := <-bus.Events():
		if !ok {
			t.Fatal("the bus closed before delivering an event")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a session event")
	}
	return scheduler.SessionEvent{}
}

// The three-tier picker reads Dispatcher.ChatKinds(), which type-asserts the
// bridge. This is the assertion the whole Ask path hinges on, so it is pinned
// rather than left to a compile-time var that a refactor could delete quietly.
func TestBridgeImplementsAskChatCapability(t *testing.T) {
	var b any = New(quietLogger())
	if _, ok := b.(scheduler.ChatTurnClient); !ok {
		t.Fatal("the claude bridge does not implement scheduler.ChatTurnClient — claude would register but never be offered for Ask")
	}
	if _, ok := b.(scheduler.SessionOwnerKind); !ok {
		t.Fatal("the claude bridge does not implement SessionOwnerKind — a foreign session id could be dispatched to it")
	}
}

// The launch must pin the session identity and install the interactive profile.
// Without `--session-id` the adapter cannot name the session it created; without
// `--permission-prompts host` the CLI auto-denies anything that would prompt and
// no consent card is ever raised.
func TestAskLaunchPinsSessionAndTheInteractiveProfile(t *testing.T) {
	h, _ := askHarness(t)
	ctx := context.Background()

	sid, err := h.b.CreateConversationSession(ctx, "conv-argv", "title")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if strings.TrimSpace(sid) == "" {
		t.Fatal("CreateConversationSession returned an empty session id")
	}

	bus, err := h.b.Subscribe(ctx, "conv-argv")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bus.Close()

	if err := h.b.SendTurnMessage(ctx, "conv-argv", sid, "you are a test", "claude/anthropic/claude-sonnet-4", "hello"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}

	argv := strings.Join(h.argv, " ")
	for _, want := range []string{
		"--session-id " + sid,
		"--permission-prompts host",
		"--input-format stream-json",
		"--output-format stream-json",
		"--model claude-sonnet-4",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("ask argv is missing %q:\n  %s", want, argv)
		}
	}
	if strings.Contains(argv, "bypass") {
		t.Errorf("ask argv carries a bypass token: %s", argv)
	}
}

// The env the child inherits is what selects the HOOK's rule set. If the profile
// var is missing the session silently runs the WORKER rule set, where nothing is
// ever asked.
//
// TWO PROFILES, NAMED APART ON PURPOSE. This one is the HOOK's (a PreToolUse rule
// set: allow / deny / ask) and it IS interactive. The GUARD's profile is a
// different thing entirely and is deliberately NOT interactive (see
// TestAskChildEnvCarriesTheDefaultProfileExecutionGuard). Conflating the two is
// the mistake that produced a shim which could only widen the sanctioned set.
func TestAskChildEnvSelectsTheInteractiveHookProfile(t *testing.T) {
	s := newAskSession(New(quietLogger()), "conv-env", "/tmp/orchicon-ask/conv-env")
	env := strings.Join(s.childEnv(), "\n")
	for _, want := range []string{
		HookProfileEnv + "=" + ProfileAskEnvValue,
		AskDirEnv + "=" + "/tmp/orchicon-ask/conv-env",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("ask child env is missing %q", want)
		}
	}
}

// THE CONSENT PATH, end to end at this adapter's seam: a can_use_tool line becomes
// a typed permission event whose PermissionID is the id a reply must echo, and
// the reply lands on the child's stdin as the exact control_response frame.
func TestAskRaisesATypedPermissionEventAndAnswersIt(t *testing.T) {
	h, slot := askHarness(t)
	ctx := context.Background()

	sid, err := h.b.CreateConversationSession(ctx, "conv-ask", "t")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	bus, err := h.b.Subscribe(ctx, "conv-ask")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bus.Close()
	if err := h.b.SendTurnMessage(ctx, "conv-ask", sid, "", "claude/anthropic/claude-sonnet-4", "run a command"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	fp := *slot
	if fp == nil {
		t.Fatal("the transport did not spawn a child")
	}

	fp.push(`{"type":"control_request","request_id":"req_777","request":{` +
		`"subtype":"can_use_tool","tool_name":"Bash",` +
		`"input":{"command":"rm -rf /tmp/x"},"blocked_path":"/tmp/x"}}`)

	ev := nextEvent(t, bus)
	if ev.Kind != "permission" {
		t.Fatalf("event kind = %q, want permission", ev.Kind)
	}
	if ev.PermissionID != "req_777" {
		t.Fatalf("PermissionID = %q, want req_777 — the reply would have no id to echo", ev.PermissionID)
	}
	// The TYPED fields, not opencode's property vocabulary: the contract was
	// extended so an adapter can name the action directly.
	if ev.Tool != "Bash" {
		t.Errorf("Tool = %q, want Bash", ev.Tool)
	}
	if ev.Command != "rm -rf /tmp/x" {
		t.Errorf("Command = %q, want the shell line the card must show", ev.Command)
	}
	if len(ev.Targets) == 0 || ev.Targets[0] != "/tmp/x" {
		t.Errorf("Targets = %v, want the blocked path", ev.Targets)
	}
	if !strings.Contains(ev.InputJSON, "rm -rf /tmp/x") {
		t.Errorf("InputJSON = %q, want the call's arguments", ev.InputJSON)
	}

	// The operator approves ONCE. The frame must be the nesting the CLI matches.
	if err := h.b.ReplyPermissionDecision(ctx, sid, "req_777", "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	var found string
	for _, turn := range fp.turnsSnapshot() {
		if strings.Contains(turn, "control_response") {
			found = turn
		}
	}
	if found == "" {
		t.Fatal("no control_response reached the child — the CLI would stay parked on the ask")
	}
	var doc struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(found), &doc); err != nil {
		t.Fatalf("control frame is not valid JSON (%q): %v", found, err)
	}
	if doc.Response.RequestID != "req_777" {
		t.Errorf("response.request_id = %q, want req_777", doc.Response.RequestID)
	}
	if doc.Response.Response.Behavior != BehaviorAllow {
		t.Errorf("behavior = %q, want allow for an approval", doc.Response.Response.Behavior)
	}
}

// A rejection must reach the child as a DENY with the required message.
func TestAskRejectionDeniesWithAMessage(t *testing.T) {
	h, slot := askHarness(t)
	ctx := context.Background()
	sid, _ := h.b.CreateConversationSession(ctx, "conv-reject", "t")
	bus, _ := h.b.Subscribe(ctx, "conv-reject")
	defer bus.Close()
	_ = h.b.SendTurnMessage(ctx, "conv-reject", sid, "", "claude/anthropic/claude-sonnet-4", "x")
	fp := *slot

	fp.push(`{"type":"control_request","request_id":"req_9","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/tmp/y"}}}`)
	if ev := nextEvent(t, bus); ev.PermissionID != "req_9" {
		t.Fatalf("PermissionID = %q, want req_9", ev.PermissionID)
	}
	if err := h.b.ReplyPermissionDecision(ctx, sid, "req_9", "reject"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}

	var found string
	for _, turn := range fp.turnsSnapshot() {
		if strings.Contains(turn, "control_response") {
			found = turn
		}
	}
	if !strings.Contains(found, `"behavior":"deny"`) {
		t.Fatalf("rejection frame = %q, want behavior deny", found)
	}
	if !strings.Contains(found, `"message":`) {
		t.Fatalf("rejection frame = %q; the CLI's schema REQUIRES a message on deny", found)
	}
}

// Ending a turn must emit `idle`, or the collector drains forever on a turn that
// already finished.
func TestAskResultEndsTheTurnWithIdle(t *testing.T) {
	h, slot := askHarness(t)
	ctx := context.Background()
	sid, _ := h.b.CreateConversationSession(ctx, "conv-idle", "t")
	bus, _ := h.b.Subscribe(ctx, "conv-idle")
	defer bus.Close()
	_ = h.b.SendTurnMessage(ctx, "conv-idle", sid, "", "claude/anthropic/claude-sonnet-4", "x")

	(*slot).push(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)
	if ev := nextEvent(t, bus); ev.Kind != "part" || ev.Text != "hi" {
		t.Fatalf("assistant text mapped to %+v, want a text part", ev)
	}
	(*slot).push(`{"type":"result","subtype":"success","result":"done"}`)
	if ev := nextEvent(t, bus); ev.Kind != "idle" {
		t.Fatalf("result mapped to kind %q, want idle", ev.Kind)
	}
}

// An abort must retire a parked ask, not leave the card on screen. The CLI
// documents a cancel as the way an in-flight can_use_tool is settled.
func TestAskAbortSettlesAPendingAsk(t *testing.T) {
	h, slot := askHarness(t)
	ctx := context.Background()
	sid, _ := h.b.CreateConversationSession(ctx, "conv-abort", "t")
	bus, _ := h.b.Subscribe(ctx, "conv-abort")
	defer bus.Close()
	_ = h.b.SendTurnMessage(ctx, "conv-abort", sid, "", "claude/anthropic/claude-sonnet-4", "x")
	fp := *slot

	fp.push(`{"type":"control_request","request_id":"req_park","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if ev := nextEvent(t, bus); ev.PermissionID != "req_park" {
		t.Fatalf("PermissionID = %q, want req_park", ev.PermissionID)
	}

	if err := h.b.AbortConversationSession(ctx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}
	var sawCancel bool
	for _, turn := range fp.turnsSnapshot() {
		if strings.Contains(turn, `"behavior":"cancelled"`) && strings.Contains(turn, "req_park") {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatal("abort did not settle the parked ask — the card would outlive the turn")
	}
	if got := fp.signals(); len(got) == 0 || got[0] != "INT" {
		t.Errorf("abort signals = %v, want INT first (an interrupt keeps the session resumable)", got)
	}
}

// An unknown session is a safe no-op for abort (idempotent) but a LOUD failure
// for a reply: answering an ask that has no live session cannot be silently
// dropped, or the card would look answered while the CLI stayed parked.
func TestAskAbortUnknownIsQuietButReplyIsLoud(t *testing.T) {
	b := New(quietLogger())
	ctx := context.Background()
	if err := b.AbortConversationSession(ctx, "nope"); err != nil {
		t.Errorf("AbortConversationSession(unknown) = %v, want nil (abort is idempotent)", err)
	}
	if err := b.ReplyPermissionDecision(ctx, "nope", "req", "once"); err == nil {
		t.Error("ReplyPermissionDecision(unknown session) = nil, want an actionable error")
	}
}

// THE PARITY PIN. Both other adapters put the OS-level execution shim on the Ask
// path (opencode's host serve, the native bridge's bash environ). Claude briefly
// did not, which made it the only Ask transport where `a subprocess did it` was
// unguarded — the class that carries the never-allow binaries and the protected
// roots, neither of which the PreToolUse hook can see.
//
// Two things are asserted, and both are load-bearing:
//  1. the shim DIRECTORY is first on PATH, so any process the session spawns
//     resolves `rm`/`sudo`/`dd` through it;
//  2. the INTERACTIVE guard vars are ABSENT, matching opencode's Ask serve. The
//     interactive profile can only WIDEN the sanctioned set (grants, once-targets,
//     policy accepts, fullsend) and it fail-closes on a missing policy file,
//     which would refuse every path-scoped command in every Ask conversation on a
//     plane with no policy at permpolicy.DefaultPath().
func TestAskChildEnvCarriesTheDefaultProfileExecutionGuard(t *testing.T) {
	s := newAskSession(New(quietLogger()), "conv-guard", filepath.Join(t.TempDir(), "ask"))
	s.ensureGuard()
	if s.guard == nil {
		t.Skip("the execution guard could not be built in this environment")
	}
	defer s.guard.Close()

	env := s.childEnv()

	var pathEntry string
	var sawInteractive bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			pathEntry = strings.TrimPrefix(kv, "PATH=")
		case strings.HasPrefix(kv, "ORCHICON_GUARD_POLICY="):
			sawInteractive = true
		}
	}
	if pathEntry == "" {
		t.Fatal("the Ask child env has no PATH entry")
	}
	first := strings.Split(pathEntry, string(os.PathListSeparator))[0]
	if !strings.Contains(first, "orchicon-guard-") {
		t.Fatalf("PATH[0] = %q, want the guard shim dir first — a spawned process would resolve `rm` directly", first)
	}
	if sawInteractive {
		t.Fatal("the Ask shim is in the INTERACTIVE profile: it can only widen the sanctioned set, and it fail-closes on a missing policy file — refusing every path-scoped command where opencode's Ask serve refuses nothing extra")
	}
}

// THE SCOPE PIN. An empty project dir is not "no guard" — it is the mode where
// blocked_path refuses EVERY absolute target outside the Orchicon scratch dir and
// the operator's accept list. That is what opencode's host serve and the native
// bridge run, and it is what this must match.
//
// The bug this pins: the shim allows an absolute target INSIDE its project dir,
// so scoping the guard to the conversation's directory made `rm -rf
// <abs path in the ask dir>` legitimate — a WIDER sanctioned set than both
// references, justified by a claim ("one conversation, one directory") that
// describes the session's shape rather than containing it.
//
// Asserted BEHAVIOURALLY: run the shim's own `rm` against an absolute path inside
// the ask directory and require a refusal. A future change that re-scopes the
// guard turns this red.
func TestAskGuardRefusesAnAbsolutePathInTheAskDir(t *testing.T) {
	askDir := filepath.Join(t.TempDir(), "ask")
	if err := os.MkdirAll(filepath.Join(askDir, "scratch"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := newAskSession(New(quietLogger()), "conv-scope", askDir)
	s.ensureGuard()
	if s.guard == nil {
		t.Skip("the execution guard could not be built in this environment")
	}
	defer s.guard.Close()

	env := s.childEnv()

	// The shim dir is PATH[0]; run the shimmed `rm` directly.
	var pathEntry string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathEntry = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if pathEntry == "" {
		t.Fatal("no PATH entry in the ask child env")
	}
	shimRm := filepath.Join(strings.Split(pathEntry, string(os.PathListSeparator))[0], "rm")

	target := filepath.Join(askDir, "scratch", "gone")
	cmd := exec.Command(shimRm, "-rf", target)
	cmd.Env = env
	cmd.Dir = askDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the shim ALLOWED `rm -rf %s` — an absolute target inside the ask dir must be refused (the ask dir is a cwd, not a sanctioned scope); output=%q", target, out)
	}
	if !strings.Contains(strings.ToUpper(string(out)), "ORCHICON GUARD") {
		t.Fatalf("refusal did not name the guard: %q", out)
	}

	// ...while a RELATIVE cleanup still works, which is why the empty scope costs
	// the session nothing: it manages its own working directory the ordinary way.
	rel := exec.Command(shimRm, "-rf", "scratch")
	rel.Env = env
	rel.Dir = askDir
	if out, err := rel.CombinedOutput(); err != nil {
		t.Fatalf("a relative cleanup was refused (%v): %q", err, out)
	}
}
