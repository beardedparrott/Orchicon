package chat

// toolrows_test.go — the TUI's durable tool-call rows.
//
// THE DEFECT THIS PINS. conversationItems turned a message's prose, reasoning and recorded ask card into
// items and then DROPPED its tool calls, so tool rows existed only while the SSE stream was live and every
// call a turn had made VANISHED as soon as the turn finalized. That is invisible on the native/opencode
// adapters (their recorded calls are mostly one-line reads) and it is the whole transcript for an
// antigravity turn, where the calls ARE the record of what agy did.
//
// THE SUBJECT KEYS ARE AGY'S OWN (`CommandLine`, `AbsolutePath`, `TargetFile`, `Url`), because the recorded
// input keeps agy's spelling even after canonicalisation. A row that knew only opencode's `command`/
// `file_path` drew the name and nothing else.

import (
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// agyMessage is one recorded assistant turn carrying agy's own tool calls and their results.
func agyMessage() *apiv1.ChatMessage {
	return &apiv1.ChatMessage{
		Id:        "a-agy-1",
		Role:      "assistant",
		Content:   "I could not write that file.",
		CreatedAt: timestamppb.Now(),
		ToolCalls: []*apiv1.ToolCall{
			{Id: "tc-1", FunctionName: "run_command", Arguments: `{"CommandLine":"go test ./internal/tui/... -run Antigravity","Cwd":"/home/op/project"}`},
			{Id: "tc-2", FunctionName: "view_file", Arguments: `{"AbsolutePath":"/home/op/project/README.md"}`},
			{Id: "tc-3", FunctionName: "read_url_content", Arguments: `{"Url":"https://antigravity.google/download"}`},
			{Id: "tc-4", FunctionName: "write_file", Arguments: `{"TargetFile":"/home/op/project/probe.txt","CodeContent":"ok\n"}`},
		},
		ToolResults: []*apiv1.ToolResult{
			{ToolCallId: "tc-1", Output: "ok  internal/tui/modelpick  0.006s"},
			{ToolCallId: "tc-2", Output: "# Orchicon"},
			{ToolCallId: "tc-3", Output: "Download Antigravity (200)"},
			{ToolCallId: "tc-4", Output: "REFUSED BY THE PLATFORM: write is not available in brainstorm mode", IsError: true},
		},
	}
}

// TestDurableToolCallsBecomeRows is the fix itself: a recorded turn's calls appear in the transcript.
func TestDurableToolCallsBecomeRows(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{agyMessage()})

	var tools []*ParsedTool
	for _, it := range items {
		if it.Kind == KindTool && it.Tool != nil {
			tools = append(tools, it.Tool)
		}
	}
	// ONE ROW PER CALL, in ledger order. The turn also carries a text row, which must not be disturbed.
	if len(tools) != 4 {
		t.Fatalf("recorded tool calls produced %d rows, want 4 — a dropped call is a turn with no record "+
			"of the work it did (items: %+v)", len(tools), items)
	}
	wantNames := []string{"run_command", "view_file", "read_url_content", "write_file"}
	for i, want := range wantNames {
		if tools[i].ToolName != want {
			t.Errorf("row %d = %q, want %q", i, tools[i].ToolName, want)
		}
	}
	if !containsKind(items, KindText) {
		t.Errorf("the turn's prose must survive beside its tool rows: %+v", items)
	}
}

// TestDurableToolRowsShowTheAgySubject is the parity claim on the row itself: the subject comes from agy's
// own argument keys, so the operator reads the command, the path or the URL — not a JSON blob.
func TestDurableToolRowsShowTheAgySubject(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{agyMessage()})
	subjects := map[string]string{}
	for _, it := range items {
		if it.Kind != KindTool || it.Tool == nil {
			continue
		}
		subjects[it.Tool.ToolName] = toolclass.CollapseWhitespace(
			toolclass.RowSubject(it.Tool.ToolName, it.Tool.Input))
	}
	want := map[string]string{
		"run_command":      "go test ./internal/tui/... -run Antigravity",
		"view_file":        "/home/op/project/README.md",
		"read_url_content": "https://antigravity.google/download",
		"write_file":       "/home/op/project/probe.txt",
	}
	for tool, wantSubject := range want {
		if subjects[tool] != wantSubject {
			t.Errorf("%s subject = %q, want %q (agy's own key: CommandLine/AbsolutePath/TargetFile/Url)",
				tool, subjects[tool], wantSubject)
		}
	}
}

// TestDurableToolRowPairsItsOwnResult proves the pairing is by ID, not by position: the refused write
// carries the platform's refusal text, which is exactly what AC3's mode-refused frame asserts.
func TestDurableToolRowPairsItsOwnResult(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{agyMessage()})
	for _, it := range items {
		if it.Kind == KindTool && it.Tool != nil && it.Tool.ToolName == "write_file" {
			if !contains(it.Tool.Output, "REFUSED BY THE PLATFORM") {
				t.Fatalf("the refused call's row lost its refusal text: %q", it.Tool.Output)
			}
			return
		}
	}
	t.Fatal("no write_file row to check")
}

// TestDurableToolRowsSkipCardsAndPermissionRecords is the boundary: an ask_user call is its CARD and a
// permission ledger row is the card's RESOLUTION, so neither becomes a row of its own.
func TestDurableToolRowsSkipCardsAndPermissionRecords(t *testing.T) {
	m := &apiv1.ChatMessage{
		Id: "a-1", Role: "assistant", CreatedAt: timestamppb.Now(),
		ToolCalls: []*apiv1.ToolCall{
			{Id: "c1", FunctionName: "ask_user", Arguments: `{"question":"which?","options":[]}`},
			{Id: "c2", FunctionName: "permission.user_PERMISSION_CHOICE_DENY"},
			{Id: "c3", FunctionName: "run_command", Arguments: `{"CommandLine":"ls"}`},
		},
	}
	rows := toolRowsForMessage(m)
	if len(rows) != 1 || rows[0].Tool == nil || rows[0].Tool.ToolName != "run_command" {
		t.Fatalf("rows = %+v, want exactly the run_command — a card and a permission record are not rows", rows)
	}
}

// TestDurableToolRowsKeepAnIdlessOutput keeps a ledger recorded without ids readable rather than silent.
func TestDurableToolRowsKeepAnIdlessOutput(t *testing.T) {
	m := &apiv1.ChatMessage{
		Id: "a-1", Role: "assistant", CreatedAt: timestamppb.Now(),
		ToolCalls:   []*apiv1.ToolCall{{FunctionName: "run_command", Arguments: `{"CommandLine":"pwd"}`}},
		ToolResults: []*apiv1.ToolResult{{Output: "/home/op/project"}},
	}
	rows := toolRowsForMessage(m)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].Tool.Output != "/home/op/project" {
		t.Errorf("an id-less call lost its output: %q", rows[0].Tool.Output)
	}
}

func containsKind(items []ChatItem, kind ItemKind) bool {
	for _, it := range items {
		if it.Kind == kind {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
