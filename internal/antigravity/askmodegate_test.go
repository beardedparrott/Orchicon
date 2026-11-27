package antigravity

// askmodegate_test.go — AC3: the mode boundary table, and the "no card on a refusal" invariant.
//
// THE ASSERTION THAT MATTERS MOST IS THE NEGATIVE ONE. A gate that refuses a tool but STILL raises a card
// puts the operator in front of a question whose "yes" the platform will not honour — worse than no gate,
// because it looks like the boundary was offered a choice. So each denial case asserts BOTH the reject option
// on the wire AND that zero permission events reached the bus.

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskModeGate_Table is AC3.
func TestAskModeGate_Table(t *testing.T) {
	const projectDir = "/p/project"
	writeArgs := map[string]any{"TargetFile": projectDir + "/a.txt", "CodeContent": "hi"}

	type row struct {
		name        string
		mode        string
		rawName     string
		canonical   string
		input       map[string]any
		wantDenied  bool
		wantCard    bool
		wantTargets []string
	}

	rows := []row{
		// --- Brainstorm: the work tools are REFUSED, without a card. ---
		// The denial set comes from askmode itself (see TestAskModeGateFollowsThePolicyTable), never from a
		// literal repeated here, so this table cannot drift from the policy.
		{name: "brainstorm write_to_file", mode: askmode.Brainstorm,
			rawName: "write_to_file", canonical: "write", input: writeArgs, wantDenied: true},
		{name: "brainstorm replace_file_content", mode: askmode.Brainstorm,
			rawName: "replace_file_content", canonical: "edit",
			input: map[string]any{"TargetFile": projectDir + "/a.txt"}, wantDenied: true},
		{name: "brainstorm multi_replace_file_content", mode: askmode.Brainstorm,
			rawName: "multi_replace_file_content", canonical: "edit",
			input: map[string]any{"TargetFile": projectDir + "/a.txt"}, wantDenied: true},
		{name: "brainstorm run_command", mode: askmode.Brainstorm,
			rawName: "run_command", canonical: "bash",
			input: map[string]any{"CommandLine": "ls"}, wantDenied: true},
		// The platform's OWN sidecar must not be a way around the gate.
		{name: "brainstorm mcp orchicon bash", mode: askmode.Brainstorm,
			rawName: "mcp_orchicon_orchicon_bash", input: map[string]any{"command": "ls"}, wantDenied: true},
		{name: "brainstorm mcp orchicon write", mode: askmode.Brainstorm,
			rawName: "mcp_orchicon_orchicon_write", input: writeArgs, wantDenied: true},

		// --- Brainstorm: reads PROCEED, and a read raises no card. ---
		{name: "brainstorm view_file", mode: askmode.Brainstorm,
			rawName: "view_file", canonical: "read",
			input: map[string]any{"AbsolutePath": projectDir + "/a.txt"}},
		{name: "brainstorm grep_search", mode: askmode.Brainstorm,
			rawName: "grep_search", canonical: "grep", input: map[string]any{"query": "hi"}},

		// --- Iteration: a write INSIDE the project raises exactly ONE card naming the file. ---
		{name: "iteration write inside project", mode: askmode.Iteration,
			rawName: "write_to_file", canonical: "write", input: writeArgs,
			wantCard: true, wantTargets: []string{projectDir + "/a.txt"}},
		{name: "iteration run_command", mode: askmode.Iteration,
			rawName: "run_command", canonical: "bash",
			input: map[string]any{"CommandLine": "ls"}, wantCard: true},

		// --- Quick Work denies the work tools too (the same policy table): it DISPATCHES rather than acts. ---
		{name: "quick_work run_command", mode: askmode.QuickWork,
			rawName: "run_command", canonical: "bash",
			input: map[string]any{"CommandLine": "ls"}, wantDenied: true},
		{name: "quick_work write_to_file", mode: askmode.QuickWork,
			rawName: "write_to_file", canonical: "write", input: writeArgs, wantDenied: true},
		// ...but the platform's own dispatch tools are exactly what it is FOR, so they are allowed.
		{name: "quick_work platform dispatch", mode: askmode.QuickWork,
			rawName: "mcp_orchicon_orchicon_create_work_item",
			input: map[string]any{"title": "x"}},

		// --- The opaque MCP rule: denied where the mode may not act. ---
		{name: "brainstorm opaque mcp", mode: askmode.Brainstorm,
			rawName: "mcp_github_create_issue", input: map[string]any{}, wantDenied: true},
		// AN OPAQUE MCP TOOL IS NOT A CARDED TOOL even where the mode may act: the card set is the claude Ask
		// profile's (bash/write/edit/websearch/webfetch), and a third-party server's own approval flow is not
		// this platform's question to ask.
		{name: "iteration opaque mcp", mode: askmode.Iteration,
			rawName: "mcp_github_create_issue", input: map[string]any{}},
		{name: "brainstorm platform mcp read", mode: askmode.Brainstorm,
			rawName: "mcp_orchicon_orchicon_list_projects", input: map[string]any{}},
		// BRAINSTORM AUTHORING WORK IS ALLOWED, and that is the policy's own split, not an oversight:
		// Brainstorm denies theWorkTools (write/edit/bash) and nothing else, so writing a plan AS A WORK ITEM
		// is exactly what it is for. Quick Work denies the same set and is the mode that DISPATCHES.
		{name: "brainstorm platform mcp dispatch", mode: askmode.Brainstorm,
			rawName: "mcp_orchicon_orchicon_create_work_item", input: map[string]any{"title": "x"}},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			h := newAskHarness(t)
			h.askDir(projectDir)

			// CARDS BLOCK BY DESIGN (no timeout), so a carded row is answered here; every other row returns on
			// its own.
			var res askDecision
			if r.wantCard {
				res = h.permissionAnswered(t, modeCtx(r.mode), r.rawName, r.canonical, r.input, "once")
			} else {
				res = h.permission(t, modeCtx(r.mode), r.rawName, r.canonical, r.input)
			}

			// THE MODE GATE'S VERDICT, computed the same way the handler computes it.
			denied, refusal := askModeGate(r.mode, r.rawName, r.canonical)
			if denied != r.wantDenied {
				t.Fatalf("askModeGate(%q, %q) denied=%v, want %v", r.mode, r.rawName, denied, r.wantDenied)
			}
			if denied {
				for _, want := range []string{"REFUSED BY THE PLATFORM", r.mode, "ASK THE USER"} {
					if !strings.Contains(refusal, want) {
						t.Errorf("the refusal is missing %q: %s", want, refusal)
					}
				}
			}

			events := h.bus.permissionEvents()
			if r.wantCard {
				if len(events) != 1 {
					t.Fatalf("permission events = %d, want exactly 1: %+v", len(events), events)
				}
				if r.wantTargets != nil {
					for _, want := range r.wantTargets {
						if !containsStr(events[0].Targets, want) {
							t.Errorf("card targets = %v, want %q among them", events[0].Targets, want)
						}
					}
				}
				if r.rawName != "" && events[0].Tool != r.rawName {
					t.Errorf("card Tool = %q, want the RAW agy name %q", events[0].Tool, r.rawName)
				}
				return
			}

			// NO CARD. Ever.
			if len(events) != 0 {
				t.Fatalf("a card was raised for %+v — a refused tool must never be put to the operator, and an "+
					"auto-allowed one must never be asked about", events)
			}
			if denied {
				// AND THE REJECT OPTION WENT TO AGY, so the tool really did not run.
				if got, want := res.optionID, "deny"; got != want {
					t.Errorf("denial answered optionId %q, want %q", got, want)
				}
				// AND THE REFUSAL TEXT REACHED THE TRANSCRIPT, because agy gets no reason over ACP.
				if !h.transcriptHasRefusal() {
					t.Error("a refused tool emitted no tool_result carrying the refusal — the operator and the " +
						"model are both left with no explanation")
				}
			}
		})
	}
}

