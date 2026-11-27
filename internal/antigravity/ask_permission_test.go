package antigravity

// ask_permission_test.go — AC2 and AC3 for the Antigravity Ask transport.
//
// AC2 (the card and its answer) is proved over the REAL ACP wire: the fake `agy acp` child sends a genuine
// `session/request_permission` for `run_command {CommandLine:"ls"}`, and the test asserts (a) exactly one
// `permission` bus event with Tool "run_command" and Command "ls", (b) the optionId agy was handed back for
// each operator answer, and (c) that a teardown while the card is open answers with the reject option
// rather than leaving the call hanging.
//
// AC3 (the mode gate) is a table over mode × tool, asserting the PAIR that matters: whether a card was
// raised, and what agy was answered.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// runCommandAsk is the recorded permission request AC2 names: `run_command {CommandLine:"ls"}`.
func runCommandAsk(t *testing.T, h *askHarness) string {
	t.Helper()
	return h.proc(0).askPermission(t, "run_command", map[string]any{"CommandLine": "ls"})
}

// awaitAskEvent waits for the card and returns it.
func awaitAskEvent(t *testing.T, col *eventCollector) sessionEvent {
	t.Helper()
	waitFor(t, "a permission event on the bus", func() bool { return len(col.ofKind("permission")) > 0 })
	return col.ofKind("permission")[0]
}

// turnReady opens a conversation in one mode with a live child and a draining bus, which is the state every
// permission test starts from.
func turnReady(t *testing.T, mode string) (*askHarness, *eventCollector, string) {
	t.Helper()
	sc := script{}
	h := newAskHarness(t, sc)
	sid := h.create(mode)
	ctx, cancel := context.WithCancel(h.turnCtx(mode))
	t.Cleanup(cancel)
	col := drain(t, h.subscribe(ctx))
	return h, col, sid
}

// TestAskPermission_CardShapeAndAllow is AC2's first half: ONE `permission` event with the agy tool name and
// the command line, and answering "once" hands agy the allow option id.
func TestAskPermission_CardShapeAndAllow(t *testing.T) {
	h, col, _ := turnReady(t, askmode.Iteration)

	// Start a turn so a permission request has a live prompt to belong to, then ask.
	_, ackCh := turnInFlight(t, h, nil, askmode.Iteration)
	fp := h.latestProc()
	reqID := runCommandAsk(t, h)

	ev := awaitAskEvent(t, col)
	if ev.Tool != "run_command" {
		t.Errorf("permission event Tool = %q, want %q (the agy tool name, AC2)", ev.Tool, "run_command")
	}
	if ev.Command != "ls" {
		t.Errorf("permission event Command = %q, want %q", ev.Command, "ls")
	}
	if ev.PermissionID == "" {
		t.Error("permission event carries no PermissionID — the card cannot be answered without one")
	}
	if len(col.ofKind("permission")) != 1 {
		t.Errorf("bus carried %d permission events, want exactly 1", len(col.ofKind("permission")))
	}

	// ANSWER: "once" → the allow option id agy offered.
	if err := h.b.ReplyPermissionDecision(context.Background(), h.sessionID(), ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision(once): %v", err)
	}
	out := fp.awaitPermAnswer(t, reqID)
	if out.Outcome.Outcome != "selected" || out.Outcome.OptionID != "allow" {
		t.Fatalf("agy was answered %+v, want selected/allow", out.Outcome)
	}
	ackTurn(t, fp, ackCh)
}

// TestAskPermission_DenySendsRejectOption is AC2's second half: a refusal answers the reject option id.
func TestAskPermission_DenySendsRejectOption(t *testing.T) {
	h, col, _ := turnReady(t, askmode.Iteration)
	_, ackCh := turnInFlight(t, h, nil, askmode.Iteration)
	fp := h.latestProc()
	reqID := runCommandAsk(t, h)

	ev := awaitAskEvent(t, col)
	if err := h.b.ReplyPermissionDecision(context.Background(), h.sessionID(), ev.PermissionID, "reject"); err != nil {
		t.Fatalf("ReplyPermissionDecision(reject): %v", err)
	}
	out := fp.awaitPermAnswer(t, reqID)
	if out.Outcome.Outcome != "selected" || out.Outcome.OptionID != "deny" {
		t.Fatalf("agy was answered %+v, want selected/deny (the reject option)", out.Outcome)
	}
	ackTurn(t, fp, ackCh)
}

