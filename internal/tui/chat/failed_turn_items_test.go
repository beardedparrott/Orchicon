package chat

// failed_turn_items_test.go — THE MESSAGE-TO-ITEM DECISION FOR A FAILED TURN.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The operator's OWN failure text, verbatim from the report's screenshot (AC 10: verify against the real
// shape, not only a synthetic string).
const operatorFailureText = "conversation session send: orchicon bridge: start Ask turn: provider status 401 Unauthorized"

func failedTurnPage(errText, modelRef string) []*apiv1.ChatMessage {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	return []*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant", Content: "", CreatedAt: at,
			Metadata: &apiv1.MessageMetadata{Error: errText, ModelRef: modelRef}},
		{Id: "m1", Role: "user", Content: "why can't you connect?", CreatedAt: at},
	}
}

// AC 1 + AC 3 + AC 10: a failed turn routes to KindError and names the reason AND the model.
func TestAFailedTurnRoutesToErrorAndNamesTheModel(t *testing.T) {
	items := conversationItems(failedTurnPage(operatorFailureText, "orchicon/ollama/deepseek-v4.1-flash"))

	var got *ChatItem
	for i := range items {
		if items[i].Kind == KindError {
			got = &items[i]
		}
	}
	if got == nil {
		t.Fatalf("a failed turn (assistant + empty content + metadata.error) did not route to KindError — it "+
			"fell through to the assistant default and drew nothing: %+v", items)
	}
	if !strings.Contains(got.Text, operatorFailureText) {
		t.Errorf("the error row does not carry the failure text: %q", got.Text)
	}
	if !strings.Contains(got.Text, "orchicon/ollama/deepseek-v4.1-flash") {
		t.Errorf("the error row does not name the model that refused (AC 3): %q", got.Text)
	}
}

// AC 4 (revised): THE ROW'S REASON MAKES NO COMPOSER CLAIM; THE SHELL APPENDS IT WHERE IT RESTORED
// THE DRAFT.
func TestTheReasonMakesNoComposerClaimAndTheShellAddsIt(t *testing.T) {
	reason := FailedTurnText("provider status 401", "m")
	if strings.Contains(reason, "back in the composer") {
		t.Errorf("the row's reason claims a composer state the shell may not have produced: %q", reason)
	}
	withLine := WithRetryAffordance(reason)
	low := strings.ToLower(withLine)
	if !strings.Contains(low, "composer") || !strings.Contains(low, "enter") {
		t.Errorf("WithRetryAffordance did not add the retry affordance (AC 4): %q", withLine)
	}
	if n := strings.Count(WithRetryAffordance(withLine), RetryAffordanceLine); n != 1 {
		t.Errorf("WithRetryAffordance is not idempotent — a later re-stamp doubled the line (%d): %q", n, WithRetryAffordance(withLine))
	}
}

// AC ORDER (AC 2): the metadata error is consulted AFTER the two named roles and BEFORE the
// assistant default — bubbleKindFor's ordering.
func TestTheErrorIsConsultedInBubbleKindForOrder(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))

	// A system row with error metadata stays a notice (role first).
	sys := conversationItems([]*apiv1.ChatMessage{
		{Id: "s1", Role: "system", Content: "a notice", CreatedAt: at,
			Metadata: &apiv1.MessageMetadata{Error: "noise"}},
	})
	if len(sys) != 1 || sys[0].Kind != KindNotice {
		t.Errorf("a system row with metadata.error was not kept as a notice — the role must be checked "+
			"first, as bubbleKindFor does: %+v", sys)
	}

	// A user row with error metadata stays the operator's own message.
	usr := conversationItems([]*apiv1.ChatMessage{
		{Id: "u1", Role: "user", Content: "hi", CreatedAt: at,
			Metadata: &apiv1.MessageMetadata{Error: "noise"}},
	})
	if len(usr) != 1 || usr[0].Kind != KindUser {
		t.Errorf("a user row with metadata.error was not kept as the operator's message: %+v", usr)
	}

	// An assistant row with an EMPTY error string is still the model's reply (the default).
	plain := conversationItems([]*apiv1.ChatMessage{
		{Id: "a1", Role: "assistant", Content: "the reply", CreatedAt: at,
			Metadata: &apiv1.MessageMetadata{Error: ""}},
	})
	if len(plain) != 1 || plain[0].Kind != KindText {
		t.Errorf("an assistant row with an empty error string is no longer the model's reply: %+v", plain)
	}
}