// TestAskModeGateFollowsThePolicyTable is the anti-drift half: the table above must agree with askmode's OWN
// denial set, so widening or narrowing the policy cannot leave this adapter behind.
func TestAskModeGateFollowsThePolicyTable(t *testing.T) {
	for _, mode := range []string{askmode.Brainstorm, askmode.QuickWork, askmode.Iteration} {
		for _, name := range askmode.DeniedNames(mode) {
			denied, _ := askModeGate(mode, name, name)
			if !denied {
				t.Errorf("askModeGate(%q, %q) allowed a tool the policy DENIES", mode, name)
			}
		}
	}
	// And Iteration denies nothing, so no canonical work tool is gated there.
	for _, name := range []string{"write", "edit", "bash"} {
		if denied, _ := askModeGate(askmode.Iteration, name, name); denied {
			t.Errorf("askModeGate(iteration, %q) denied — iteration denies nothing", name)
		}
	}
}

// TestAskModeGateAllowsAnUnstampedMode documents the empty-mode answer: no mode means the policy's own
// answer, which is ALLOW, and the gate must not invent a stricter boundary than the platform's.
func TestAskModeGateAllowsAnUnstampedMode(t *testing.T) {
	if denied, _ := askModeGate("", "write_to_file", "write"); denied {
		t.Error("an unstamped mode denied a write — the gate invented a boundary the policy does not have")
	}
}

