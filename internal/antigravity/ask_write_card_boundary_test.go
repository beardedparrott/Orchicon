package antigravity

// ask_write_card_boundary_test.go — WHAT AC4 CAN AND CANNOT MEAN ON agy 1.5.13, pinned as a test.
//
// THE PROBLEM THIS FILE EXISTS FOR. AC4 asks for "streamed reasoning" and "a permission card for a write".
// Both were reported NOT VERIFIED and described as unreachable, and the documentation said so in prose.
// Prose is not a test. Two of the three claims below were untested in the Ask package:
//
//   - the REASONING path: `updateThought` existed in the fake child but NO Ask test delivered one, so nobody
//     could tell "agy never sends it" from "the bridge drops it";
//   - the NEVER-FABRICATE rule, which is the property that makes an empty reasoning block honest.
//
// And the third claim — "a file edit does not card on this binary" — is a measured property of the installed
// agy, not of this transport, so it belongs in a test that RECORDS the outcome instead of demanding one.
//
// THE MEASUREMENT BEHIND IT (agy 1.5.13, this host, 12 variants — default/accept-edits/auto/plan modes and
// `permissions.ask`, `permissions.deny`, `permissions.allow:[]`, per-tool string and object forms): a file
// write lands with ZERO `session/request_permission`. `permissions.deny` is inert. A `run_command` DOES card
// (TestAskLive_RunCommandRaisesACard). A thought chunk is emitted ONLY in `plan` mode — which is why
// "reasoning is never emitted" was wrong: it is emitted, just not in the acting modes.
//
// AND THE WRITE BOUNDARY THAT DOES HOLD IS THE SESSION MODE. In Brainstorm the session is put into `plan`, so
// the write never happens at all (proven live by TestAskLive_BrainstormCannotWrite). That is the enforceable
// promise; this file keeps it from being confused with "the operator was asked".

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestAskThoughtChunkStreamsAsReasoning is AC4's REASONING half on the Ask transport: an
// `agent_thought_chunk` becomes a reasoning delta AND a coalesced reasoning part at turn end, so the GUI
// fold and TUI reasoning_fold have something to render.
//
// IT IS NOT DEAD CODE FOR A FUTURE agy. agy 1.5.13 emits this chunk in `plan` mode (measured: 1–2 chunks with
// real text), and `plan` is exactly the agy mode a BRAINSTORM conversation is put into (modegate.go
// agySessionMode) — so a Brainstorm transcript shows streamed reasoning TODAY. That is the opposite of what
// the docs claimed ("agy never emits agent_thought_chunk"), which is why the mode is stated here.
func TestAskThoughtChunkStreamsAsReasoning(t *testing.T) {
	h := newAskHarness(t, script{
		updates: []string{
			updateThought("weighing the options, "),
			updateThought("then deciding."),
			updateMessage("here is the answer"),
		},
	})
	ctx := h.turnCtx(askmode.Brainstorm)
	h.subscribe(ctx)
	h.create(askmode.Brainstorm)

	col := drainSince(t, h)
	sent := h.sendTurn(ctx, h.sessionID(), askmode.Brainstorm, "think about it")
	waitFor(t, "the turn to end", func() bool { return len(col.ofKind("idle")) >= 1 })
	<-sent

	// THE STREAMED REASONING DELTAS.
	var reasoning string
	for _, d := range col.ofKind("delta") {
		if d.IsReasoning {
			reasoning += d.Text
		}
	}
	// EXACT TEXT, not a substring: a prefix or a reworded chunk means something is synthesising reasoning.
	if reasoning != "weighing the options, then deciding." {
		t.Fatalf("reasoning deltas = %q, want exactly %q (the two thought chunks concatenated); deltas "+
			"seen = %+v", reasoning, "weighing the options, then deciding.", col.ofKind("delta"))
	}

	// THE COALESCED REASONING PART (one per turn, as claude's Ask path emits).
	var found bool
	for _, p := range col.ofKind("part") {
		if p.Type == "reasoning" && p.Text == "weighing the options, then deciding." {
			found = true
		}
	}
	if !found {
		t.Fatalf("no coalesced part{reasoning} carrying the thought text; parts = %+v", col.ofKind("part"))
	}
}

// TestAskNoThoughtChunkMeansNoReasoning is the other half of the rule, and the one that keeps an empty
// reasoning block HONEST: a turn with no `agent_thought_chunk` must produce NO reasoning event at all. The
// bridge never synthesises reasoning, so a client showing nothing is showing the truth rather than a gap.
//
// THIS IS THE GUARD AGAINST A "just show something" CHANGE. A well-meaning refactor that emits a placeholder
// ("thinking…" as reasoning TEXT, or the assistant's own text relabelled) fails here.
func TestAskNoThoughtChunkMeansNoReasoning(t *testing.T) {
	h := newAskHarness(t, script{
		updates: []string{updateMessage("here is the answer")},
	})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	h.create(askmode.Iteration)

	col := drainSince(t, h)
	sent := h.sendTurn(ctx, h.sessionID(), askmode.Iteration, "answer me")
	waitFor(t, "the turn to end", func() bool { return len(col.ofKind("idle")) >= 1 })
	<-sent

	for _, d := range col.ofKind("delta") {
		if d.IsReasoning || d.Text != "here is the answer" {
			// The assistant text is a TEXT delta; anything flagged IsReasoning is fabricated reasoning.
			if d.IsReasoning {
				t.Fatalf("a turn with NO agent_thought_chunk emitted reasoning %q — reasoning must never be "+
					"fabricated", d.Text)
			}
		}
	}
	for _, p := range col.ofKind("part") {
		if p.Type == "reasoning" && strings.TrimSpace(p.Text) != "" {
			t.Fatalf("a turn with NO agent_thought_chunk emitted part{reasoning} = %q — it must be absent or "+
				"empty, never invented", p.Text)
		}
	}
}

