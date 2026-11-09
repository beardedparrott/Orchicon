package askorchicon

// ask_id_uniqueness_test.go — A TOOL-CALL ID IS AN IDENTITY, SO IT MUST NOT REPEAT.
//
// REPORTED: "clicking an answer sometimes doesn't work unless I first refresh the page."
//
// The cause was a collision, and the refresh "fixing" it is the fingerprint. A ledger is created PER TURN
// (chat.go: turnLedger := newToolLedger()), so its counter restarted at 1 every turn and every turn's ask was
// minted as `tc-3`. Three separate things treat that string as an identity:
//
//	the client's settled-ask map (frontend ask-orchicon.tsx: const settledAsks = new Map<string,string>())
//	findPromotedAsk's query  (tool_calls @> '{"id": <askID>}', across the WHOLE conversation)
//	the in-process pending-ask registry
//
// So answering an ask in one turn put `tc-3` in the client's map; a later turn's ask was also `tc-3`; the card was
// drawn already-answered, every option button was disabled, and clicks did nothing. A refresh re-initialises the
// module and clears the map — hence "sometimes", and hence a refresh appearing to fix it.

import (
	"strings"
	"testing"
)

// ACROSS TURNS — the reported case. Two ledgers, as the chat path creates one per turn.
func TestToolCallIDsDoNotCollideAcrossTurns(t *testing.T) {
	turn1 := newToolLedger()
	turn1.mu.Lock()
	first := turn1.newToolCallID()
	turn1.mu.Unlock()

	turn2 := newToolLedger()
	turn2.mu.Lock()
	second := turn2.newToolCallID()
	turn2.mu.Unlock()

	if first == second {
		t.Fatalf("two turns minted the same tool-call id %q — every consumer that keys on it (the client's "+
			"settled map, findPromotedAsk, the pending registry) would confuse one turn's ask with another's, "+
			"which is exactly the dead-card bug", first)
	}
}

// ACROSS CONVERSATIONS, and within one ledger — every id distinct.
func TestToolCallIDsAreDistinctWithinALedger(t *testing.T) {
	l := newToolLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := l.newToolCallID()
		if id == "" {
			t.Fatal("minted an empty id")
		}
		if seen[id] {
			t.Fatalf("id %q minted twice within one ledger", id)
		}
		seen[id] = true
	}
}

// THE SHAPE IS PRESERVED. Nothing parses the number — the id is only ever compared for equality — but it keeps
// the `tc-` prefix every reader and log line already expects.
func TestToolCallIDKeepsTheExpectedShape(t *testing.T) {
	l := newToolLedger()
	l.mu.Lock()
	id := l.newToolCallID()
	l.mu.Unlock()
	if !strings.HasPrefix(id, "tc-") {
		t.Fatalf("id %q lost its `tc-` prefix", id)
	}
	if len(id) <= len("tc-") {
		t.Fatalf("id %q has no body", id)
	}
}

// AND THE PROMOTED ASK — the id the OPERATOR'S card is anchored to — is minted through the same
// door, so it cannot collide either.
func TestPromotedAskIDsDoNotCollideAcrossTurns(t *testing.T) {
	// A wrapped ask (opencode's `execute`) promotes a first-class call; the args are the ask payload.
	const payload = `{"recorded":true,"question":"Which branch should I use?","options":["develop","main"]}`
	promote := func() string {
		l := newToolLedger()
		l.mu.Lock()
		defer l.mu.Unlock()
		// A wrapped call whose resolution reports an ask.
		l.calls = append(l.calls, toolCallEntry{ID: "wrapped-1", Type: "function", FunctionName: "execute", resolved: true})
		l.promoteRecordedAsk("wrapped-1", payload)
		for i := len(l.calls) - 1; i >= 0; i-- {
			if isAskToolName(l.calls[i].FunctionName) {
				return l.calls[i].ID
			}
		}
		return ""
	}
	a, b := promote(), promote()
	if a == "" || b == "" {
		t.Fatalf("no promoted ask call was recorded (%q, %q)", a, b)
	}
	if a == b {
		t.Fatalf("two turns promoted their ask to the SAME id %q — the operator's answer to one card would "+
			"settle, or be delivered to, the other question", a)
	}
}
