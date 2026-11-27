package antigravity

// askpermission_test.go — AC2: the permission CARD on the real wire.
//
// THE FAKE CHILD ASKS, the bridge's handler decides, and the test reads the option id that actually went back
// over the pipe. That is deliberately different from askmodegate_test.go, which drives the handler directly:
// here the whole stack runs — conn, handler, bus, reply RPC — because the failure this catches is "the handler
// decided correctly and the answer never reached agy".
//
// EVERY DENIAL CASE ASSERTS THE REJECT ID, not merely "not allowed": agy offered `deny` and the wire must carry
// `deny`. An empty or unrecognised answer would also not run the tool, but it would look to the model like a
// transport failure rather than a decision.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskPermission_RunCommandRaisesCard is T1: one card, with agy's own fields, and it BLOCKS.
func TestAskPermission_RunCommandRaisesCard(t *testing.T) {
	h := newAskHarness(t)
	h.installAskPermission(runCommandAsk())
	h.autoAnswer("once")
	h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "list the directory")

	// THE CARD.
	events := h.bus.permissionEvents()
	if len(events) != 1 {
		t.Fatalf("permission events = %d, want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Tool != "run_command" {
		t.Errorf("card Tool = %q, want the raw agy name %q", ev.Tool, "run_command")
	}
	if ev.Command != "ls" {
		t.Errorf("card Command = %q, want %q", ev.Command, "ls")
	}
	if ev.PermissionID == "" {
		t.Error("card PermissionID is empty — the reply RPC cannot correlate an answer to it")
	}
	if !strings.Contains(ev.InputJSON, "CommandLine") {
		t.Errorf("card InputJSON = %q, want the call's arguments", ev.InputJSON)
	}

	// AND THE ALLOW WENT BACK, because the turn could not have finished otherwise: the fake blocks on the
	// client's answer before it emits its updates (that is what askPermissionFor does).
	if got := h.procAt(0).answeredOptionID(0); got != "allow" {
		t.Errorf("agy received optionId %q, want the allow option", got)
	}
}

// TestAskPermission_AnswerOnceSendsAllowOption is T2: every spelling of "yes" that the consent layer may send
// resolves to the option agy actually offered.
func TestAskPermission_AnswerOnceSendsAllowOption(t *testing.T) {
	for _, decision := range []string{"once", "allow", "approve", "once"} {
		t.Run(decision, func(t *testing.T) {
			h := newAskHarness(t)
			h.installAskPermission(runCommandAsk())
			h.queueAnswer(decision)
			h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "go")
			if got, want := h.procAt(0).answeredOptionID(0), "allow"; got != want {
				t.Errorf("decision %q answered optionId %q, want %q", decision, got, want)
			}
		})
	}
}

// TestAskPermission_AlwaysPrefersAllowAlwaysWhenOffered documents the defensive branch: agy 1.5.13 offers only
// allow_once, so "always" must fall back to it — but a future build that offers allow_always must get THAT.
func TestAskPermission_AlwaysPrefersAllowAlwaysWhenOffered(t *testing.T) {
	t.Run("only allow_once offered", func(t *testing.T) {
		h := newAskHarness(t)
		h.installAskPermission(runCommandAsk())
		h.queueAnswer("always")
		h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "go")
		if got, want := h.procAt(0).answeredOptionID(0), "allow"; got != want {
			t.Errorf("answered optionId %q, want the allow_once fallback %q", got, want)
		}
	})
	t.Run("allow_always offered", func(t *testing.T) {
		h := newAskHarness(t)
		h.installAskPermission(func(int) *permissionAsk {
			return &permissionAsk{
				ToolCall: acp.ToolCallInfo{
					ToolCallID: "call_ls", Name: "run_command", Kind: "execute",
					RawInput: mustJSON(map[string]any{"CommandLine": "ls"}),
				},
				Options: []acp.PermissionOption{
					{OptionID: "allow", Name: "Allow", Kind: acp.OptionKindAllowOnce},
					{OptionID: "allow_all", Name: "Always allow", Kind: acp.OptionKindAllowAlways},
					{OptionID: "deny", Name: "Deny", Kind: acp.OptionKindRejectOnce},
				},
			}
		})
		h.queueAnswer("always")
		h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "go")
		if got, want := h.procAt(0).answeredOptionID(0), "allow_all"; got != want {
			t.Errorf("answered optionId %q, want the allow_always option %q", got, want)
		}
	})
}

