package antigravity

// ask_modegate_test.go — AC3: the mode boundary, as a table over mode × tool.
//
// THE PAIR IS THE CLAIM, not just the verdict. For every (mode, tool) the test asserts:
//
//	carded    — did a `permission` event reach the operator at all?
//	allowed   — was agy answered the ALLOW option (or anything other than the reject/cancelled)?
//
// which together pin both halves of the requirement: "in Brainstorm, write/replace/run are rejected WITHOUT
// a card" (carded=false, allowed=false) and "view_file/grep_search proceed" (allowed=true), and "in
// Iteration a write inside the project raises a card" (carded=true).
//
// THE CARD IS ASSERTED OVER THE REAL WIRE. The fake child sends a genuine session/request_permission, so a
// verdict here is a statement about what agy would have been told, not about a helper's return value.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskModeGate is AC3.
func TestAskModeGate(t *testing.T) {
	// A path INSIDE the conversation project, so the Iteration write is a real in-boundary write.
	const insideProject = "docs/notes.md"

	cases := []struct {
		name    string
		mode    string
		tool    string
		args    func(project string) map[string]any
		carded  bool
		allowed bool
	}{
		// --- BRAINSTORM: no write, no edit, no shell; reads proceed. -----------------------------
		{"brainstorm refuses write_to_file WITHOUT a card", askmode.Brainstorm, "write_to_file",
			func(p string) map[string]any { return map[string]any{"TargetFile": filepath.Join(p, insideProject)} },
			false, false},
		{"brainstorm refuses replace_file_content WITHOUT a card", askmode.Brainstorm, "replace_file_content",
			func(p string) map[string]any { return map[string]any{"TargetFile": filepath.Join(p, insideProject)} },
			false, false},
		{"brainstorm refuses run_command WITHOUT a card", askmode.Brainstorm, "run_command",
			func(string) map[string]any { return map[string]any{"CommandLine": "ls"} },
			false, false},
		{"brainstorm allows view_file with no card", askmode.Brainstorm, "view_file",
			func(p string) map[string]any { return map[string]any{"AbsolutePath": filepath.Join(p, insideProject)} },
			false, true},
		{"brainstorm allows grep_search with no card", askmode.Brainstorm, "grep_search",
			func(string) map[string]any { return map[string]any{"query": "func main", "SearchPath": "."} },
			false, true},
		// An OPAQUE third-party MCP tool is an ACTION, and Brainstorm may not act.
		{"brainstorm refuses an opaque MCP tool WITHOUT a card", askmode.Brainstorm, "mcp_github_create_issue",
			func(string) map[string]any { return map[string]any{"repo": "x/y"} },
			false, false},
		// ...but the platform's OWN tools are never opaque, so the ask card still works in Brainstorm.
		{"brainstorm allows orchicon_ask_user", askmode.Brainstorm, "mcp_orchicon_orchicon_ask_user",
			func(string) map[string]any { return map[string]any{"question": "which?"} },
			false, true},

		// --- QUICK WORK: the same refusals (it dispatches rather than does). ---------------------
		{"quick work refuses write_to_file WITHOUT a card", askmode.QuickWork, "write_to_file",
			func(p string) map[string]any { return map[string]any{"TargetFile": filepath.Join(p, insideProject)} },
			false, false},
		{"quick work refuses run_command WITHOUT a card", askmode.QuickWork, "run_command",
			func(string) map[string]any { return map[string]any{"CommandLine": "rm -rf /tmp/x"} },
			false, false},
		{"quick work allows view_file with no card", askmode.QuickWork, "view_file",
			func(p string) map[string]any { return map[string]any{"AbsolutePath": filepath.Join(p, insideProject)} },
			false, true},

		// --- ITERATION: the hands. A write inside the project IS a card. ------------------------
		{"iteration raises a card for a write inside the project", askmode.Iteration, "write_to_file",
			func(p string) map[string]any {
				return map[string]any{"TargetFile": filepath.Join(p, insideProject), "Content": "hi"}
			},
			true, true},
		{"iteration raises a card for a run_command", askmode.Iteration, "run_command",
			func(string) map[string]any { return map[string]any{"CommandLine": "ls"} },
			true, true},
		{"iteration allows view_file with no card", askmode.Iteration, "view_file",
			func(p string) map[string]any { return map[string]any{"AbsolutePath": filepath.Join(p, insideProject)} },
			false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, col, _ := turnReady(t, tc.mode)
			_, ackCh := turnInFlight(t, h, nil, tc.mode)
			fp := h.latestProc()

			args := tc.args(h.dir)
			reqID := fp.askPermission(t, tc.tool, args)

			// THE PAIR: was a card raised, and what did agy get told? A card parks until an operator answers,
			// so this must poll for EITHER, never wait for the answer alone.
			carded, out := awaitCardOrAnswer(t, col, fp, reqID, 5*time.Second)
			if carded != tc.carded {
				t.Fatalf("carded = %v, want %v (%d permission events)", carded, tc.carded, len(col.ofKind("permission")))
			}

			if tc.carded {
				// APPROVE the card so the turn can finish, and read the answer it produces.
				ev := awaitAskEvent(t, col)
				if err := h.b.ReplyPermissionDecision(context.Background(), h.sessionID(), ev.PermissionID, "once"); err != nil {
					t.Fatalf("ReplyPermissionDecision: %v", err)
				}
				out = fp.awaitPermAnswer(t, reqID)
			}
			allowed := out.Outcome.Outcome == "selected" && out.Outcome.OptionID == "allow"
			if allowed != tc.allowed {
				t.Fatalf("agy was answered %+v (allowed=%v), want allowed=%v", out.Outcome, allowed, tc.allowed)
			}
			ackTurn(t, fp, ackCh)
		})
	}
}

