package askorchicon

// stopped_metadata_test.go — a user stop must reach the client as a STOP, not as an error.
//
// THE BUG. Every finalize path funnelled its terminating note through a single `errText` argument,
// which persistConversationReply wrote to metadata["error"]. For a user stop that note is
// "Turn stopped by the user." — so a deliberate action was recorded as a failure.
//
// That was not cosmetic. metadata.error selects the GUI's error shell, and the error shell renders the
// error text INSTEAD of the message content. The server has always kept the partial reply (the
// errUserStop arm is commented "KEEP THE PARTIAL TEXT"), so the text was in the row and the UI threw
// it away: stopping a turn emptied the conversation, fired a "Reply failed" toast, and offered a
// Retry for something the operator had chosen to do.
//
// The fix gives a stop its own metadata field. These tests pin the WIRE half, which is a pure function
// and therefore needs no database — the persistence half is covered by the round-trip in
// internal/db and by manual verification.

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestUserStopTravelsAsStoppedNotError is the direct regression guard: a row persisted with
// stopped=true must expose `stopped` on the wire and leave `error` empty.
func TestUserStopTravelsAsStoppedNotError(t *testing.T) {
	meta, err := json.Marshal(map[string]any{
		"model_ref": "orchicon/opencode-go/deepseek-v4.1-flash",
		"stopped":   "Turn stopped by the user.",
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	want := "Orchicon orchestrates, runtimes execute."
	wire := messageRowToProto(db.MessageRow{
		ID:             "msg-1",
		ConversationID: "conv-1",
		Role:           "assistant",
		Content:        want,
		Metadata:       meta,
	})

	if wire.Metadata == nil {
		t.Fatal("no metadata on the wire")
	}
	if wire.Metadata.Error != "" {
		t.Errorf("stopped turn carried error=%q on the wire; a deliberate stop must not be an error",
			wire.Metadata.Error)
	}
	if wire.Metadata.Stopped != "Turn stopped by the user." {
		t.Errorf("stopped = %q, want %q", wire.Metadata.Stopped, "Turn stopped by the user.")
	}
	// THE POINT OF THE WHOLE FIX: the partial reply survives to the client.
	if wire.Content != want {
		t.Errorf("content = %q, want the partial reply %q", wire.Content, want)
	}
}

// TestGenuineFailureStillTravelsAsError makes sure the new field did not swallow real failures.
func TestGenuineFailureStillTravelsAsError(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"error": "provider status 429 too many requests"})
	wire := messageRowToProto(db.MessageRow{ID: "m", Role: "assistant", Metadata: meta})
	if wire.Metadata.Error == "" {
		t.Fatal("a failed turn lost its error on the wire")
	}
	if wire.Metadata.Stopped != "" {
		t.Errorf("stopped = %q on a genuine failure, want empty", wire.Metadata.Stopped)
	}
}

// TestPartialContentSurvivesAFailedTurn pins the other half of the same complaint: a turn that
// streamed text and THEN failed keeps that text. This was already true of the persistence, and the
// wire must not drop it either.
func TestPartialContentSurvivesAFailedTurn(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"error": "the opencode serve did not recover"})
	const partial = "Here is the beginning of a long answer that then died"
	wire := messageRowToProto(db.MessageRow{
		ID: "m", Role: "assistant", Content: partial, Metadata: meta,
	})
	if wire.Content != partial {
		t.Errorf("content = %q, want the partial reply the turn produced", wire.Content)
	}
	if wire.Metadata.Error == "" {
		t.Error("the failure note went missing")
	}
}
