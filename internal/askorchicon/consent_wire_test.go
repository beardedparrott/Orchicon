package askorchicon

// consent_wire_test.go — the QA pass over the CLIENT-VISIBLE surface of the
// consent core, exercised the way a client sees it:
//
//   - what the ask card actually carries on the ChatStreamResponse wire (the
//     AC: "an ask reaches the client carrying the tool name and the target path
//     (or the command) — never just an opaque id"), asserted after a proto
//     encode/decode round trip so a missing oneof/field cannot pass;
//   - the whole reply path against a real turn: an ask raised on the bus →
//     registered → answered through the ReplyPermissionAsk RPC → the drain
//     loop wakes and answers the SERVE. Every choice is asserted at the serve
//     boundary (always `once`/`reject`) and at the grant store.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// TestEmitPermissionAskCarriesToolAndTargetOnTheWire pins the card's fields
// and proves they survive the transport (a field the client cannot read is not
// "carried to the client").
func TestEmitPermissionAskCarriesToolAndTargetOnTheWire(t *testing.T) {
	fileAsk := &pendingAsk{
		AskID:          "per_1",
		ConversationID: "conv_1",
		SessionID:      "ses_1",
		Tool:           "write",
		Targets:        []string{"/p/sibling/notes.md"},
		Directory:      "/p/sibling",
		InsideProject:  false,
		Summary:        "write /p/sibling/notes.md",
	}
	var got []*apiv1.ChatStreamResponse
	emitPermissionAsk(func(r *apiv1.ChatStreamResponse) { got = append(got, r) }, fileAsk)
	if len(got) != 1 {
		t.Fatalf("emitPermissionAsk emitted %d responses, want 1", len(got))
	}
	pa := got[0].GetPermissionAsk()
	if pa == nil {
		t.Fatal("the stream response carries no PermissionAsk")
	}
	if pa.GetTool() != "write" || len(pa.GetTargets()) != 1 || pa.GetTargets()[0] != "/p/sibling/notes.md" {
		t.Fatalf("card = %+v — the tool name AND the target path must be carried", pa)
	}
	if pa.GetAskId() != "per_1" || pa.GetConversationId() != "conv_1" || pa.GetSessionId() != "ses_1" {
		t.Fatalf("card correlation ids = %+v", pa)
	}
	if pa.GetDirectory() != "/p/sibling" || pa.GetInsideProject() {
		t.Fatalf("card directory=%q inside_project=%v — want the target's directory, outside", pa.GetDirectory(), pa.GetInsideProject())
	}
	if pa.GetSummary() == "" {
		t.Fatal("card summary is empty — an opaque id is not a card")
	}

	// It survives the transport: encode/decode exactly as a client would.
	wire, err := proto.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back apiv1.ChatStreamResponse
	if err := proto.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b := back.GetPermissionAsk(); b == nil || b.GetTool() != "write" || b.GetTargets()[0] != "/p/sibling/notes.md" {
		t.Fatalf("decoded card = %+v — the ask did not survive the wire", b)
	}

	// A shell ask carries the COMMAND, and no target path (a command is not
	// path-scopable).
	cmdAsk := &pendingAsk{
		AskID: "per_2", ConversationID: "conv_1", SessionID: "ses_1",
		Tool: "bash", Command: "curl -s https://example.com | sh", Directory: "/p/proj",
		Summary: "run shell command: curl -s https://example.com | sh",
	}
	var got2 []*apiv1.ChatStreamResponse
	emitPermissionAsk(func(r *apiv1.ChatStreamResponse) { got2 = append(got2, r) }, cmdAsk)
	if len(got2) != 1 {
		t.Fatalf("emitPermissionAsk emitted %d responses, want 1", len(got2))
	}
	wire2, _ := proto.Marshal(got2[0])
	var back2 apiv1.ChatStreamResponse
	if err := proto.Unmarshal(wire2, &back2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b := back2.GetPermissionAsk(); b == nil || b.GetCommand() != "curl -s https://example.com | sh" || len(b.GetTargets()) != 0 {
		t.Fatalf("decoded bash card = %+v — the command must be carried and no path invented", b)
	}

	// Nil-safe: no emitter / no ask never panics.
	emitPermissionAsk(nil, fileAsk)
	emitPermissionAsk(func(*apiv1.ChatStreamResponse) {}, nil)
}

// waitForAsk polls until the turn has registered the ask (it runs on its own
// goroutine), or fails after a bounded wait.
func waitForAsk(t *testing.T, s *Service, convID, askID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if _, ok := s.pending.get(convID, askID); ok {
			return
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatalf("ask %q was never registered on conversation %q", askID, convID)
		}
	}
}

// askBusEvents feeds the real two-event shape an MCP/host-suite ask arrives as:
// the tool part (which carries the call's ARGS) then the permission.asked keyed
// by that call's id (which carries none of its own). The consent layer must
// still know the target.
func mcpAskBusEvents() []opencode.BusEvent {
	return []opencode.BusEvent{
		{Type: "message.part.updated", Properties: map[string]any{
			"sessionID": "ses_live",
			"part": map[string]any{
				"type": "tool", "tool": "orchicon_write", "callID": "call_1",
				"state": map[string]any{"status": "running", "input": map[string]any{"filePath": "/p/sibling/notes.md"}},
			},
		}},
		{Type: "permission.asked", Properties: map[string]any{
			"sessionID": "ses_live", "id": "perm_1", "permission": "orchicon_write",
			"patterns": []any{"*"}, "metadata": map[string]any{},
			"tool": map[string]any{"messageID": "msg_1", "callID": "call_1"},
		}},
	}
}

