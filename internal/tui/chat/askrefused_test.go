package chat

// askrefused_test.go — a REFUSED clarifying question is not an answer.
//
// The operator's own transcript carried this row, drawn as a settled record:
//
//	answered · ask_user could not be asked: ask_user: `question` is required and must not be empty
//
// Nothing was asked and nothing was answered. The consent layer refused the call before any card
// existed (the tool's own validator rejected it), delivered the refusal as the ask_user RESULT, and
// because that result was a SUCCESS the client read it as the operator's words — recording a decision
// they never made about a question they never saw. Three such rows were on the prod plane, every one
// with `is_error: false`.
//
// `is_error` is the fact that separates the two states, and it is the only reliable one here: the
// message text is prose.

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// askItems builds the transcript for one recorded ask_user call resolved by a result.
func askItems(t *testing.T, output string, isError bool) []ChatItem {
	t.Helper()
	return conversationItems([]*apiv1.ChatMessage{{
		Id:        "m-ask",
		Role:      "assistant",
		Content:   "Let me check that with you.",
		CreatedAt: timestamppb.Now(),
		ToolCalls: []*apiv1.ToolCall{{
			Id: "tc-ask", FunctionName: "ask_user", Arguments: "{}",
		}},
		ToolResults: []*apiv1.ToolResult{{
			ToolCallId: "tc-ask", Output: output, IsError: isError,
		}},
	}})
}

func askOf(t *testing.T, items []ChatItem) *ParsedAsk {
	t.Helper()
	for _, it := range items {
		if it.Kind == KindAsk && it.Ask != nil {
			return it.Ask
		}
	}
	t.Fatal("no ask card was produced for the recorded call")
	return nil
}

// TestARefusedQuestionIsNotAnswered is the reported row. The call resolved with an ERROR, so it must
// read as refused — never as a decision the operator made.
func TestARefusedQuestionIsNotAnswered(t *testing.T) {
	items := askItems(t, "ask_user: `question` is required and must not be empty — ask one clear question in one or two sentences.", true)
	ask := askOf(t, items)

	if ask.Answered {
		t.Fatal("a REFUSED question was recorded as ANSWERED — the operator was never shown it and never said anything")
	}
	if !ask.Refused {
		t.Fatal("the error result was not recognised as a refusal")
	}
	if !strings.Contains(ask.RefusalText, "question` is required") {
		t.Fatalf("the refusal must carry WHY, so the record explains itself; got %q", ask.RefusalText)
	}

	// And the DRAWN record must not claim an answer. This is what the operator actually read.
	out := RenderItems(items, 110)
	if strings.Contains(out, "answered") {
		t.Fatalf("the transcript still draws a refused question as an answer:\n%s", out)
	}
	if !strings.Contains(out, "not asked") {
		t.Fatalf("the record must say the question was not asked:\n%s", out)
	}
}

// The control: a genuinely ANSWERED question keeps its meaning. Without this, a fix that refused
// everything would pass the test above.
func TestAnAnsweredQuestionIsStillAnswered(t *testing.T) {
	items := askItems(t, "Use the develop branch.", false)
	ask := askOf(t, items)

	if ask.Refused {
		t.Fatal("a real answer was recorded as a refusal")
	}
	if !ask.Answered || ask.AnswerText != "Use the develop branch." {
		t.Fatalf("the operator's answer must be carried, got answered=%v text=%q", ask.Answered, ask.AnswerText)
	}
	out := RenderItems(items, 110)
	if !strings.Contains(out, "answered") {
		t.Fatalf("an answered question must still read as answered:\n%s", out)
	}
	if strings.Contains(out, "not asked") {
		t.Fatalf("an answered question must not read as refused:\n%s", out)
	}
}

// A refused question whose ARGUMENTS are also unreadable is still a refusal, not a corrupt answer —
// the arguments are exactly what is wrong with it.
func TestARefusedQuestionWithUnreadableArguments(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{{
		Id:        "m-ask-2",
		Role:      "assistant",
		Content:   "Trying.",
		CreatedAt: timestamppb.Now(),
		ToolCalls: []*apiv1.ToolCall{{
			Id: "tc-ask-2", FunctionName: "ask_user", Arguments: `{"question":`,
		}},
		ToolResults: []*apiv1.ToolResult{{
			ToolCallId: "tc-ask-2", Output: "ask_user: `question` is required", IsError: true,
		}},
	}})
	ask := askOf(t, items)
	if ask.Answered {
		t.Fatal("unreadable arguments resolved as an ERROR were recorded as answered")
	}
	if !ask.Refused {
		t.Fatal("an unreadable-and-refused question must read as refused")
	}
}