// TestAskPermission_AnswerDenySendsRejectOption is T3. EVERY non-allow string — including one this adapter does
// not recognise — must send the reject id. An unrecognised decision is never consent.
func TestAskPermission_AnswerDenySendsRejectOption(t *testing.T) {
	for _, decision := range []string{
		"reject", "deny", "denied", "garbage", "", "refused: the target is outside the project",
	} {
		t.Run("decision="+decision, func(t *testing.T) {
			h := newAskHarness(t)
			h.installAskPermission(runCommandAsk())
			h.queueAnswer(decision)
			h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "go")
			if got, want := h.procAt(0).answeredOptionID(0), "deny"; got != want {
				t.Errorf("decision %q answered optionId %q, want the reject option %q", decision, got, want)
			}
		})
	}
}

// TestAskPermission_TeardownWhileWaitingRejects is T4: the module is torn down while a card is parked, and the
// wire still gets a REJECT — not a silent close, and not an allow.
func TestAskPermission_TeardownWhileWaitingRejects(t *testing.T) {
	h := newAskHarness(t)
	h.installAskPermission(runCommandAsk())
	// NO answer is queued: the operator never replies, and the plane shuts down underneath them.
	h.startTurn(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "go")

	waitFor(t, "the permission card", func() bool { return len(h.bus.permissionEvents()) == 1 })

	h.b.CloseAsk()

	// THE CHILD SAW A REJECT. The fake records the client's answer as it arrives, so this is the wire itself.
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if s := h.procAt(0).answeredOptionID(0); s != "" {
			got = s
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got != "deny" {
		t.Errorf("teardown answered optionId %q, want the reject option %q (fail closed)", got, "deny")
	}
	// AND NO CARD IS LEFT PARKED.
	if n := h.pendingAsks("conv-test"); n != 0 {
		t.Errorf("%d asks are still parked after teardown — a leaked goroutine per cancelled card", n)
	}
}

// TestAskPermission_AutoAllowsReadsAndOrchiconMCP is T5: a read or the platform's own MCP tool is NOT a consent
// decision, and raises zero cards.
func TestAskPermission_AutoAllowsReadsAndOrchiconMCP(t *testing.T) {
	for _, tc := range []struct{ raw, canonical string }{
		{"view_file", "read"},
		{"grep_search", "grep"},
		{"list_dir", "ls"},
		{"mcp_orchicon_orchicon_ask_user", "mcp_orchicon_orchicon_ask_user"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			h := newAskHarness(t)
			h.installAskPermission(func(int) *permissionAsk {
				return &permissionAsk{
					ToolCall: acp.ToolCallInfo{
						ToolCallID: "call_" + tc.raw, Name: tc.raw,
						RawInput: mustJSON(map[string]any{"AbsolutePath": "/p/project/a.txt"}),
					},
					Options: defaultPermissionOptions(),
				}
			})
			h.turnOnce(t, scopeCtx(askmode.Iteration, "conv-test", "prj_1"), "look")

			if n := len(h.bus.permissionEvents()); n != 0 {
				t.Fatalf("%s raised %d cards — the Ask profile cards only bash/write/edit/websearch/webfetch", tc.raw, n)
			}
			if got := h.procAt(0).answeredOptionID(0); got != "allow" {
				t.Errorf("%s answered optionId %q, want allow", tc.raw, got)
			}
		})
	}
}