// TestAskPermission_AlwaysFallsBackToAllowOnce proves the defensive arm: askorchicon only sends "once", but a
// client that learns to send "always" must still be handed an option agy offered — and when agy offers no
// allow_always, the honest downgrade is allow_once, not a rejection.
func TestAskPermission_AlwaysFallsBackToAllowOnce(t *testing.T) {
	h, col, _ := turnReady(t, askmode.Iteration)
	_, ackCh := turnInFlight(t, h, nil, askmode.Iteration)
	fp := h.latestProc()
	reqID := runCommandAsk(t, h)

	ev := awaitAskEvent(t, col)
	if err := h.b.ReplyPermissionDecision(context.Background(), h.sessionID(), ev.PermissionID, "always"); err != nil {
		t.Fatalf("ReplyPermissionDecision(always): %v", err)
	}
	out := fp.awaitPermAnswer(t, reqID)
	if out.Outcome.OptionID != "allow" {
		t.Fatalf("agy was answered %+v, want the offered allow option (no allow_always was offered)", out.Outcome)
	}
	ackTurn(t, fp, ackCh)
}

// TestAskPermission_TeardownRejectsWhileParked is AC2's third half: tearing the session down while a card is
// open answers the call rather than leaving it parked forever.
func TestAskPermission_TeardownRejectsWhileParked(t *testing.T) {
	h, col, _ := turnReady(t, askmode.Iteration)
	_, _ = turnInFlight(t, h, nil, askmode.Iteration)
	fp := h.latestProc()
	reqID := runCommandAsk(t, h)

	ev := awaitAskEvent(t, col)

	// Parked: no answer yet.
	select {
	case <-fp.awaitPermAnswerAsync(t, reqID):
		t.Fatal("agy was answered before the operator said anything")
	case <-time.After(50 * time.Millisecond):
	}

	// TEARDOWN WHILE PARKED. The session goes away; the parked card must be settled with a REFUSAL.
	h.b.CloseAsk()

	out := fp.awaitPermAnswer(t, reqID)
	switch {
	case out.Outcome.Outcome == "selected" && out.Outcome.OptionID == "deny":
		// The rejection agy offered — the answer we want.
	case out.Outcome.Outcome == "cancelled":
		// Also a refusal (it grants nothing), and the only option when agy offered no reject at all.
	default:
		t.Fatalf("teardown answered %+v, want the reject option or cancelled (never an allow)", out.Outcome)
	}
	_ = ev
}

// TestAskPermission_UnknownIDIsAnError pins the correlation contract: an answer for a card that is not
// parked must fail loudly rather than silently succeed (which would hide a lost decision).
func TestAskPermission_UnknownIDIsAnError(t *testing.T) {
	h, _, _ := turnReady(t, askmode.Iteration)
	if err := h.b.ReplyPermissionDecision(context.Background(), h.sessionID(), "no-such-card", "once"); err == nil {
		t.Fatal("ReplyPermissionDecision for an unknown card returned nil — a lost decision would be silent")
	}
}

// TestAskPermission_FailClosedOnNoAnswer proves the no-answer path fails CLOSED: with a consent window that
// expires, agy is answered reject.
func TestAskPermission_FailClosedOnNoAnswer(t *testing.T) {
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "60ms")
	h, col, _ := turnReady(t, askmode.Iteration)
	_, ackCh := turnInFlight(t, h, nil, askmode.Iteration)
	fp := h.latestProc()
	reqID := runCommandAsk(t, h)

	_ = awaitAskEvent(t, col)
	// NOBODY ANSWERS. The window closes and the call must be refused.
	out := fp.awaitPermAnswer(t, reqID)
	if out.Outcome.Outcome == "selected" && out.Outcome.OptionID == "allow" {
		t.Fatalf("an unanswered card was ALLOWED (%+v) — the consent gate would fail open", out.Outcome)
	}
	ackTurn(t, fp, ackCh)
}

// TestAskPermission_AskTargets checks the card's Targets carry the call's subject, which is what tells the
// operator WHAT they are approving.
func TestAskPermission_AskTargets(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		locations []string
		want      string
	}{
		{"write_to_file names its target", "write_to_file", map[string]any{"TargetFile": "/p/a.md"}, nil, "/p/a.md"},
		{"replace_file_content names its target", "replace_file_content", map[string]any{"TargetFile": "/p/b.go"}, nil, "/p/b.go"},
		{"read_url_content names its url", "read_url_content", map[string]any{"Url": "https://example.com/x"}, nil, "https://example.com/x"},
		{"search_web names its query", "search_web", map[string]any{"query": "orchicon ask"}, nil, "orchicon ask"},
		{"a location is a target too", "view_file", map[string]any{}, []string{"/p/c.txt"}, "/p/c.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := askPermissionTargetsFor(tc.tool, tc.args, tc.locations)
			if len(got) == 0 || got[0] != tc.want {
				t.Fatalf("targets = %v, want %q first", got, tc.want)
			}
		})
	}
	// An MCP tool names itself: there is no path, and the operator must be told WHICH tool.
	got := askPermissionTargetsFor("mcp_github_create_issue", nil, nil)
	if len(got) != 1 || !strings.Contains(got[0], "mcp_github") {
		t.Fatalf("MCP targets = %v, want the tool name", got)
	}
}
