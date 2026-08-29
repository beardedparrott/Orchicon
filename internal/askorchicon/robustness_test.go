package askorchicon

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// busToolRunning builds a tool-part bus event with a NON-terminal status — the
// shape the serve emits while a tool call is executing (and the exact shape
// LegacyEventFromBus drops, so the stall monitor must be fed proactively via
// observeToolStart).
func busToolRunning(sessionID, tool string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type": "tool", "tool": tool,
				"state": map[string]any{"status": "running"},
			},
		},
	}
}

// TestToolWedgeDetectsUnresolvedTool verifies the AC1 MCP-wedge signal: a tool
// call issued but never resolved trips toolWedge after the wedge window, and
// names the stalled tool. This is the signal no_progress cannot see (a wedged
// tool emits no completed tool_use, so LegacyEventFromBus never reports it).
func TestToolWedgeDetectsUnresolvedTool(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW", "5s")
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free")
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("list_projects")

	// Within the window: not wedged.
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	if name, wedged := m.toolWedge(); wedged {
		t.Fatalf("toolWedge = (%q,true) within window; want false", name)
	}
	// Past the window: wedged, naming the stalled tool.
	m.now = func() time.Time { return base.Add(6 * time.Second) }
	if name, wedged := m.toolWedge(); !wedged || name != "list_projects" {
		t.Fatalf("toolWedge = (%q,%v), want (list_projects,true)", name, wedged)
	}
}

// TestToolWedgeClearedByActivity verifies a legitimately slow tool that still
// streams activity never trips the wedge: a completed text (or any non-tool
// event) closes the open tool, so toolWedge returns false afterward.
func TestToolWedgeClearedByActivity(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW", "5s")
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free")
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("list_projects")
	// A completed text part means the model moved past the tool: close it.
	m.now = func() time.Time { return base.Add(3 * time.Second) }
	m.observe("text", nil)

	m.now = func() time.Time { return base.Add(10 * time.Second) }
	if _, wedged := m.toolWedge(); wedged {
		t.Fatal("toolWedge = true after a completed text closed the open tool; want false")
	}
}

// TestActiveToolNameNonTerminal verifies the raw-bus helper that feeds the
// wedge monitor: a non-terminal tool part is detected, a completed/errored one
// is not (that is LegacyEventFromBus's job), and a non-tool part is ignored.
func TestActiveToolName(t *testing.T) {
	if name, ok := activeToolName(busToolRunning("s", "read_file")); !ok || name != "read_file" {
		t.Fatalf("activeToolName(running) = (%q,%v), want (read_file,true)", name, ok)
	}
	if _, ok := activeToolName(opencode.BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"part": map[string]any{"type": "tool", "tool": "read_file", "state": map[string]any{"status": "completed"}},
	}}); ok {
		t.Fatal("activeToolName(completed) = true; want false (LegacyEventFromBus handles it)")
	}
	if _, ok := activeToolName(opencode.BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"part": map[string]any{"type": "text", "text": "hi"},
	}}); ok {
		t.Fatal("activeToolName(text part) = true; want false")
	}
	if _, ok := activeToolName(opencode.BusEvent{Type: "session.idle", Properties: map[string]any{}}); ok {
		t.Fatal("activeToolName(idle) = true; want false")
	}
}

// TestCollectConversationReplyRecyclesWedgedSession is the end-to-end AC1
// regression: a tool call that is issued and never resolves is detected as a
// wedge, the wedged session is aborted + recycled to a FRESH seeded session,
// and the SAME user message is re-dispatched and completes — no manual
// restart, no failure surfaced to the user.
func TestCollectConversationReplyRecyclesWedgedSession(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW", "500ms")
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "1500ms")
	t.Setenv("ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS", "2")
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")

	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", seedSystem: "SEED_SYSTEM", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello",
	}
	go func() {
		waitForSend(t, client, 1)
		// The wedged tool: issued, never resolves.
		client.sub.feed(busToolRunning("ses_live", "list_projects"))
		// Wait for the recycle's second dispatch, then the fresh session
		// completes normally.
		waitForSend(t, client, 2)
		client.sub.feed(busText("ses_1", "recovered reply"))
		client.sub.feed(busIdle("ses_1"))
	}()

	reply, _, sid, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("collect error: %v", err)
	}
	if reply != "recovered reply" {
		t.Errorf("reply = %q, want %q", reply, "recovered reply")
	}
	if sid != "ses_1" {
		t.Errorf("final session = %q, want recycled ses_1", sid)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	// The wedged session was aborted; a fresh one was created and used.
	if len(client.aborted) != 1 || client.aborted[0] != "ses_live" {
		t.Errorf("aborted = %v, want [ses_live]", client.aborted)
	}
	if len(client.created) != 1 || client.created[0] != "ses_1" {
		t.Errorf("created = %v, want [ses_1]", client.created)
	}
	// Two dispatches: the first onto the (now-wedged) reused session, the
	// retry onto the fresh seeded session.
	if len(client.sendCalls) != 2 {
		t.Fatalf("send calls = %d, want 2", len(client.sendCalls))
	}
	if got := client.sendCalls[1]; got.sessionID != "ses_1" || got.system != "SEED_SYSTEM" {
		t.Errorf("retry send = %+v, want session ses_1 with SEED_SYSTEM", got)
	}
}

// TestCollectConversationReplyReconnectBudgetIsBounded verifies the reconnect
// budget: after the bounded number of attempts, a persistently-wedged session
// is NOT recycled forever — the turn fails with a clear retryable error naming
// the stalled tool.
func TestCollectConversationReplyReconnectBudgetIsBounded(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW", "500ms")
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "1500ms")
	t.Setenv("ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS", "0")
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")

	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello",
	}
	go func() {
		waitForSend(t, client, 1)
		client.sub.feed(busToolRunning("ses_live", "list_projects"))
	}()

	_, _, _, err := collectTurn(t, client, opts)
	if err == nil || !strings.Contains(err.Error(), "wedged") {
		t.Fatalf("error = %v, want a wedged-tool retryable error", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 0 {
		t.Errorf("created = %v, want none (budget 0 → no recycle)", client.created)
	}
}