// TestAskPermissionAutoAllowsReadsAndOrchiconMCP is T5: the card set is exactly the tools claude's Ask
// profile cards, and a read/orchicon-MCP tool is allowed with ZERO permission events.
func TestAskPermissionAutoAllowsReadsAndOrchiconMCP(t *testing.T) {
	for _, tc := range []struct{ raw, canonical string }{
		{"view_file", "read"},
		{"grep_search", "grep"},
		{"list_dir", "ls"},
		{"mcp_orchicon_orchicon_ask_user", "mcp_orchicon_orchicon_ask_user"},
		{"mcp_orchicon_orchicon_get_current_conversation", "mcp_orchicon_orchicon_get_current_conversation"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			h := newAskHarness(t)
			h.askDir(t.TempDir())
			res := h.permission(t, modeCtx(askmode.Iteration), tc.raw, tc.canonical,
				map[string]any{"AbsolutePath": "/p/project/a.txt"})
			if len(h.bus.permissionEvents()) != 0 {
				t.Fatalf("%s raised a card — the Ask profile cards only bash/write/edit/websearch/webfetch", tc.raw)
			}
			if got := res.optionID; got != "allow" {
				t.Errorf("%s answered optionId %q, want allow", tc.raw, got)
			}
		})
	}
}

// TestAskPermissionBlocksUntilAnswered is T1: a carded call emits ONE event with agy's own fields and then
// BLOCKS — there is no timeout, because a human decision is what it waits on.
func TestAskPermissionBlocksUntilAnswered(t *testing.T) {
	h := newAskHarness(t)
	h.askDir(t.TempDir())

	done := h.permissionAsync(t, modeCtx(askmode.Iteration), "run_command", "bash",
		map[string]any{"CommandLine": "ls"})

	waitFor(t, "the permission card", func() bool { return len(h.bus.permissionEvents()) == 1 })
	ev := h.bus.permissionEvents()[0]
	if ev.Tool != "run_command" {
		t.Errorf("card Tool = %q, want the raw agy name %q", ev.Tool, "run_command")
	}
	if ev.Command != "ls" {
		t.Errorf("card Command = %q, want %q", ev.Command, "ls")
	}
	if ev.PermissionID == "" {
		t.Error("card PermissionID is empty — the reply cannot be correlated")
	}
	if !strings.Contains(ev.InputJSON, "CommandLine") {
		t.Errorf("card InputJSON = %q, want the call's arguments", ev.InputJSON)
	}

	// NO TIMEOUT: still parked well after the settle window.
	select {
	case res := <-done:
		t.Fatalf("the handler answered on its own (%+v) — an unanswered ask must block, not expire", res)
	case <-time.After(300 * time.Millisecond):
	}

	h.answer(t, ev.PermissionID, "once")
	res := h.awaitDecision(t, done)
	if res.optionID != "allow" {
		t.Errorf("answered once → optionId %q, want the allow option", res.optionID)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
