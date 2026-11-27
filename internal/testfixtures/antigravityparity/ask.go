package antigravityparity

// ask.go — the CONVERSATION the mode-refused capture renders.
//
// WHY THE REFUSAL IS A RECORDED TRANSCRIPT AND NOT A LIVE TURN. The mode gate refuses a tool BEFORE any
// card is raised (internal/antigravity/modegate.go), and what the operator sees is an ERROR TOOL RESULT in
// the transcript plus a queued notice for the model. A recorded message therefore carries the entire
// observable state — the refusal text, the agy tool row that was refused — with no live agy, no live model
// and no spend. A live turn would add coin-toss variance to a capture whose claim is "these words appear".
//
// THE TOOL ROWS ARE AGY'S OWN SPELLINGS with agy's own argument keys (`CommandLine`, `TargetFile`, `Url`),
// which is what makes them worth capturing: they prove rowSubject renders a subject for an antigravity
// call rather than a truncated JSON object.

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

// The conversation's identifiers, so both captures address the same row.
const (
	ConvID        = "conv-agy-parity"
	ConvTitle     = "antigravity parity (agy)"
	ProjectID     = "proj-agy-parity"
	ModelRefAgy   = "antigravity/google/gemini-4-neon-low"
	refusalPrompt = "Create probe.txt in the project with the word ok."
)

// The refusal text is the claude/claude-parity one the agy mode gate hands back (claude.AskModeRefusal),
// quoted here because the fixture must show the WORDS an operator sees. It is asserted against the real
// generator in TestModeRefusalMatchesTheGenerator so this copy cannot drift.
const modeRefusal = "REFUSED BY THE PLATFORM: write is not available in brainstorm mode, so it was NOT " +
	"executed. Brainstorm is for thinking and planning — it does not touch the filesystem.\n\n" +
	"You cannot override this, and you cannot switch your own mode — only the user can, from the mode " +
	"selector on this conversation. Say plainly that this is a Iteration job, name the mode, and ASK " +
	"THE USER TO SWITCH TO ITERATION. Do not attempt the call again, and do not work around it with " +
	"another tool."

// AskHandler implements the conversation surface both clients read.
type AskHandler struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	p *Plane
}

// ListConversations returns the one conversation, in the Ask-capable antigravity scope.
func (h *AskHandler) ListConversations(_ context.Context, _ *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	return connect.NewResponse(&v1.ListConversationsResponse{
		Conversations: []*v1.Conversation{{
			Id: ConvID, Title: ConvTitle, ProjectId: ProjectID,
			MessageCount: 2, TurnInFlight: false,
			ModelRef: ModelRefAgy,
		}},
	}), nil
}

// GetConversation returns the one conversation.
func (h *AskHandler) GetConversation(_ context.Context, req *connect.Request[v1.GetConversationRequest]) (*connect.Response[v1.GetConversationResponse], error) {
	return connect.NewResponse(&v1.GetConversationResponse{Conversation: &v1.Conversation{
		Id: ConvID, Title: ConvTitle, ProjectId: ProjectID,
		MessageCount: 2, ModelRef: ModelRefAgy,
	}}), nil
}

// ListPendingAsks returns none: every state this fixture captures is already settled, so no card is
// pending and the transcript is the whole story.
func (h *AskHandler) ListPendingAsks(_ context.Context, _ *connect.Request[v1.ListPendingAsksRequest]) (*connect.Response[v1.ListPendingAsksResponse], error) {
	return connect.NewResponse(&v1.ListPendingAsksResponse{}), nil
}

// ListMessages serves the durable transcript NEWEST-FIRST (the server's own order; both clients reverse
// it). Every state carries the SAME user prompt so the two captures differ only in what the assistant did.
func (h *AskHandler) ListMessages(_ context.Context, _ *connect.Request[v1.ListMessagesRequest]) (*connect.Response[v1.ListMessagesResponse], error) {
	base := time.Now().Add(-3 * time.Second)

	user := &v1.ChatMessage{
		Id: "u-agy-1", ConversationId: ConvID, Role: "user",
		Content: refusalPrompt, CreatedAt: timestamppb.New(base),
	}
	assistant := &v1.ChatMessage{
		Id: "a-agy-1", ConversationId: ConvID, Role: "assistant",
		CreatedAt:   timestamppb.New(base.Add(time.Second)),
		Reasoning:   []string{agyReasoning},
		ToolCalls:   agyToolCalls(h.p.state),
		ToolResults: agyToolResults(h.p.state),
	}
	switch h.p.state {
	case StateModeRefused:
		assistant.Content = "I cannot create that file — this conversation is in **brainstorm** mode, which " +
			"is read-only for the filesystem. The platform refused `write_file` before it ran. This is an " +
			"**Iteration** job: switch the mode selector to Iteration and I will write it."
	default:
		assistant.Content = "I listed the project and read the Antigravity docs page you pointed at. " +
			"Say the word and I will run the test suite."
	}
	return connect.NewResponse(&v1.ListMessagesResponse{
		Messages: []*v1.ChatMessage{assistant, user},
	}), nil
}

// agyReasoning is a reasoning chunk. Short on purpose: the capture proves the reasoning FOLD exists and is
// labelled, not that a model can think.
const agyReasoning = "The operator wants a file written. I am in brainstorm mode, so I should check whether " +
	"writing is permitted before attempting it."

// agyToolCalls are recorded with agy's OWN names and argument keys, which is the whole point: the row
// subject has to come from `CommandLine` / `TargetFile` / `Url`, not from opencode's `command`/`file_path`.
func agyToolCalls(st State) []*v1.ToolCall {
	calls := []*v1.ToolCall{
		{
			Id: "tc-agy-1", Type: "function", FunctionName: "run_command",
			Arguments: `{"CommandLine":"go test ./internal/tui/... -run Antigravity","Cwd":"/home/op/project"}`,
		},
		{
			Id: "tc-agy-2", Type: "function", FunctionName: "view_file",
			Arguments: `{"AbsolutePath":"/home/op/project/README.md"}`,
		},
		{
			Id: "tc-agy-3", Type: "function", FunctionName: "read_url_content",
			Arguments: `{"Url":"https://antigravity.google/download"}`,
		},
	}
	if st == StateModeRefused {
		// THE REFUSED CALL: agy named it `write_file`, and the platform refused it without a card.
		calls = append(calls, &v1.ToolCall{
			Id: "tc-agy-4", Type: "function", FunctionName: "write_file",
			Arguments: `{"TargetFile":"/home/op/project/probe.txt","CodeContent":"ok\n"}`,
		})
	}
	return calls
}

// agyToolResults pairs every call with its result. The refused write's result is an ERROR carrying the
// refusal text — exactly what emitRefusal publishes on the bus.
func agyToolResults(st State) []*v1.ToolResult {
	out := []*v1.ToolResult{
		{ToolCallId: "tc-agy-1", Output: "ok  internal/tui/modelpick  0.006s"},
		{ToolCallId: "tc-agy-2", Output: "# Orchicon"},
		{ToolCallId: "tc-agy-3", Output: "Download Antigravity (200)"},
	}
	if st == StateModeRefused {
		out = append(out, &v1.ToolResult{ToolCallId: "tc-agy-4", Output: modeRefusal, IsError: true})
	}
	return out
}

// ToSNotice is re-exported so a capture test can name what it expects without importing authmethod.
func ToSNotice() string { return authmethod.ToSNotice }
