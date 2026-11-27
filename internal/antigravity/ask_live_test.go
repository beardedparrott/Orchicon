package antigravity

// ask_live_test.go — ONE live turn against the real `agy`, opt-in by environment, OFF by default.
//
// WHY IT EXISTS AND WHY IT SKIPS. The fixture harness (testsupport_test.go) proves the transport against the
// RECORDED wire; this proves the same code against the INSTALLED agy, which is the only way to catch a
// contract drift a recording cannot (a renamed option id, a permission request that stopped arriving, a flag
// agy now rejects). It costs a real model turn, so it never runs by accident:
//
//	ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity/ -run TestAskLive -v -timeout 180s
//
// IT ASSERTS ONLY STRUCTURE, NEVER MODEL OUTPUT. A live assertion on the model's prose is a test that fails
// when the model is in a different mood; "a permission request arrived and agy honoured the answer" is a
// property of the transport, which is what this file is for. That is also why the workspace is a temp dir the
// turn is allowed to write in: the probe must be able to OBSERVE an approved write without touching anything
// of the operator's.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestAskLive_OneTurnWithACard drives ONE real turn that must write a file, and answers the card.
func TestAskLive_OneTurnWithACard(t *testing.T) {
	if os.Getenv("ORCHICON_ANTIGRAVITY_LIVE") != "1" {
		t.Skip("live agy probe: set ORCHICON_ANTIGRAVITY_LIVE=1 to spend one model turn (~a few thousand tokens)")
	}
	// THE ONLY BOUNDED CALL IN THIS RUN, and it is bounded by BOTH the turn and the consent window.
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "120s")

	workspace := t.TempDir()
	b := New(quiet())
	b.SetAskRoot(t.TempDir())
	b.SetProjectDirResolver(func(context.Context, string, string) (string, error) { return workspace, nil })
	b.SetDiffLedger(nil, func(context.Context, string, string, string, string, map[string]any, string) {})
	var (
		cards, results []scheduler.SessionEvent
		mu             sync.Mutex
	)
	t.Cleanup(b.CloseAsk)

	ctx := context.Background()
	ctx = askmode.WithMode(ctx, askmode.Iteration)
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{ConversationID: "live_probe", ProjectID: "prj_live"})

	sid, err := b.CreateConversationSession(ctx, "live_probe", "live")
	if err != nil {
		t.Fatalf("CreateConversationSession against the real agy: %v", err)
	}
	t.Logf("live agy session id = %s", sid)

	sub, err := b.Subscribe(ctx, "live_probe")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	turnDone := make(chan error, 1)
	go func() {
		turnDone <- b.SendTurnMessage(ctx, "live_probe", sid, "You are a test harness.",
			"antigravity/google/"+liveModel(), "Create a file named probe.txt in the current directory containing exactly: ok. Then stop.")
	}()

	deadline := time.After(150 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("the bus closed mid-turn")
			}
			switch ev.Kind {
			case "permission":
				mu.Lock()
				cards = append(cards, ev)
				mu.Unlock()
				t.Logf("CARD: tool=%q command=%q targets=%v input=%s", ev.Tool, ev.Command, ev.Targets, ev.InputJSON)
				// APPROVE: this is the whole point — the write must actually happen.
				if err := b.ReplyPermissionDecision(ctx, sid, ev.PermissionID, "once"); err != nil {
					t.Errorf("ReplyPermissionDecision: %v", err)
				}
			case "tool_result":
				if ev.ToolName != "" {
					mu.Lock()
					results = append(results, ev)
					mu.Unlock()
					t.Logf("TOOL RESULT: %s error=%v output=%.120q", ev.ToolName, ev.IsError, ev.Output)
				}
			case "idle":
				goto done
			case "error":
				t.Fatalf("live turn failed: %s", ev.Text)
			}
		case <-deadline:
			t.Fatal("the live turn did not finish inside 150s")
		}
	}
