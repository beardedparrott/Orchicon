package orchicon

// compaction_observable_test.go — a mid-run collapse must reach the operator, all the way from the engine that
// performed it to the part the run page reads.
//
// THE FAILURE THIS PINS. A worker execution failed 4 minutes and 30 tool rounds in with
// `400 The reasoning_content in the thinking mode must be passed back to the API`. Its history had been
// collapsed under it moments earlier — the offload file on disk was the ONLY trace. The engine logged nothing
// on success, wrote no transcript event, and therefore stored no session part, so the first request the
// provider rejected after the collapse could not be correlated with anything an operator can see. The other two
// adapters (opencode, claude) had always written the part; the native engine — the one in play — did not, and
// no renderer drew it anyway.
//
// The chain is: doCompact → TransCompaction transcript event → sessionPartsRecorder → db.SessionPartCompacted.

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// THE END-TO-END CHAIN: a real compaction, through the real recorder, lands as the part every renderer reads.
func TestACompactionEmitsTheSessionPartTheRunPageReads(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000000)
	s.appendUser(TransUserMessage, "goal", "goal")
	for i := 0; i < 12; i++ {
		r := "thinking about round " + string(rune('a'+i))
		s.appendAssistantToolUse("", []ToolCall{{
			Index: 0, ToolCallID: "call-" + string(rune('a'+i)), Name: "bash", ArgsJSON: `{"command":"ls"}`,
		}}, r)
		s.appendToolResults([]toolResult{{
			ToolCall: ToolCall{ToolCallID: "call-" + string(rune('a'+i)), Name: "bash"}, Output: "ok",
		}})
	}

	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_compact", "tnt_test")
	defer rec.Close() // pump never started — Close must not block
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	s.transcript.SetObserver(func(seq int64, typ string, data []byte) { rec.observe(seq, typ, data) })

	// doCompact is always reached from maybeCompact, which folds the turn's live usage into the spend
	// accumulator first — that snapshot is what makes a spend-driven trigger verifiable afterwards.
	s.cs.spend.AddFromUsage(214000, 31000, 23000, 0, 0.31)

	before := len(s.history)
	if !s.doCompact(9, "budget:tokens:escalate") {
		t.Fatalf("expected a compaction over %d messages", before)
	}

	rec.flush()
	parts := partsOf(store)
	var compacted []map[string]any
	for _, p := range parts {
		if p.Kind != "compacted" {
			continue
		}
		var pl map[string]any
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			t.Fatalf("compacted part payload is not JSON: %v", err)
		}
		compacted = append(compacted, pl)
	}
	if len(compacted) != 1 {
		t.Fatalf("got %d compacted parts, want exactly 1 (kinds: %v)", len(compacted), kindsOf(parts))
	}
	pl := compacted[0]
	if pl["reason"] != "budget:tokens:escalate" {
		t.Errorf("the part does not name the trigger that fired: %v", pl["reason"])
	}
	// The trigger is only answerable if the part carries WHY it fired: a spend-driven collapse and a
	// window-pressure collapse look identical from the transcript alone.
	if n, _ := pl["fresh_tokens"].(float64); n != 268000 {
		t.Errorf("the part's spend snapshot is %v, want 268000 (prompt+completion+reasoning) — without it "+
			"nobody can verify a spend-driven trigger after the fact", pl["fresh_tokens"])
	}
	if after, _ := pl["messages_after"].(float64); int(after) != len(s.history) {
		t.Errorf("messages_after = %v, history holds %d", pl["messages_after"], len(s.history))
	}
	if evicted, _ := pl["tool_rounds_evicted"].(float64); evicted == 0 {
		t.Errorf("the part claims no eviction, but the history shrank from %d to %d", before, len(s.history))
	}
}

// AND THE ENGINE SAYS IT IN THE LOG. The run page is one reader; a plane's logs are the other, and a collapse
// missing from both is what made this failure take a live box to explain.
func TestACompactionIsLogged(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000000)
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	s.appendUser(TransUserMessage, "goal", "goal")
	for i := 0; i < 8; i++ {
		s.appendAssistantToolUse("", []ToolCall{{Index: 0, ToolCallID: "c" + string(rune('a'+i)), Name: "bash", ArgsJSON: `{}`}}, "")
		s.appendToolResults([]toolResult{{ToolCall: ToolCall{ToolCallID: "c" + string(rune('a'+i)), Name: "bash"}, Output: "ok"}})
	}
	var logged []string
	s.log = slog.New(slog.NewTextHandler(slogWriterFunc(func(p []byte) (int, error) {
		logged = append(logged, string(p))
		return len(p), nil
	}), nil))
	if !s.doCompact(5, "turn_count") {
		t.Fatal("expected a compaction")
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"context compacted", "reason=turn_count"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the compaction log line is missing %q; got:\n%s", want, joined)
		}
	}
}

// A resume must NOT re-apply a collapse: the transcript part is a record, not an instruction. Replay rebuilds
// history from the tool_call/user_message parts, which already reflect the post-compaction state.
func TestReplayIgnoresTheCompactionRecord(t *testing.T) {
	s := reasoningSession()
	payload, _ := json.Marshal(map[string]any{"step": 9, "reason": "budget:tokens:escalate", "messages_before": 61, "messages_after": 12})
	s.replay([]replayEvent{{Type: TransCompaction, Data: payload}})
	if len(s.history) != 0 {
		t.Fatalf("replay turned a compaction record into %d history messages", len(s.history))
	}
}

// slogWriterFunc lets a test capture the session's structured log lines.
type slogWriterFunc func([]byte) (int, error)

func (f slogWriterFunc) Write(p []byte) (int, error) { return f(p) }

func kindsOf(parts []db.SessionPart) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Kind)
	}
	return out
}
