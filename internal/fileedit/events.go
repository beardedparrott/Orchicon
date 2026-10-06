package fileedit

// events.go — HOW A LEDGER ROW REACHES THE LIVE STREAMS.
//
// The operator: "The diff bar doesn't update in the GUI unless I hit refresh in the browser. My guess is,
// this issue may exist in the TUI as well" — and it did, because it was never the clients. Both read the
// same two RPCs (GetSessionFileEdits for the durable ledger, StreamFileEdits for live entries), so a gap on
// the PUBLISHING side is a gap in both at once.
//
// THE DEFECT. The publisher refused every owner kind but the execution one, and hardcoded its subject to
// match:
//
//	if ownerKind != db.FileEditOwnerExecution || pub == nil { return }
//	... eventbus.SubjectFor("execution", "file_edit") ...
//
// So an Ask conversation's edits were WRITTEN to the ledger — which is why a browser refresh, i.e. a fresh
// GetSessionFileEdits, showed them — and never PUBLISHED. The live half of StreamFileEdits had nothing to
// forward for that owner, so the diff pane sat frozen until the page was reloaded. The TUI's pane opens its
// subscription with the same owner kind (diffs.Model.SetOwner → subs.Registry.FileEdits), so it was frozen
// in exactly the same way.
//
// ONE FUNCTION NOW DECIDES WHERE A KIND'S EVENTS LIVE, used by the publisher AND the subscriber, because the
// failure above was a subject spelled out at each end and free to drift.

import (
	"encoding/json"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
)

const fileEditEventSuffix = "file_edit"

// EventSubject is the NATS subject one owner kind's file-edit rows are published on and subscribed to.
//
// THE OWNER KIND IS THE SUBJECT'S AGGREGATE, so every kind gets its own subject and no consumer has to
// receive another kind's traffic to serve its own.
func EventSubject(ownerKind string) string {
	return eventbus.SubjectFor(ownerKind, fileEditEventSuffix)
}

// EventType names the event in the envelope — what the webhook dispatcher looks subscriptions up BY.
//
// IT MUST FOLLOW THE SUBJECT'S AGGREGATE, and that is not cosmetic: the dispatcher resolves subscriptions
// with db.ListActiveSubscriptions(event_type), so an Ask edit published under the execution event type would
// have fired every webhook subscribed to EXECUTION file edits. Tying both to the owner kind keeps a
// conversation's edits out of an execution's hooks (and means a subscription to a kind's edits is possible
// at all).
func EventType(ownerKind string) string {
	return ownerKind + "." + fileEditEventSuffix
}

// EventPayload builds the subject and envelope for one ledger row.
//
// It is the whole publish decision, in one testable place: the wiring in cmd/server is a call to this and a
// Publish, so the part that was wrong before (which kind gets published, under which subject, named how) is
// pinned by a unit test instead of by reading a closure.
func EventPayload(ownerKind, ownerID string, row *db.FileEditLedgerRow) (subject string, payload []byte, err error) {
	body, err := json.Marshal(map[string]any{
		"event_type":   EventType(ownerKind),
		"tenant_id":    row.TenantID,
		"execution_id": row.OwnerID,
		"owner_kind":   ownerKind,
		"owner_id":     ownerID,
		"edit":         RowToProto(row),
		"occurred_at":  time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", nil, err
	}
	return EventSubject(ownerKind), body, nil
}
