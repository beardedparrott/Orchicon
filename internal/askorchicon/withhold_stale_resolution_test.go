package askorchicon

// withhold_stale_resolution_test.go — the LEGACY half of the card-flicker fix.
//
// Ask ids are now globally unique (orchicon.NativeBridge.nextPermID), so a new ask cannot inherit an old
// ask's recorded outcome. But ids written BEFORE that fix are still in the transcript ledger: measured on one
// conversation, native-ask-1 carries BOTH an ALLOW_ONCE from an earlier day and a live raise from a later
// one. A card raised against such an id is still settled from the stale record and then re-armed from the
// open list — the same settle/re-arm flicker, on legacy data only.
//
// The server can see the contradiction the client cannot: the ledger says the ask is finished, the pending
// registry says a turn is parked on it. The registry wins, so the historical record is withheld from the
// wire. This is the "current truth beats historical truth" rule.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ledgerMessage builds a message the way the ledger does: the ask id IS the tool call id.
func ledgerMessage(id string, verdicts ...string) *apiv1.ChatMessage {
	msg := &apiv1.ChatMessage{Id: "m-1", ConversationId: "c1"}
	for _, v := range verdicts {
		msg.ToolCalls = append(msg.ToolCalls, &apiv1.ToolCall{Id: id, FunctionName: v})
	}
	return msg
}

func verdictNames(msg *apiv1.ChatMessage) []string {
	out := make([]string, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		out = append(out, tc.GetFunctionName())
	}
	return out
}

// THE RULE. A resolution is withheld ONLY when that exact ask is open right now.
func TestStaleResolutionIsWithheldOnlyWhileItsAskIsOpen(t *testing.T) {
	// (a) the ask is OPEN: the historical ALLOW_ONCE must not reach the clients, or it settles the live
	// card and the next poll re-arms it — the flicker.
	live := ledgerMessage("native-ask-1", "permission.ask", "permission.user_PERMISSION_CHOICE_ALLOW_ONCE")
	withholdStaleResolutions(live, map[string]bool{"native-ask-1": true})
	for _, fn := range verdictNames(live) {
		if fn == "permission.user_PERMISSION_CHOICE_ALLOW_ONCE" {
			t.Fatal("a resolution was sent for an ask the server is holding OPEN — the clients settle the " +
				"live card from it and re-arm it on the next poll, which is the settle/re-arm flicker")
		}
	}
	// The RAISE is untouched: it is what tells a late client the card exists at all.
	found := false
	for _, fn := range verdictNames(live) {
		if fn == "permission.ask" {
			found = true
		}
	}
	if !found {
		t.Error("the raise record was dropped along with the resolution — a client attaching now would " +
			"not know the ask exists")
	}

	// (b) the SAME message with the ask no longer open: the resolution flows, and the card settles
	// normally. Without this the rule would leak cards forever.
	settled := ledgerMessage("native-ask-1", "permission.ask", "permission.user_PERMISSION_CHOICE_ALLOW_ONCE")
	withholdStaleResolutions(settled, map[string]bool{})
	if len(settled.ToolCalls) != 2 {
		t.Fatalf("a resolution for a CLOSED ask must reach the clients; got %v", verdictNames(settled))
	}
}

// ONLY RESOLUTIONS ARE WITHHELD. A raise (`permission.ask`, `permission.question`) and a call that proceeded
// without a card (`session_grant`, `fullsend`) must pass through untouched even for an open ask — none of
// them says anything about whether a card was answered, and dropping the raise would hide the card.
func TestOnlyResolutionVerdictsAreWithheld(t *testing.T) {
	passThrough := []string{
		"permission.ask",
		"permission.question",
		"permission.session_grant",
		"permission.fullsend",
		"permission.accept",
		"permission.project",
	}
	for _, fn := range passThrough {
		msg := ledgerMessage("native-ask-1", fn)
		withholdStaleResolutions(msg, map[string]bool{"native-ask-1": true})
		if len(msg.ToolCalls) != 1 {
			t.Errorf("%q was withheld, but it is not a resolution — it must always reach the clients", fn)
		}
	}
}

// EACH resolution verdict the clients treat as an outcome is covered, so the vocabulary here and in the two
// clients cannot drift silently into "the server withholds one thing, the clients settle on another".
func TestEveryResolutionVerdictIsWithheldWhileOpen(t *testing.T) {
	for fn := range permissionResolutionVerdicts {
		msg := ledgerMessage("native-ask-1", fn)
		withholdStaleResolutions(msg, map[string]bool{"native-ask-1": true})
		if len(msg.ToolCalls) != 0 {
			t.Errorf("%q is a resolution but was NOT withheld for an open ask", fn)
		}
	}
}

// A DIFFERENT ask's resolution is never touched — the filter keys on the id, not on the message.
func TestAnotherAsksResolutionIsUntouched(t *testing.T) {
	msg := &apiv1.ChatMessage{Id: "m-1", ConversationId: "c1"}
	msg.ToolCalls = []*apiv1.ToolCall{
		{Id: "native-ask-1", FunctionName: "permission.user_PERMISSION_CHOICE_ALLOW_ONCE"},
		{Id: "native-ask-2", FunctionName: "permission.user_PERMISSION_CHOICE_DENY"},
	}
	withholdStaleResolutions(msg, map[string]bool{"native-ask-1": true})
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].GetId() != "native-ask-2" {
		t.Fatalf("only native-ask-1 is open, so only its resolution may be withheld; got %+v", msg.ToolCalls)
	}
}

// Nil-safety: the empty cases must not panic, because this runs on every transcript page.
func TestWithholdIsNilSafe(t *testing.T) {
	withholdStaleResolutions(nil, map[string]bool{"x": true})
	withholdStaleResolutions(&apiv1.ChatMessage{}, map[string]bool{"x": true})
	withholdStaleResolutions(&apiv1.ChatMessage{}, nil)
	msg := ledgerMessage("a", "permission.user_PERMISSION_CHOICE_ALLOW_ONCE")
	withholdStaleResolutions(msg, nil) // nil set: nothing open, nothing withheld
	if len(msg.ToolCalls) != 1 {
		t.Error("a nil open-set must withhold nothing")
	}
}
