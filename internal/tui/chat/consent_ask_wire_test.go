package chat

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// --- the converter (pure) --------------------------------------------------

// TestPermissionAskFromProtoNamesTheCommandForAnExecution: the card must name
// WHAT is being approved. For an execution that is the command, which the wire
// carries in its own field rather than in targets.
func TestPermissionAskFromProtoNamesTheCommandForAnExecution(t *testing.T) {
	got := PermissionAskFromProto(&apiv1.PermissionAsk{
		AskId:     "ask-1",
		Tool:      "bash",
		Command:   "rm -rf /tmp/x",
		Directory: "/tmp",
		Summary:   "bash rm -rf /tmp/x",
	})
	if got.Target != "rm -rf /tmp/x" {
		t.Errorf("Target = %q, want the command — an approval card that does not say what it approves is unusable", got.Target)
	}
	if got.Tool != "bash" || got.Directory != "/tmp" || got.ID != "ask-1" {
		t.Errorf("mapping lost a field: %+v", got)
	}
	if got.Kind != AskTool {
		t.Errorf("Kind = %q, want %q (a permission ask is the tool decision, never the clarifying question)", got.Kind, AskTool)
	}
}

// TestPermissionAskFromProtoNamesThePathForAWrite: a write ask carries its
// paths in Targets (and no command), so the card names the path.
func TestPermissionAskFromProtoNamesThePathForAWrite(t *testing.T) {
	got := PermissionAskFromProto(&apiv1.PermissionAsk{
		AskId:     "ask-2",
		Tool:      "write",
		Targets:   []string{"/etc/hosts", "/etc/hostname"},
		Directory: "/etc",
	})
	if got.Target != "/etc/hosts" {
		t.Errorf("Target = %q, want the first target path", got.Target)
	}
}

// TestPermissionAskFromProtoCarriesTheDenyEntry: the card must be able to STATE
// the rule that denies the target, so it can disable the session-grant row
// instead of offering a choice the policy will refuse.
func TestPermissionAskFromProtoCarriesTheDenyEntry(t *testing.T) {
	got := PermissionAskFromProto(&apiv1.PermissionAsk{
		AskId:            "ask-3",
		Tool:             "write",
		Targets:          []string{"~/.ssh/id_rsa"},
		Directory:        "~/.ssh",
		DenyEntriesBelow: []string{"~/.ssh/**", "~/.ssh/known_hosts"},
	})
	if got.DeniedBy != "~/.ssh/**" {
		t.Errorf("DeniedBy = %q, want the first deny entry at or below the directory", got.DeniedBy)
	}
}

// TestPermissionAskFromProtoToleratesAnEmptyAsk: a malformed/absent ask must not
// panic or invent a directory. ShowConsentAsk fills a missing id.
func TestPermissionAskFromProtoToleratesAnEmptyAsk(t *testing.T) {
	if got := PermissionAskFromProto(nil); got.ID != "" || got.Target != "" || got.Directory != "" {
		t.Errorf("nil ask mapped to %+v, want the zero value", got)
	}
	if got := PermissionAskFromProto(&apiv1.PermissionAsk{}); got.Kind != AskTool {
		t.Errorf("Kind = %q, want %q even for an empty ask", got.Kind, AskTool)
	}
}

// TestPermissionAskFromProtoDoesNotLeakAWhitespaceCommand: a blank command must
// fall through to the path rather than producing a card that names nothing.
func TestPermissionAskFromProtoDoesNotLeakAWhitespaceCommand(t *testing.T) {
	got := PermissionAskFromProto(&apiv1.PermissionAsk{
		Tool:    "write",
		Command: "   ",
		Targets: []string{"/tmp/a.txt"},
	})
	if got.Target != "/tmp/a.txt" {
		t.Errorf("Target = %q, want the path — a whitespace command is not a command", got.Target)
	}
}

// --- the wire arm (through the real stream handler) ------------------------

// TestHandleEventEmitsConsentAsk is the regression guard for the missing wire
// arm. The server has always built the ask, the proto has always carried it, and
// the shell has always had a hook for it — but nothing read the field, so no
// permission card could ever render in the TUI, for this adapter or any other.
func TestHandleEventEmitsConsentAsk(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	rec := &recorder{conn: map[string]bool{}}
	cmds := make(chan tea.Cmd, 16)
	c.Bind(rec, cmds)

	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_PermissionAsk{
			PermissionAsk: &apiv1.PermissionAsk{
				AskId:     "ask-9",
				Tool:      "bash",
				Command:   "cp /etc/hostname /tmp/probe",
				Directory: "/tmp",
				Summary:   "bash cp /etc/hostname /tmp/probe",
			},
		},
	})

	var asks []ConsentAskMsg
	for _, cmd := range drainCmds(t, cmds, 1) {
		if msg, ok := cmd().(ConsentAskMsg); ok {
			asks = append(asks, msg)
		}
	}
	if len(asks) != 1 {
		t.Fatalf("the stream handler emitted %d consent asks, want exactly 1 — without this the card never reaches the shell", len(asks))
	}
	if asks[0].ConvID != "c1" {
		t.Errorf("ConvID = %q, want the conversation the ask belongs to (not whatever is on screen)", asks[0].ConvID)
	}
	if asks[0].Ask.ID != "ask-9" || asks[0].Ask.Target != "cp /etc/hostname /tmp/probe" || asks[0].Ask.Directory != "/tmp" {
		t.Errorf("ask = %+v, want the wire fields mapped through", asks[0].Ask)
	}
	if asks[0].Ask.Kind != AskTool {
		t.Errorf("Kind = %q, want %q", asks[0].Ask.Kind, AskTool)
	}
}

// TestHandleEventOtherSignalsEmitNoConsentAsk is the CONTROL: ordinary stream
// traffic must not fabricate a card. Without it, the assertion above could pass
// for a handler that emits asks unconditionally.
func TestHandleEventOtherSignalsEmitNoConsentAsk(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	rec := &recorder{conn: map[string]bool{}}
	cmds := make(chan tea.Cmd, 16)
	c.Bind(rec, cmds)

	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TextChunk{TextChunk: &apiv1.TextChunk{Content: "hello"}},
	})
	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_ToolCallResult{
			ToolCallResult: &apiv1.ToolCallResult{ToolCallId: "tc-1", Output: "ok"},
		},
	})

	// Drain whatever the handler queued WITHOUT blocking: drainCmds(n) waits for
	// exactly n, so passing 0 returns nothing and cannot assert "nothing was
	// sent" — the control would have been vacuous.
	var got []tea.Cmd
	for {
		queued := false
		select {
		case cmd := <-cmds:
			got = append(got, cmd)
			queued = true
		default:
		}
		if !queued {
			break
		}
	}
	for _, cmd := range got {
		if msg, ok := cmd().(ConsentAskMsg); ok {
			t.Fatalf("an ordinary stream signal fabricated a consent card: %+v", msg)
		}
	}
}