// TestAskPermission_BrainstormRefusesWithoutACard is AC3 on the wire: Brainstorm refuses a write, agy gets the
// reject id, and the operator is never asked.
func TestAskPermission_BrainstormRefusesWithoutACard(t *testing.T) {
	h := newAskHarness(t)
	h.installAskPermission(func(int) *permissionAsk {
		return &permissionAsk{
			ToolCall: acp.ToolCallInfo{
				ToolCallID: "call_w", Name: "write_to_file", Kind: "edit",
				RawInput: mustJSON(map[string]any{"TargetFile": "/p/project/a.txt", "CodeContent": "hi"}),
			},
			Options: defaultPermissionOptions(),
		}
	})
	// AN ANSWER IS QUEUED BUT MUST NEVER BE USED: if a card were raised, the answer would allow the write, and
	// the reject assertion below would fail loudly.
	h.queueAnswer("once")
	h.turnOnce(t, scopeCtx(askmode.Brainstorm, "conv-test", "prj_1"), "write it")

	if n := len(h.bus.permissionEvents()); n != 0 {
		t.Fatalf("Brainstorm raised %d permission cards for a write — a refused tool must never reach the operator", n)
	}
	if got := h.procAt(0).answeredOptionID(0); got != "deny" {
		t.Errorf("Brainstorm answered optionId %q, want the reject option", got)
	}
	if !h.transcriptHasRefusal() {
		t.Error("no refusal reached the transcript — the model retries a call it cannot see the reason for")
	}
}

// --- driving helpers -------------------------------------------------------------------------------

// runCommandAsk is the recorded `run_command` permission request.
func runCommandAsk() func(int) *permissionAsk {
	return func(int) *permissionAsk {
		return &permissionAsk{
			ToolCall: acp.ToolCallInfo{
				ToolCallID: "call_ls",
				Name:       "run_command",
				Kind:       "execute",
				RawInput:   mustJSON(map[string]any{"CommandLine": "ls"}),
			},
			Options: defaultPermissionOptions(),
		}
	}
}

// installAskPermission scripts the child to ask one permission request before its turn's updates.
func (h *askHarness) installAskPermission(ask func(int) *permissionAsk) {
	h.serve(script{
		askPermission: ask,
		updates:       []string{updateMessage("done")},
	})
}

// queueAnswer phases the harness so the NEXT raised card is answered with decision, without a goroutine.
//
// THE ANSWER IS APPLIED BY THE CARD-POLLING BUS RECORDER, which is a test-only property: the card is raised deep
// inside the turn's prompt, so the test's own goroutine cannot be the one that answers it.
func (h *askHarness) queueAnswer(decision string) {
	h.mu.Lock()
	h.nextDecision = decision
	h.mu.Unlock()
}

// autoAnswer is queueAnswer's readable name at a call site that expects the card to be answered.
func (h *askHarness) autoAnswer(decision string) { h.queueAnswer(decision) }

// answerNextFromQueue is called by the bus recorder whenever a permission event arrives.
func (h *askHarness) answerNextFromQueue(convID, permissionID string) {
	h.mu.Lock()
	decision := h.nextDecision
	h.mu.Unlock()
	if decision == "" {
		return
	}
	if ask := h.b.ensureAskSession(convID).takeAsk(permissionID); ask != nil {
		select {
		case ask.decision <- decision:
		default:
		}
	}
}

// pendingAsks reports how many cards are parked on a conversation.
func (h *askHarness) pendingAsks(convID string) int {
	return h.b.ensureAskSession(convID).pendingAsks()
}

// startTurn begins one turn and returns immediately (the caller polls the bus).
func (h *askHarness) startTurn(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	sid, err := h.b.CreateConversationSession(ctx, "conv-test", "t")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if err := h.b.SendTurnMessage(ctx, "conv-test", sid, "", "antigravity/google/"+harnessModel, text); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
}

// turnOnce runs a whole turn: create the session, send one message, and wait for the terminal event.
func (h *askHarness) turnOnce(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	h.startTurn(t, ctx, text)
	waitFor(t, "the turn to end", func() bool {
		for _, e := range h.bus.all() {
			if e.Kind == "idle" || e.Kind == "error" {
				return true
			}
		}
		return false
	})
	for _, e := range h.bus.all() {
		if e.Kind == "error" {
			t.Fatalf("the turn failed: %s", e.Text)
		}
	}
}
