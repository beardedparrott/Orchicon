package fileedit

// events_test.go — A LEDGER ROW REACHES BOTH CLIENTS, FOR EVERY OWNER KIND.
//
// The operator: "The diff bar doesn't update in the GUI unless I hit refresh in the browser. My guess is,
// this issue may exist in the TUI as well" — and it did, because the gap was on the publishing side, which
// both clients consume. The publisher used to refuse every owner kind but the execution one, so an Ask
// conversation's edits were written to the ledger (a refresh showed them) and never published (the live
// stream forwarded nothing).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestEveryOwnerKindHasItsOwnSubject(t *testing.T) {
	// The subject's aggregate IS the owner kind, so each kind's traffic lives on its own subject and no
	// consumer has to receive another kind's events to serve its own.
	exec := EventSubject(db.FileEditOwnerExecution)
	ask := EventSubject(db.FileEditOwnerAskConversation)

	if exec != "orchicon.events.execution.file_edit" {
		t.Errorf("execution subject = %q, want the unchanged literal (existing consumers must not move)", exec)
	}
	if ask != "orchicon.events.ask_conversation.file_edit" {
		t.Errorf("ask subject = %q — an ask conversation's edits must travel on their own subject, or they "+
			"are published where their own subscriber never looks", ask)
	}
	if exec == ask {
		t.Fatal("both owner kinds share a subject")
	}
}

func TestTheEventTypeFollowsTheSubjectAggregate(t *testing.T) {
	// NOT COSMETIC. The webhook dispatcher resolves subscriptions with
	// db.ListActiveSubscriptions(event_type), so an Ask edit published under the EXECUTION event type would
	// have fired every webhook subscribed to execution file edits — a conversation's work arriving on an
	// execution's hook.
	for _, kind := range []string{db.FileEditOwnerExecution, db.FileEditOwnerAskConversation} {
		subject := EventSubject(kind)
		eventType := EventType(kind)
		// The subject is "orchicon.events.<aggregate>.<event>"; the event type is "<aggregate>.<event>".
		aggregate, event, ok := strings.Cut(strings.TrimPrefix(subject, "orchicon.events."), ".")
		if !ok {
			t.Fatalf("subject %q is not in the expected shape", subject)
		}
		if want := aggregate + "." + event; eventType != want {
			t.Errorf("EventType(%q) = %q, want %q — the event type must name the subject's aggregate, or a "+
				"webhook subscribed to one kind fires for the other", kind, eventType, want)
		}
	}
	if got := EventType(db.FileEditOwnerAskConversation); got != "ask_conversation.file_edit" {
		t.Errorf("ask event type = %q, want ask_conversation.file_edit", got)
	}
}

func TestEventPayloadCarriesEverythingASubscriberNeeds(t *testing.T) {
	row := &db.FileEditLedgerRow{
		ID: "row-1", TenantID: "tnt_dev", OwnerKind: db.FileEditOwnerAskConversation, OwnerID: "conv-9",
		Path: "src/main.go", Kind: "edit", UnifiedDiff: "@@ -1 +1 @@", Tool: "edit",
	}
	subject, payload, err := EventPayload(db.FileEditOwnerAskConversation, "conv-9", row)
	if err != nil {
		t.Fatalf("EventPayload: %v", err)
	}
	if subject != EventSubject(db.FileEditOwnerAskConversation) {
		t.Errorf("payload subject = %q, want the ask subject", subject)
	}
	var env struct {
		EventType   string `json:"event_type"`
		TenantID    string `json:"tenant_id"`
		OwnerKind   string `json:"owner_kind"`
		OwnerID     string `json:"owner_id"`
		ExecutionID string `json:"execution_id"`
		Edit        struct {
			Path string `json:"Path"`
		} `json:"edit"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// THE SUBSCRIBER'S OWN FILTER READS tenant_id AND owner_id, so a payload missing either would be
	// dropped by the very stream it was published for.
	if env.TenantID != "tnt_dev" || env.OwnerID != "conv-9" || env.OwnerKind != db.FileEditOwnerAskConversation {
		t.Errorf("envelope = %+v, want the row's tenant and owner", env)
	}
	if env.EventType != EventType(db.FileEditOwnerAskConversation) {
		t.Errorf("envelope event_type = %q, want %q", env.EventType, EventType(db.FileEditOwnerAskConversation))
	}
	if env.Edit.Path != "src/main.go" {
		t.Errorf("envelope edit = %+v, want the row's edit (the stream delivers it to the pane)", env.Edit)
	}
	if env.ExecutionID != "conv-9" {
		// Kept for the execution-published shape, where a consumer may read it instead of owner_id.
		t.Errorf("execution_id = %q, want the row's owner id (the field is the legacy alias)", env.ExecutionID)
	}
}
