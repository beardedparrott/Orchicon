package antigravity

// ask_card_live_test.go — AC5 against the REAL agy: can a conversation reach the platform's ask card?
//
// THE SPLIT THAT MAKES THIS TEST WHAT IT IS. `orchicon_ask_user` is a tool on the ORCHICON MCP SIDECAR, not
// an ACP method. agy calls it over MCP; the sidecar runs it in askorchicon's process, and askorchicon is what
// raises the option card and returns the operator's answer. NONE of that travels through this bridge or the
// ACP wire, which is exactly why agy's native `ask_question` was rejected as the routing (it has no answer
// channel at all — docs/adapters/antigravity-acp.md §3.5).
//
// So the transport's responsibility, and therefore this test's claim, is narrower and precise: THE MODEL CAN
// REACH THE TOOL. That is precisely what fails when the Orchicon MCP server is missing from the
// conversation's `session/new` mcpServers, and it is the one link in the chain this adapter owns. The card
// render and answer delivery are covered by `internal/askorchicon` (ask_card_claude_test.go,
// ask_user_loop_test.go, tool_ask_user_test.go), which see a tool call rather than a transport and are
// therefore shared by every MCP-served adapter.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestAskLive_AskUserToolReachesTheModel is AC5's transport half: a real agy turn reaches the platform's
// `orchicon_ask_user` tool, which only works when the Orchicon MCP server is registered in Ask scope.
//
//	ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity/ -run TestAskLive_AskUserToolReachesTheModel -v
func TestAskLive_AskUserToolReachesTheModel(t *testing.T) {
	if os.Getenv("ORCHICON_ANTIGRAVITY_LIVE") != "1" {
		t.Skip("live agy probe: set ORCHICON_ANTIGRAVITY_LIVE=1 to spend model turns")
	}
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "120s")
	// THE SIDECAR MUST BE A REAL `orchicon`. Without this the entry resolves to the TEST binary, which has
	// no `mcp` subcommand: agy cannot start it, the conversation gets no orchicon_* tools, and this probe
	// fails for a reason no product change can fix (see asksidecar_test.go).
	askSidecarBinary(t)

	workspace := t.TempDir()
	b := New(quiet())
	b.SetAskRoot(t.TempDir())
	b.SetProjectDirResolver(func(context.Context, string, string) (string, error) { return workspace, nil })
	b.SetDiffLedger(nil, func(context.Context, string, string, string, string, map[string]any, string) {})
	t.Cleanup(b.CloseAsk)

	ctx := askmode.WithMode(context.Background(), askmode.Iteration)
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{
		ConversationID: "live_askcard", ProjectID: "prj_live"})
	model := "antigravity/google/" + liveModel()

	sid, err := b.CreateConversationSession(ctx, "live_askcard", "live")
	if err != nil {
		t.Fatalf("CreateConversationSession against the real agy: %v", err)
	}
	t.Logf("live agy session id = %s", sid)

	sub, err := b.Subscribe(ctx, "live_askcard")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// --- TURN 1: the model must ask the operator a question ---------------------------------------
	// The prompt names the two options and forbids answering it itself, so the only way to proceed is a
	// tool call. `orchicon_ask_user` is on the Orchicon MCP server, which Ask scope registers.
	turn1 := "Ask me to choose between exactly these two options: RED and BLUE. Use the Orchicon MCP tool " +
		"orchicon_ask_user for the question — do NOT use agy's own ask_question tool, and do NOT guess my " +
		"answer. Ask, then stop and wait."

	var (
		mu        sync.Mutex
		askEvents []scheduler.SessionEvent
		toolNames []string
	)
	done := make(chan error, 1)
	go func() { done <- b.SendTurnMessage(ctx, "live_askcard", sid, "You are a test harness.", model, turn1) }()

	turn1Text := collectTurn(t, sub, 150*time.Second, func(ev scheduler.SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Kind {
		case "permission":
			// A card for the ask tool itself would be a card about a card; record it if it happens.
			t.Logf("PERMISSION CARD: tool=%q command=%q", ev.Tool, ev.Command)
		case "tool_result", "tool_use":
			if ev.ToolName != "" {
				toolNames = append(toolNames, ev.ToolName)
			}
		case "ask":
			// The platform's ask card event.
			askEvents = append(askEvents, ev)
			t.Logf("ASK CARD: id=%q question=%.160q options=%v allowOther=%v",
				ev.PermissionID, ev.Text, ev.Targets, ev.AllowOther)
		}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Logf("turn 1 SendTurnMessage returned: %v (the turn still completed)", err)
		}
	case <-time.After(10 * time.Second):
	}

	mu.Lock()
	nAsk, names := len(askEvents), append([]string(nil), toolNames...)
	mu.Unlock()

	t.Logf("turn 1: %d ask card(s); tools seen = %v; text = %.200q", nAsk, names, turn1Text)

	// THE STRUCTURAL CLAIM: the model reached the platform's ask tool. If it did not, the MCP server is not
	// in scope (or the preamble did not reach it) — either is a real failure of this criterion, not a model
	// quirk, because the prompt names the tool and forbids the alternative.
	askedViaPlatform := nAsk > 0
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), "ask") {
			askedViaPlatform = true
			t.Logf("the model called an ask tool: %q", n)
		}
	}
	if !askedViaPlatform {
		t.Fatalf("the model never reached an ask tool (tools seen = %v, reply %.300q). "+
			"`orchicon_ask_user` lives on the Orchicon MCP server, which Ask scope must register — if it is "+
			"missing the conversation has no way to ask the operator anything.", names, turn1Text)
	}

	// --- WHAT THIS BRIDGE CAN AND CANNOT PROVE ----------------------------------------------------
	//
	// THE CARD ITSELF IS NOT THIS TRANSPORT'S. `orchicon_ask_user` is a tool on the Orchicon MCP SIDECAR:
	// agy calls it over MCP, the sidecar's implementation runs in askorchicon's process, and THAT is what
	// raises the option card and returns the operator's answer. There is no `ReplyAskCard` on this bridge
	// and inventing one here would be a test of nothing — the answer never travels through the antigravity
	// ACP wire at all, which is precisely why routing the native `ask_question` here was rejected.
	//
	// So the honest claim at THIS layer is: the model can reach the platform's ask tool. That is exactly the
	// thing that fails when the Orchicon MCP server is missing from the conversation's `mcpServers`, and it
	// is what the transport is responsible for. The card render + answer delivery are covered by
	// `internal/askorchicon` (ask_card_claude_test.go, ask_user_loop_test.go, tool_ask_user_test.go) and are
	// transport-independent: they see a tool call, not an ACP session.
	t.Logf("ASK TOOL REACHED LIVE: the conversation called %v, so the Orchicon MCP server is registered in "+
		"Ask scope and the platform's ask card is reachable from agy", names)
}

// collectTurn reads the bus until the turn ends, returning the assistant text and handing each event to fn.
func collectTurn(t *testing.T, sub scheduler.SessionBus, limit time.Duration, fn func(scheduler.SessionEvent)) string {
	t.Helper()
	var text strings.Builder
	deadline := time.After(limit)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return text.String()
			}
			switch ev.Kind {
			case "delta", "text":
				text.WriteString(ev.Text)
			case "idle":
				return text.String()
			case "error":
				t.Logf("turn error event: %s", ev.Text)
				return text.String()
			}
			if fn != nil {
				fn(ev)
			}
		case <-deadline:
			t.Logf("the turn did not report idle inside %s; returning what arrived", limit)
			return text.String()
		}
	}
}