// AC 6: NO REGRESSION. user, system (compaction notice) and ordinary assistant text still route exactly as
// before.
func TestTheOtherRolesStillRouteAsBefore(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	items := conversationItems([]*apiv1.ChatMessage{
		{Id: "m3", Role: "assistant", Content: "the reply", CreatedAt: at},
		{Id: "m2", Role: "system", Content: "compacted 2343 messages", CreatedAt: at},
		{Id: "m1", Role: "user", Content: "hi", CreatedAt: at},
	})
	got := map[ItemKind]string{}
	for _, it := range items {
		got[it.Kind] = it.Text
	}
	if got[KindUser] != "hi" {
		t.Errorf("the operator's message did not route to KindUser: %+v", items)
	}
	if got[KindNotice] != "compacted 2343 messages" {
		t.Errorf("a system row did not route to KindNotice: %+v", items)
	}
	if got[KindText] != "the reply" {
		t.Errorf("an ordinary assistant reply did not route to KindText: %+v", items)
	}
	if _, isErr := got[KindError]; isErr {
		t.Errorf("a row with NO error metadata was routed to KindError: %+v", items)
	}
}

// AC 7: the error text stays RAW (no markdown).
func TestFailedTurnTextKeepsTheRawError(t *testing.T) {
	raw := "start Ask turn: provider status 401 Unauthorized — see *Settings → Default models*"
	out := FailedTurnText(raw, "orchicon/ollama/deepseek-v4.1-flash")
	if !strings.Contains(out, "*Settings → Default models*") {
		t.Errorf("the raw error text was mangled (AC 7): %q", out)
	}
	if !strings.Contains(out, "orchicon/ollama/deepseek-v4.1-flash") {
		t.Errorf("the model is not named: %q", out)
	}
}

// A FAILED TURN IS TWO ROWS: THE MODEL'S PROSE, THEN THE FAILURE — never one row wearing the
// error's label and colour over prose.
func TestAMidReplyFailureDrawsTheProseOnTheModelBandThenTheError(t *testing.T) {
	page := []*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant",
			Content:   "I began to answer and the provider dropped mid-sentence",
			Metadata:  &apiv1.MessageMetadata{Error: "stalled:no_progress", ModelRef: "orchicon/ollama/deepseek-v4.1-flash"},
			CreatedAt: timestamppb.New(time.Unix(1_700_000_000, 0))},
		{Id: "m1", Role: "user", Content: "why can't you connect?", CreatedAt: timestamppb.New(time.Unix(1_700_000_000, 0))},
	}
	items := conversationItems(page)

	var errIdx, proseIdx = -1, -1
	for i, it := range items {
		switch it.Kind {
		case KindError:
			errIdx = i
			if strings.Contains(it.Text, "I began to answer") {
				t.Errorf("the MODEL'S PROSE is inside the error row — that is what puts the `error` label and "+
					"the error's red over the model's own words (the operator's \"also red and on the same "+
					"line as the error\"): %q", it.Text)
			}
		case KindText:
			if strings.Contains(it.Text, "I began to answer") {
				proseIdx = i
			}
		}
	}
	if errIdx < 0 {
		t.Fatalf("the failure produced no error row: %+v", items)
	}
	if proseIdx < 0 {
		t.Errorf("the partial reply was DROPPED rather than drawn on the model's band — a failed turn that "+
			"produced prose must keep it: %+v", items)
	}
	if proseIdx >= 0 && proseIdx > errIdx {
		t.Errorf("the prose is drawn AFTER the failure it preceded (prose=%d error=%d) — the reading order "+
			"must follow the turn: %+v", proseIdx, errIdx, items)
	}
}

// AND THE PAINTED FRAME, which is the layer the operator's report is true at: the `error` label must sit on
// the FAILURE line, not in front of the model's prose.
func TestTheErrorLabelSitsOnTheFailureNotTheProse(t *testing.T) {
	page := []*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant",
			Content:   "thinking out loud before the drop",
			Metadata:  &apiv1.MessageMetadata{Error: "stalled:no_progress", ModelRef: "m"},
			CreatedAt: timestamppb.New(time.Unix(1_700_000_000, 0))},
	}
	rendered := ansi.Strip(RenderItems(conversationItems(page), 100))
	for _, ln := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "error ") {
			continue
		}
		if strings.Contains(trimmed, "thinking out loud") {
			t.Errorf("the `error` label is in front of the MODEL'S PROSE: %q", trimmed)
		}
		if !strings.Contains(trimmed, "turn failed:") {
			t.Errorf("the `error` label is not on the failure line: %q", trimmed)
		}
	}
	if !strings.Contains(rendered, "thinking out loud") {
		t.Errorf("the partial prose is gone from the frame:\n%s", rendered)
	}
}

// AND THE EMPTY-CONTENT GUARD: a failed turn with NO error text does not invent an error row.
func TestAnEmptyErrorStringIsNotAnErrorRow(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{
		{Id: "m1", Role: "assistant", Content: "", CreatedAt: timestamppb.New(time.Unix(1, 0))},
	})
	for _, it := range items {
		if it.Kind == KindError {
			t.Errorf("a row with an empty content and no error metadata became an error row: %+v", items)
		}
	}
}
