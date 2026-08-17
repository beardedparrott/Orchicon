package transcript

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func part(seq int64, kind, payload string) db.SessionPart {
	return db.SessionPart{Seq: seq, Kind: kind, Payload: []byte(payload)}
}

// TestRenderTailSkippedKinds verifies the recovery-tail renderer skips the
// low-signal kinds (reasoning, step start/finish, system_prompt,
// session_info) and keeps user messages, assistant text, tool calls, and
// errors.
func TestRenderTailSkippedKinds(t *testing.T) {
	parts := []db.SessionPart{
		part(1, db.SessionPartSystemPrompt, `{"text":"big system prompt"}`),
		part(2, db.SessionPartReasoning, `{"part":{"text":"think think think"}}`),
		part(3, db.SessionPartStepStart, `{"part":{"text":"step"}}`),
		part(4, db.SessionPartUserMessage, `{"text":"Goal: build it","source":"goal"}`),
		part(5, db.SessionPartToolUse, `{"part":{"tool":"bash"}}`),
		part(6, db.SessionPartText, `{"part":{"text":"I built it."}}`),
		part(7, db.SessionPartStepFinish, `{"part":{"text":"step"}}`),
		part(8, db.SessionPartError, `{"error":"boom"}`),
		part(9, db.SessionPartSessionInfo, `{"session_id":"s1","serve_url":"u"}`),
	}
	out := RenderTail(parts, 64*1024)
	for _, want := range []string{
		"USER (goal): Goal: build it",
		"TOOL CALL: bash",
		"ASSISTANT: I built it.",
		"ERROR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tail missing %q; got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"big system prompt", "think think think", "step", "session_id", "serve_url"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("tail should skip kind, but contains %q; got:\n%s", unwanted, out)
		}
	}
}

// TestRenderTailChronologicalForTailInput verifies a tail-first (DESC)
// input is rendered chronologically.
func TestRenderTailChronologicalForTailInput(t *testing.T) {
	parts := []db.SessionPart{
		part(4, db.SessionPartUserMessage, `{"text":"later","source":"human"}`),
		part(3, db.SessionPartText, `{"part":{"text":"middle"}}`),
		part(2, db.SessionPartToolUse, `{"part":{"tool":"bash"}}`),
		part(1, db.SessionPartUserMessage, `{"text":"first","source":"goal"}`),
	}
	out := RenderTail(parts, 64*1024)
	first := strings.Index(out, "first")
	mid := strings.Index(out, "TOOL CALL")
	later := strings.Index(out, "later")
	if first == -1 || mid == -1 || later == -1 || !(first < mid && mid < later) {
		t.Errorf("tail output not chronological:\n%s", out)
	}
}

// TestRenderTailPerPartCapAndByteCap verifies the per-part cap and the hard
// byte cap are both honored and that truncation is marked.
func TestRenderTailPerPartCapAndByteCap(t *testing.T) {
	// Per-part cap: a single long message is capped at 1200 chars with the
	// per-part marker.
	long := strings.Repeat("x", 5000)
	out := RenderTail([]db.SessionPart{part(1, db.SessionPartUserMessage, `{"text":"`+long+`","source":"goal"}`)}, 64*1024)
	if !strings.Contains(out, "…(truncated)") {
		t.Errorf("per-part truncation marker missing")
	}
	if len(out) >= 5000 {
		t.Errorf("per-part cap not honored: len=%d", len(out))
	}

	// Byte cap: many parts summing beyond the cap must stop and append the
	// transcript-truncation marker.
	var parts []db.SessionPart
	for i := int64(0); i < 20; i++ {
		parts = append(parts, part(i, db.SessionPartUserMessage, `{"text":"`+strings.Repeat("y", 400)+`","source":"goal"}`))
	}
	out = RenderTail(parts, 2048)
	if !strings.Contains(out, "…(transcript truncated)") {
		t.Errorf("transcript truncation marker missing; len=%d", len(out))
	}
	// The renderer stops appending once the cap is reached, but the last
	// full part can overshoot by up to one part's rendered size.
	if len(out) > 2048+600 {
		t.Errorf("byte cap exceeded: len=%d", len(out))
	}

	// A single short part under the byte cap renders in full (no truncation
	// markers).
	short := []db.SessionPart{part(1, db.SessionPartUserMessage, `{"text":"hello","source":"goal"}`)}
	out = RenderTail(short, 2048)
	if strings.Contains(out, "truncated") {
		t.Errorf("short transcript wrongly marked truncated:\n%s", out)
	}
}

// TestRenderTailEmptyParts verifies an empty part list renders to "" without
// panicking.
func TestRenderTailEmptyParts(t *testing.T) {
	if out := RenderTail(nil, 1024); out != "" {
		t.Errorf("empty tail should render empty, got %q", out)
	}
}
