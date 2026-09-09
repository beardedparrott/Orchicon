package askorchicon

// Session-durability tests: every message/action is persisted LIVE, so a
// session killed mid-turn (timeout/abort/crash — collector gone, finalize
// never runs) still leaves the user message AND the partial assistant output
// visible. A fresh session then resumes with full history (the DB transcript
// is the resume source, not the dead collector's memory).
//
// Skipped unless ORCHICON_TEST_DSN points at a disposable database (the
// pattern used across the repo).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// busToolStart builds a tool-ISSUED (in-flight, status running) bus event:
// the call has been made but no result arrived. The drain loop records it in
// the live tool ledger the moment it is observed.
func busToolStart(sessionID, tool string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type":  "tool",
				"tool":  tool,
				"state": map[string]any{"status": "running", "input": map[string]any{"dir": "src"}},
			},
		},
	}
}

// TestKillMidTurnLeavesHistoryIntact is the acceptance test: start a turn,
// stream partial text, issue a tool, then kill the turn mid-stream via the
// registry cancel (the Stop path — the collector finalizes promptly with the
// stop error, the same write a timeout/abort produces). Reload the
// conversation: the user message AND the partial assistant output (text +
// tool call) must both be present — nothing up to the kill is lost.
func TestKillMidTurnLeavesHistoryIntact(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "investigate the outage", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)

	// The turn streams partial content: a completed text part, then a tool
	// call is issued. Each onPartial snapshot is mirrored (250ms flusher).
	client.sub.feed(busText("ses_1", "root cause analysis so far"))
	client.sub.feed(busToolStart("ses_1", "orchicon_list_project_dir"))
	// Wait until the partial row carries BOTH the text and the tool call
	// (proves the live mirror landed before the kill).
	deadline := time.After(10 * time.Second)
	for {
		msgs := listMessages(t, pool, convID)
		var content, toolCalls string
		var found bool
		for i := range msgs {
			if msgs[i].ID == ackID {
				content = msgs[i].Content
				toolCalls = string(msgs[i].ToolCalls)
				found = true
				break
			}
		}
		if found && strings.Contains(content, "root cause analysis so far") &&
			strings.Contains(toolCalls, "orchicon_list_project_dir") {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("partial row never carried text+tool before the kill (content=%q tools=%s)", content, toolCalls)
		}
	}

	// KILL mid-turn: cancel the collector (Stop semantics). The collector
	// finalizes promptly with the stop error over the partial — the stop
	// write itself is a durability write carrying text+reasoning+tools.
	s.turns.cancel(convID, errUserStop)
	msg := waitForMessage(t, pool, convID, ackID)

	// Reload the conversation after the kill: user message survived
	// (persisted BEFORE dispatch) and the partial assistant output survived
	// (text + tool call up to the kill point).
	msgs := listMessages(t, pool, convID)
	foundUser := false
	for i := range msgs {
		if msgs[i].Role == "user" && msgs[i].Content == "investigate the outage" {
			foundUser = true
			break
		}
	}
	if !foundUser {
		t.Fatalf("user message lost after mid-turn kill (%d messages)", len(msgs))
	}
	if !strings.Contains(msg.Content, "root cause analysis so far") {
		t.Errorf("partial text lost after kill: %q", msg.Content)
	}
	if !strings.Contains(string(msg.ToolCalls), "orchicon_list_project_dir") {
		t.Errorf("tool call lost after kill: %s", string(msg.ToolCalls))
	}
}

// TestLiveToolLedgerPersistsCallsAndResults verifies tool calls AND results
// land in the message row live (mirrored) and terminally (finalize): a turn
// that issues a tool and completes persists the full ledger on the acked row,
// and the read path maps it onto the ChatMessage wire fields.
func TestLiveToolLedgerPersistsCallsAndResults(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "list the projects", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)

	// Tool issued (start) then resolved (completed tool_use with input+output).
	client.sub.feed(busToolStart("ses_1", "orchicon_list_projects"))
	client.sub.feed(busToolWithArgs("ses_1", "orchicon_list_projects", map[string]any{"dir": "src"}))
	client.sub.feed(busText("ses_1", "here are your projects"))
	client.sub.feed(busIdle("ses_1"))

	msg := waitForMessage(t, pool, convID, ackID)
	var calls []map[string]any
	if err := json.Unmarshal(msg.ToolCalls, &calls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v", err)
	}
	if len(calls) != 1 || calls[0]["function_name"] != "orchicon_list_projects" {
		t.Errorf("tool_calls = %v, want one orchicon_list_projects call", calls)
	}
	var results []map[string]any
	if err := json.Unmarshal(msg.ToolResults, &results); err != nil {
		t.Fatalf("unmarshal tool_results: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tool_results = %v, want one result", results)
	}
	// The read path maps the columns onto the wire message.
	proto := messageRowToProto(msg)
	if len(proto.GetToolCalls()) != 1 || proto.GetToolCalls()[0].GetFunctionName() != "orchicon_list_projects" {
		t.Errorf("wire tool_calls = %v, want the ledger call", proto.GetToolCalls())
	}
	if len(proto.GetToolResults()) != 1 {
		t.Errorf("wire tool_results = %v, want the ledger result", proto.GetToolResults())
	}
}