// TestAskModeGate_RefusalReachesTheModelCheck is the OTHER half of "a refused tool never raises a card": the
// operator sees the refusal INLINE as a tool_result with IsError, and the model is told WHY on the next turn.
//
// Both halves matter and for different readers: without the event the operator sees a tool call vanish, and
// without the notice the model sees an unexplained failure and retries it.
func TestAskModeGate_RefusalReachesTheModelCheck(t *testing.T) {
	h, col, sid := turnReady(t, askmode.Brainstorm)
	_, ackCh := turnInFlight(t, h, nil, askmode.Brainstorm)
	fp := h.latestProc()

	reqID := fp.askPermission(t, "write_to_file", map[string]any{
		"TargetFile": filepath.Join(h.dir, "docs", "notes.md"),
		"Content":    "hi",
	})
	// REFUSED WITHOUT A CARD.
	out := fp.awaitPermAnswer(t, reqID)
	if out.Outcome.Outcome == "selected" && out.Outcome.OptionID == "allow" {
		t.Fatalf("a Brainstorm write was ALLOWED (%+v)", out.Outcome)
	}
	if n := len(col.ofKind("permission")); n != 0 {
		t.Fatalf("%d permission event(s) were raised for a mode-refused tool, want 0", n)
	}

	// THE OPERATOR SEES IT INLINE.
	results := col.ofKind("tool_result")
	if len(results) == 0 {
		t.Fatal("no tool_result event: the refusal is invisible to the operator")
	}
	sawRefusal := false
	for _, r := range results {
		if r.IsError && containsFold(r.Output, "REFUSED BY THE PLATFORM") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("no IsError tool_result carried the refusal text: %+v", results)
	}
	ackTurn(t, fp, ackCh)

	// THE MODEL IS TOLD ON THE NEXT TURN: the refusal is prepended to the next prompt.
	next := h.sendTurn(h.turnCtx(askmode.Brainstorm), sid, askmode.Brainstorm, "what happened?")
	waitFor(t, "the second prompt to reach agy", func() bool { return len(h.latestProc().promptsSeen()) >= 2 })
	fp = h.latestProc()
	prompts := fp.promptsSeen()
	if !containsFold(prompts[1].Text, "REFUSED BY THE PLATFORM") {
		t.Fatalf("the next prompt did not carry the refusal notice; got:\n%s", prompts[1].Text)
	}
	ackTurn(t, fp, ackCh)
	<-next
}

// TestAskModeGate_SettingsDenyDocument pins the spawn-time hardening: the `--settings` document names the
// agy spellings of the denied tools, so a refused tool cannot even reach a permission request.
func TestAskModeGate_SettingsDenyDocument(t *testing.T) {
	brainstorm := agyAskModeSettings(askmode.Brainstorm)
	for _, want := range []string{"run_command", "write_file", "write_to_file", "edit_file", "ask_question"} {
		if !containsFold(brainstorm, want) {
			t.Errorf("Brainstorm settings deny document is missing %q: %s", want, brainstorm)
		}
	}
	// ITERATION DENIES NOTHING BUT THE UNANSWERABLE NATIVE ASK TOOL.
	iteration := agyAskModeSettings(askmode.Iteration)
	if containsFold(iteration, "run_command") || containsFold(iteration, "write_file") {
		t.Errorf("Iteration's deny document names a work tool: %s", iteration)
	}
	if !containsFold(iteration, "ask_question") {
		t.Errorf("Iteration's deny document does not deny ask_question (its answer cannot be returned): %s", iteration)
	}
	// IT MUST BE VALID JSON THAT agy ACCEPTS: agy rejects an unknown key, and the document is inline.
	if !validJSONObject(iteration) || !validJSONObject(brainstorm) {
		t.Errorf("a settings deny document is not a valid JSON object: brainstorm=%s iteration=%s", brainstorm, iteration)
	}
}

// TestAskModeGate_SpawnArgvCarriesTheSettings proves the document actually reaches the child: the argv the
// bridge spawned with contains `--settings <json>` BEFORE the `acp` subcommand (agy 1.5.13 rejects the flag
// after it, verified in acp/proc_test).
func TestAskModeGate_SpawnArgvCarriesTheSettings(t *testing.T) {
	_, _, _ = turnReady(t, askmode.Brainstorm) // creates a session in Brainstorm, which denies work tools
	// turnReady already created the conversation; the harness exposes the spawned child's argv.
	h := newAskHarness(t, script{})
	h.create(askmode.Brainstorm)
	fp := h.proc(0)

	argv := fp.argv
	idx := -1
	for i, a := range argv {
		if a == "--settings" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the spawned argv carries no --settings: %v", argv)
	}
	if idx+2 >= len(argv) || argv[idx+2] != "acp" {
		t.Fatalf("--settings is not IMMEDIATELY BEFORE the acp subcommand (agy rejects it after): %v", argv)
	}
	if !containsFold(argv[idx+1], "run_command") {
		t.Fatalf("Brainstorm's --settings document names no denied tool: %s", argv[idx+1])
	}
}
