package orchicon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// askreduce_live_test.go — END-TO-END verification of the context recovery
// against a REAL wedged conversation, not a synthetic fixture.
//
// The transcript is a real (and potentially large/private) conversation history,
// so it is never committed: the test is gated on ORCHICON_ASK_WEDGE_HISTORY
// pointing at a history JSON file, and SKIPS when unset. This mirrors the
// repo's other live-gate convention (e.g. ORCH_PTY_SMOKE for the PTY gates): the
// evidence is reproducible on demand without private data in the tree.
//
// CRITICAL: the test operates on a COPY in a temp directory. The recovery path
// PERSISTS its reduced history (that is what makes the fix permanent), so
// pointing the bridge at the source directory would REWRITE THE LIVE FILE. The
// temp copy keeps the test read-only with respect to real data while still
// exercising the real code path and the real persistence.
//
// Run it against the conversation that motivated the work:
//
//	ORCHICON_ASK_WEDGE_HISTORY=/var/lib/orchicon/ask-history/orchicon-ask_<id>.json \
//	  go test ./internal/orchicon/ -run TestRealWedgeHistory -v

// askWedgeHistoryEnv names the environment variable that points at a real
// history file for the live verification.
const askWedgeHistoryEnv = "ORCHICON_ASK_WEDGE_HISTORY"

// wedgeWindowTokens is the window the real failure reported
// ("maximum context length is 1048576 tokens").
const wedgeWindowTokens = 1_048_576

func TestRealWedgeHistoryIsRescuedByTheReactivePath(t *testing.T) {
	path := strings.TrimSpace(os.Getenv(askWedgeHistoryEnv))
	if path == "" {
		t.Skipf("set %s to a real Ask history JSON to run this end-to-end verification", askWedgeHistoryEnv)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("history file unavailable: %v", err)
	}

	// Load through the SAME path a turn uses, so this exercises the real
	// deserialization (envelope version, message shape) rather than a fixture.
	//
	// The bridge is pointed at a TEMP COPY, never the source directory: the
	// recovery persists its reduced history, so a bridge aimed at the real data
	// dir would rewrite the operator's live conversation file.
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	tmpDir := t.TempDir()
	sid := strings.TrimSuffix(filepath.Base(path), ".json")
	if err := os.WriteFile(filepath.Join(tmpDir, sid+".json"), src, 0o600); err != nil {
		t.Fatalf("stage the temp copy: %v", err)
	}
	originalSize := int64(len(src))

	b := &NativeBridge{
		log:           testLogger(),
		askHistoryDir: tmpDir,
	}
	history := b.loadAskHistoryLocked(sid)
	if len(history) == 0 {
		t.Fatalf("loaded 0 messages from %s — wrong session id or unreadable envelope", path)
	}
	// The source file must not be touched by this test.
	t.Cleanup(func() {
		if now, err := os.ReadFile(path); err == nil && int64(len(now)) != originalSize {
			t.Errorf("the SOURCE history was modified by this test (%d -> %d bytes) — it must be read-only",
				originalSize, len(now))
		}
	})

	// The premise: this history is genuinely over the window. If it were not, the
	// test would be proving nothing.
	beforeBytes := conversationBytes(history)
	beforeTokens := beforeBytes / askCompactBytesPerToken
	t.Logf("loaded %d messages, %d bytes (~%d tokens at %d bytes/token)",
		len(history), beforeBytes, beforeTokens, askCompactBytesPerToken)
	if beforeTokens <= wedgeWindowTokens {
		t.Skipf("history is ~%d tokens, under the %d window — not a wedged conversation, nothing to prove",
			beforeTokens, wedgeWindowTokens)
	}

	// Drive the REAL recovery: the provider refuses with the EXACT error the
	// wedged conversation produced, then accepts the reduced request.
	b.chatHistory = map[string][]Message{sid: history}
	realErr := fmt.Errorf("orchicon bridge: start Ask turn: provider status 400 400 Bad Request: " +
		`{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1049352 tokens (1016584 in the messages, 32768 in the completion). Please reduce the length of the messages"}}`)
	if !isContextLengthError(realErr) {
		t.Fatal("the real provider error must be recognized, or recovery never triggers for this exact case")
	}
	prov := &scriptedProvider{errs: []error{realErr, nil}}
	req := &TurnRequest{Messages: history}

	if _, err := b.startTurnWithContextRecovery(context.Background(), prov, req, sid); err != nil {
		t.Fatalf("the wedged conversation must be rescued, got: %v", err)
	}

	// Evidence: the request that the provider ACCEPTED now fits.
	afterBytes := conversationBytes(req.Messages)
	afterTokens := afterBytes / askCompactBytesPerToken
	t.Logf("rescued: %d bytes -> %d bytes (~%d tokens -> ~%d tokens)",
		beforeBytes, afterBytes, beforeTokens, afterTokens)
	if afterTokens >= wedgeWindowTokens {
		t.Errorf("the reduced request is still ~%d tokens — it would be rejected again", afterTokens)
	}
	// The rescue must be a LARGE reduction, not a marginal trim: the whole point
	// is that the dominating payloads were removed. The measured figure for a real
	// wedged conversation is ~96% (the remaining bytes are conversation TEXT,
	// which this pass deliberately preserves).
	if afterBytes >= beforeBytes/2 {
		t.Errorf("reduced only %d%% — expected the dominating payloads to be removed",
			100-afterBytes*100/beforeBytes)
	}

	// And the session's persisted history is the reduced one, so the NEXT turn
	// does not re-send the oversized original (this is what makes the recovery
	// permanent rather than a one-turn reprieve).
	if got := conversationBytes(b.chatHistory[sid]); got != afterBytes {
		t.Errorf("the bridge's history is %d bytes, want the reduced %d — the next turn would re-send the original", got, afterBytes)
	}

	// Structure survived: a dropped tool pairing is itself a 400.
	if hasDanglingToolCalls(b.chatHistory[sid]) {
		t.Error("the reduced history has a dangling tool call — the provider would reject it")
	}

	// Report what was reclaimed, for the record.
	_, st := ReduceConversationHistoryForContext(history, 0)
	t.Logf("full-reduce stats: images=%d tool_results=%d text_truncated=%d reclaimed=%d bytes",
		st.ImagesDropped, st.ToolResultsElided, st.TextPartsTruncated, st.Reclaimed())
	if st.ImagesDropped == 0 && st.ToolResultsElided == 0 {
		t.Log("NOTE: no images or tool results were found to elide in this history")
	}
}
