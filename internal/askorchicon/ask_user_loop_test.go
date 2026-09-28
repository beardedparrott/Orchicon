package askorchicon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// busAskStart builds a NON-terminal tool part — the serve's "tool_part" signal
// (ToolStartFromBus maps state.status absent/"running"/"pending" to a start).
// This is what latches the stall monitor's open-tool clock.
func busAskStart(sessionID, tool string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type":  "tool",
				"tool":  tool,
				"state": map[string]any{"status": "running"},
			},
		},
	}
}

// busAskCompleted builds the resolution of a tool call with the arguments
// nested under state.input (the opencode v1.x shape recordResolve reads).
func busAskCompleted(sessionID, tool string, input map[string]any, output string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type": "tool",
				"tool": tool,
				"state": map[string]any{
					"status": "completed",
					"input":  input,
					"output": output,
				},
			},
		},
	}
}

// TestAskUserDoesNotWedgeTheStallMonitor is the deterministic half of the
// "returns immediately: no tool-call timeout, no stall-guard intervention" AC.
// It drives the REAL wedge mechanism (stall.go) with the injected clock seam: a
// record-and-return tool resolves in the same round, so the human reading the
// card for ten minutes cannot trip the wedge. The control proves the assertion
// is meaningful — a tool left open and silent DOES wedge.
func TestAskUserDoesNotWedgeTheStallMonitor(t *testing.T) {
	base := time.Now()

	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
	m.toolWedgeWindow = time.Minute
	m.now = func() time.Time { return base }
	// The model issues ask_user...
	m.observeToolStart("ask_user")
	// ...and it RESOLVES in the same round (the handler records and returns).
	m.observe("tool_use", map[string]any{
		"type": "tool", "tool": "ask_user",
		"state": map[string]any{"status": "completed"},
	})
	// The user now reads the card for ten minutes before answering — a pause
	// longer than the wedge window.
	m.now = func() time.Time { return base.Add(10 * time.Minute) }
	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("ask_user wedged the turn (tool %q) — a record-and-return tool must never trip the wedge guard", tool)
	}

	// CONTROL: the same clock advance on an UNRESOLVED tool must wedge, or the
	// assertion above would be vacuous.
	ctl := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
	ctl.toolWedgeWindow = time.Minute
	ctl.now = func() time.Time { return base }
	ctl.observeToolStart("bash")
	ctl.now = func() time.Time { return base.Add(10 * time.Minute) }
	if _, wedged := ctl.toolWedge(); !wedged {
		t.Fatal("control: a tool left open and silent must trip the wedge guard")
	}
}

// TestAskUserCallCollectedAndRecordedInLedger drives the collector through a
// full turn whose tool activity is an ask_user call, and asserts the turn ends
// COLLECTED (a completed turn — not pending, not wedged) and the live tool
// ledger — the object the finalize persists into tool_calls/tool_results —
// holds the call with its question and options intact.
func TestAskUserCallCollectedAndRecordedInLedger(t *testing.T) {
	client := &fakeSessionClient{}
	led := newToolLedger()
	args := map[string]any{
		"question": "Which branch should the run clone off?",
		"options": []any{
			map[string]any{"label": "develop", "description": "the integration branch"},
			map[string]any{"label": "main", "description": "the release branch"},
		},
	}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "cut me a branch",
		ledger: led,
	}
	go func() {
		waitForSend(t, client, 1)
		client.sub.feed(busAskStart("ses_live", "orchicon_ask_user"))
		client.sub.feed(busAskCompleted("ses_live", "orchicon_ask_user", args, `{"recorded":true}`))
		client.sub.feed(busIdle("ses_live"))
	}()

	reply, _, _, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("the turn with an unanswered ask_user must complete, got error: %v", err)
	}
	// The turn ended as a COMPLETED turn (collectTurn only returns on a terminal
	// attempt): no wedge, no stall failure. The reply may be a short line.
	_ = reply

	calls, results := led.snapshot()
	var parsedCalls []struct {
		ID           string `json:"id"`
		FunctionName string `json:"function_name"`
		Arguments    string `json:"arguments"`
	}
	if err := json.Unmarshal(calls, &parsedCalls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v", err)
	}
	if len(parsedCalls) != 1 || parsedCalls[0].FunctionName != "orchicon_ask_user" {
		t.Fatalf("tool_calls = %s, want exactly one orchicon_ask_user call", calls)
	}
	var gotArgs struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(parsedCalls[0].Arguments), &gotArgs); err != nil {
		t.Fatalf("arguments are not the recorded call object (%q): %v", parsedCalls[0].Arguments, err)
	}
	if !strings.Contains(gotArgs.Question, "clone off") {
		t.Errorf("recorded question = %q, want it intact", gotArgs.Question)
	}
	if len(gotArgs.Options) != 2 || gotArgs.Options[0].Label != "develop" || gotArgs.Options[1].Label != "main" {
		t.Errorf("recorded options = %+v, want develop + main intact", gotArgs.Options)
	}
	// The result records {recorded:true} — the model's own view of the call.
	if !strings.Contains(string(results), "recorded") {
		t.Errorf("tool_results = %s, want the recorded:true result", results)
	}
}
