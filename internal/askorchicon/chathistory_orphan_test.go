package askorchicon

// The MIRROR of the dangling-tool-call report (chathistory.go). The operator
// compacted Ask conversation 01M2C8VXFQY5ZE26PYBSNKA2CA with the new compact
// button and then sent a message; the compaction had kept a tail that began on a
// tool result whose assistant tool_calls message was collapsed away, so the
// provider rejected the replayed history with the third pairing shape:
//
//	Messages with role 'tool' must be a response to a preceding message with
//	'tool_calls'
//
// This payload must classify as a dangling tool-pairing rejection, or the repair
// path in chat.go (drop the poisoned session, dispatch fresh next turn) never
// runs and the conversation stays wedged on every send.

import (
	"encoding/json"
	"testing"
)

// fixtureProviderErrorOrphanedToolResult is the operator's verbatim payload for
// the compact-button regression.
const fixtureProviderErrorOrphanedToolResult = `{"error":{"message":"Messages with role 'tool' must be a response to a preceding ` +
	`message with 'tool_calls'","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`

func TestIsDanglingToolCallProviderErrorMatchesOrphanedResultPayload(t *testing.T) {
	if !json.Valid([]byte(fixtureProviderErrorOrphanedToolResult)) {
		t.Fatalf("fixture is not valid JSON: %s", fixtureProviderErrorOrphanedToolResult)
	}

	// The unknown-unless-classified case: this is the shape the operator hit, and
	// it must not be invisible to the repair path.
	wrapped := "conversation session send: orchicon bridge: start Ask turn: provider status 400 400 Bad Request: " +
		fixtureProviderErrorOrphanedToolResult
	if !isDanglingToolCallProviderError(wrapped) {
		t.Fatal("the orphaned-tool-result rejection is not recognized — a poisoned session would never be dropped")
	}

	// All THREE shapes must classify, so fixing one cannot regress another.
	for _, payload := range []string{
		fixtureProviderErrorNoToolOutput,
		fixtureProviderErrorUnansweredToolCalls,
		fixtureProviderErrorOrphanedToolResult,
	} {
		if !isDanglingToolCallProviderError(payload) {
			t.Fatalf("payload no longer classified as a tool-pairing rejection: %s", payload)
		}
	}

	// The new marker must not swallow unrelated failures.
	for _, other := range []string{
		"",
		"request timed out after 60s",
		"provider status 429 too many requests",
		"turn stopped by the user",
		"provider status 400 400 Bad Request: {\"error\":{\"message\":\"This model's maximum context length is 1048576 tokens.\"}}",
	} {
		if isDanglingToolCallProviderError(other) {
			t.Fatalf("non-pairing error classified as a dangling rejection: %q", other)
		}
	}
}