// TestAskWriteToolCallIsNotADiffWhileInProgress pins the ORDER the Ask diff ledger depends on: an
// `in_progress` write has not touched the disk yet, so no diff may be reported for it. The ledger fires only
// on the TERMINAL `tool_call_update` (docs §12.8), because it diffs the file's fresh on-disk state.
//
// WHY THIS IS HERE AND NOT IN THE MODE TEST. It is the reason a write cannot be turned into a card after the
// fact: by the time we know the file changed it is already changed. It is also why "just diff it afterwards
// and ask then" is not an option.
func TestAskWriteToolCallIsNotADiffWhileInProgress(t *testing.T) {
	h := newAskHarness(t, script{
		updates: []string{
			updateToolCall("call_pending", "write_file", "edit", "write_file", "in_progress",
				map[string]any{"Path": "notes.md", "Content": "hi"}, "notes.md"),
		},
	})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	h.create(askmode.Iteration)

	col := drainSince(t, h)
	sent := h.sendTurn(ctx, h.sessionID(), askmode.Iteration, "write notes.md")
	waitFor(t, "the turn to end", func() bool { return len(col.ofKind("idle")) >= 1 })
	<-sent

	// The tool call STREAMED (the operator sees it), but it is not a completed edit.
	if !hasToolUse(col.ofKind("part"), "write_file") {
		t.Fatalf("an in_progress write produced no part{tool_use}; parts = %+v", col.ofKind("part"))
	}
	for _, l := range h.ledgerCalls() {
		t.Errorf("an in_progress write recorded a diff for %q; the ledger must wait for the terminal update",
			l.Tool)
	}
}

// TestAskLive_WriteDoesNotCardOnAGY RECORDS, against the REAL agy, how a file write actually behaves, and
// asserts the half this transport owns: the call streamed and the write reached the disk.
//
// IT DELIBERATELY DOES NOT ASSERT cards==0. That is agy's current mode behaviour, not this adapter's
// contract: a future agy that starts carding edits must not fail this test, it should log a different line.
// What must never happen is an operator being told a boundary exists that does not, so the outcome is
// printed either way.
//
//	ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity/ -run TestAskLive_WriteDoesNotCard -v
func TestAskLive_WriteDoesNotCardOnAGY(t *testing.T) {
	if os.Getenv("ORCHICON_ANTIGRAVITY_LIVE") != "1" {
		t.Skip("live agy probe: set ORCHICON_ANTIGRAVITY_LIVE=1 to spend one model turn (~a few thousand tokens)")
	}
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "120s")

	workspace := t.TempDir()
	b := New(quiet())
	b.SetAskRoot(t.TempDir())
	b.SetProjectDirResolver(func(_ context.Context, _, _ string) (string, error) { return workspace, nil })
	b.SetDiffLedger(nil, func(_ context.Context, _, _, _, _ string, _ map[string]any, _ string) {})
	t.Cleanup(b.CloseAsk)

	ctx := askmode.WithMode(context.Background(), askmode.Iteration)
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{
		ConversationID: "live_writecard", ProjectID: "prj_live"})

	sid, err := b.CreateConversationSession(ctx, "live_writecard", "live")
	if err != nil {
		t.Fatalf("CreateConversationSession against the real agy: %v", err)
	}
	t.Logf("live agy session id = %s", sid)

	sub, err := b.Subscribe(ctx, "live_writecard")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	turnDone := make(chan error, 1)
	go func() {
		turnDone <- b.SendTurnMessage(ctx, "live_writecard", sid, "You are a test harness.",
			"antigravity/google/"+liveModel(),
			"Create a file named probe.txt in the current directory containing exactly: ok. Then stop.")
	}()

	var cards, toolUses []scheduler.SessionEvent
	deadline := time.After(150 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("the bus closed mid-turn")
			}
			switch ev.Kind {
			case "permission":
				cards = append(cards, ev)
				t.Logf("CARD: tool=%q targets=%v", ev.Tool, ev.Targets)
				if err := b.ReplyPermissionDecision(ctx, sid, ev.PermissionID, "once"); err != nil {
					t.Errorf("ReplyPermissionDecision: %v", err)
				}
			case "part":
				if ev.Type == "tool_use" {
					toolUses = append(toolUses, ev)
					t.Logf("TOOL_USE: %v", ev.Part["tool"])
				}
			case "idle":
				break loop
			case "error":
				t.Fatalf("live turn failed: %s", ev.Text)
			}
		case <-deadline:
			t.Fatal("the live turn did not finish inside 150s")
		}
	}
	select {
	case <-turnDone:
	case <-time.After(5 * time.Second):
	}

	if len(toolUses) == 0 {
		t.Fatal("no tool call reached the bus — the transport is not wired to the real agy")
	}
	probe := filepath.Join(workspace, "probe.txt")
	content, rerr := os.ReadFile(probe)
	if rerr != nil {
		t.Fatalf("the write tool streamed but %s was never written (%v) — an approved edit must land", probe, rerr)
	}
	if strings.TrimSpace(string(content)) != "ok" {
		t.Errorf("%s = %q, want %q", probe, string(content), "ok")
	}

	if len(cards) == 0 {
		t.Logf("MEASURED: agy 1.5.13 wrote %s with NO permission card — a file edit is not carded on this "+
			"binary. The enforceable write boundary is the SESSION MODE (Brainstorm -> plan), not a card; see "+
			"modegate.go and TestAskLive_BrainstormCannotWrite.", probe)
		return
	}
	t.Logf("MEASURED: agy raised %d card(s) for a write — this agy now asks before editing. The handler "+
		"already answers it, so no code change is needed.", len(cards))
}