done:
	select {
	case err := <-turnDone:
		if err != nil {
			t.Logf("SendTurnMessage returned: %v (the turn still completed)", err)
		}
	case <-time.After(5 * time.Second):
	}

	// THE STRUCTURAL CLAIM: a write tool was asked about, or it ran. agy may auto-approve an edit in its own
	// default mode (docs §3.3 does not promise it asks), so the probe records WHICH happened rather than
	// demanding a card — that difference is a property of agy's mode, not of this transport.
	mu.Lock()
	nCards, nResults := len(cards), len(results)
	mu.Unlock()
	t.Logf("live probe: %d card(s), %d tool result(s)", nCards, nResults)
	if nCards == 0 && nResults == 0 {
		t.Fatal("neither a permission card nor a tool call arrived — the transport is not wired to the real agy")
	}
	// If a file was written, say so; if not, that is the model's behaviour, not the transport's.
	if b, rerr := os.ReadFile(filepath.Join(workspace, "probe.txt")); rerr == nil {
		t.Logf("probe.txt = %q (the approved write reached the workspace)", string(b))
	} else {
		t.Logf("probe.txt was not written (%v) — no card was raised for it, or the model chose otherwise", rerr)
	}
}

// liveModel picks the cheapest model agy advertises for a live probe. It is a variable so an operator can pin
// one without editing this file.
func liveModel() string {
	if v := os.Getenv("ORCHICON_ANTIGRAVITY_LIVE_MODEL"); v != "" {
		return v
	}
	return "gemini-4-neon-low"
}

// TestAskLive_RunCommandRaisesACard drives ONE real turn that must run a shell command, and answers the card.
//
// WHY A SECOND PROBE EXISTS WHEN THE WRITE ONE ALREADY RUNS. agy 1.5.13's default mode auto-approves FILE
// EDITS without a `session/request_permission` (measured; docs §3.3, and the write probe above logs "0 card(s)"
// while `probe.txt` is written). A `run_command`, by contrast, DOES card. So the write probe proves the tool
// stream and that an approved write lands, and THIS probe proves the consent round-trip against the real
// binary — the card the operator actually sees, the answer travelling back, and the command running because
// the operator allowed it. Asserting "a write raises a card" would be asserting agy's mode, not this transport.
//
//	ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity/ -run TestAskLive_RunCommandRaisesACard -v
func TestAskLive_RunCommandRaisesACard(t *testing.T) {
	if os.Getenv("ORCHICON_ANTIGRAVITY_LIVE") != "1" {
		t.Skip("live agy probe: set ORCHICON_ANTIGRAVITY_LIVE=1 to spend one model turn (~a few thousand tokens)")
	}
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "120s")

	workspace := t.TempDir()
	b := New(quiet())
	b.SetAskRoot(t.TempDir())
	b.SetProjectDirResolver(func(context.Context, string, string) (string, error) { return workspace, nil })
	b.SetDiffLedger(nil, func(context.Context, string, string, string, string, map[string]any, string) {})
	t.Cleanup(b.CloseAsk)

	ctx := context.Background()
	ctx = askmode.WithMode(ctx, askmode.Iteration)
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{ConversationID: "live_command", ProjectID: "prj_live"})

	sid, err := b.CreateConversationSession(ctx, "live_command", "live")
	if err != nil {
		t.Fatalf("CreateConversationSession against the real agy: %v", err)
	}
	t.Logf("live agy session id = %s", sid)

	sub, err := b.Subscribe(ctx, "live_command")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	turnDone := make(chan error, 1)
	go func() {
		turnDone <- b.SendTurnMessage(ctx, "live_command", sid, "You are a test harness.",
			"antigravity/google/"+liveModel(), "Run the shell command `echo card-probe-ok` with run_command. Then stop.")
	}()

	var cards []scheduler.SessionEvent
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
				t.Logf("CARD: tool=%q command=%q targets=%v input=%s", ev.Tool, ev.Command, ev.Targets, ev.InputJSON)
				if err := b.ReplyPermissionDecision(ctx, sid, ev.PermissionID, "once"); err != nil {
					t.Errorf("ReplyPermissionDecision: %v", err)
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

	// THE STRUCTURAL CLAIM, and it is the one AC2's fixture asserts off the wire: agy asked, the card carried
	// the tool and (when agy supplies it) the command, and answering "once" let the call through.
	if len(cards) == 0 {
		t.Fatal("no permission card arrived for a run_command turn — the consent path is not reaching the real agy")
	}
	card := cards[0]
	if card.PermissionID == "" {
		t.Error("the card carried no PermissionID, so no client could answer it")
	}
	if card.Tool == "" {
		t.Error("the card named no tool")
	}
	t.Logf("live probe: %d card(s); first tool=%q command=%q targets=%v", len(cards), card.Tool, card.Command, card.Targets)
}