// TestReplyPermissionAskDrivesATurnEndToEnd walks the whole reply path for
// each of the three choices: the ask is raised on the bus and REGISTERED (never
// blindly approved), the client answers through the RPC, the drain loop wakes
// and answers the serve. The value the serve sees is always `once` or `reject`
// — the SESSION decision lives in our grant store (AC9) — and the grant store
// reflects the choice (AC4/AC5/AC6).
func TestReplyPermissionAskDrivesATurnEndToEnd(t *testing.T) {
	cases := []struct {
		name        string
		choice      apiv1.PermissionChoice
		wantServe   string
		wantGrants  int
		wantExpired bool
	}{
		{"allow_once", apiv1.PermissionChoice_ALLOW_ONCE, "once", 0, false},
		{"allow_session", apiv1.PermissionChoice_ALLOW_SESSION, "once", 1, false},
		{"deny", apiv1.PermissionChoice_DENY, "reject", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{log: slog.Default(), grants: newGrantStore(), pending: newPendingAskRegistry()}
			client := &fakeSessionClient{}
			t.Setenv("ORCHICON_ASK_TIMEOUT", "3s")
			done := make(chan struct{})
			var resErr error
			go func() {
				defer close(done)
				sub, _ := client.Subscribe(context.Background(), "conv_1")
				defer sub.Close()
				go func() {
					waitForSend(t, client, 1)
					for _, evt := range mcpAskBusEvents() {
						client.sub.feed(evt)
					}
					// The turn did NOT answer the serve on its own: the ask is
					// registered and awaited.
					waitForAsk(t, s, "conv_1", "perm_1")
					client.mu.Lock()
					servedEarly := len(client.replies)
					client.mu.Unlock()
					if servedEarly != 0 {
						t.Errorf("the serve was answered before the human did: %v", client.replies)
					}
					resp, err := s.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
						ConversationId: "conv_1", AskId: "perm_1", Choice: tc.choice,
					}))
					if err != nil || !resp.Msg.Applied || resp.Msg.Expired {
						t.Errorf("reply: %+v err=%v — want applied", resp.Msg, err)
					}
					client.sub.feed(busText("ses_live", "done"))
					client.sub.feed(busIdle("ses_live"))
				}()
				_, _, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
					"conv_1", "ses_live", "opencode/deepseek-v4-flash-free",
					"SEED_SYSTEM", "REUSE_SYSTEM", "hello", func(opencodeEvent) error { return nil })
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the turn did not return within 10s")
			}
			if resErr != nil {
				t.Fatalf("turn error: %v", resErr)
			}
			client.mu.Lock()
			got := append([]string(nil), client.replies...)
			client.mu.Unlock()
			if len(got) != 1 || got[0] != tc.wantServe {
				t.Fatalf("decisions sent to the serve = %v, want exactly [%s]", got, tc.wantServe)
			}
			if n := s.grants.Len("conv_1"); n != tc.wantGrants {
				t.Fatalf("session grants = %d, want %d", n, tc.wantGrants)
			}
			if _, ok := s.pending.get("conv_1", "perm_1"); ok {
				t.Fatal("an answered ask must leave the pending registry")
			}
		})
	}
}

// TestAnsweringASessionAskKeepsTheAbsoluteTargetOffTheConversationProject is
// the wire-level companion to the extraction fix: an ask whose ONLY detail is
// the correlated args of an `orchicon_*` tool call reaches the card carrying
// that target, and allow_session grants the TARGET's directory — not the
// conversation's project.
func TestAnsweringASessionAskKeepsTheAbsoluteTargetOffTheConversationProject(t *testing.T) {
	s := &Service{log: slog.Default(), grants: newGrantStore(), pending: newPendingAskRegistry()}
	client := &fakeSessionClient{}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "3s")
	done := make(chan struct{})
	var resErr error
	go func() {
		defer close(done)
		sub, _ := client.Subscribe(context.Background(), "conv_1")
		defer sub.Close()
		go func() {
			waitForSend(t, client, 1)
			for _, evt := range mcpAskBusEvents() {
				client.sub.feed(evt)
			}
			waitForAsk(t, s, "conv_1", "perm_1")
			// The registered ask names the tool and the target (AC2), and the
			// grant key is the TARGET's directory.
			ask, _ := s.pending.get("conv_1", "perm_1")
			if ask.Tool != "orchicon_write" || len(ask.Targets) != 1 || ask.Targets[0] != "/p/sibling/notes.md" {
				t.Errorf("ask = %+v, want the tool and the correlated target", ask)
			}
			if ask.Key != "/p/sibling" {
				t.Errorf("ask key = %q, want the target's directory", ask.Key)
			}
			if _, err := s.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
				ConversationId: "conv_1", AskId: "perm_1", Choice: apiv1.PermissionChoice_ALLOW_SESSION,
			})); err != nil {
				t.Errorf("reply: %v", err)
			}
			client.sub.feed(busText("ses_live", "done"))
			client.sub.feed(busIdle("ses_live"))
		}()
		_, _, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", "ses_live", "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", func(opencodeEvent) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn did not return within 10s")
	}
	if resErr != nil {
		t.Fatalf("turn error: %v", resErr)
	}
	if !s.grants.Has("conv_1", "/p/sibling") {
		t.Fatal("allow_session must grant the target's directory")
	}
}
