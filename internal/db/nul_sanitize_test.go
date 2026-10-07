package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// A NUL in a message's content killed the whole write — both the 250ms live mirror and the terminal reply —
// with "unsupported Unicode escape sequence (SQLSTATE 22P05)" on a jsonb argument. The two shapes it takes are
// pinned here, because a fix that handled only one of them would look correct and still lose turns.
func TestSanitizeMessageStripsBothNulShapes(t *testing.T) {
	toolCalls, _ := json.Marshal([]map[string]any{
		{"name": "bash", "output": "binary\x00payload"},
	})
	metadata, _ := json.Marshal(map[string]any{"model_ref": "m", "session_id": "s\x00x"})

	m := sanitizeMessage(MessageRow{
		Content:     "reply with a NUL:\x00here",
		ToolCalls:   toolCalls,
		ToolResults: []byte(`[{"output":"\u0000"}]`),
		Attachments: []byte(`[{"name":"\u0000"}]`),
		Metadata:    metadata,
		Reasoning:   []string{"part one\x00", "part two"},
	})

	// The TEXT column: the raw byte must be gone.
	if strings.ContainsRune(m.Content, 0x00) {
		t.Fatalf("content still carries a raw NUL: %q", m.Content)
	}
	if !strings.Contains(m.Content, "\uFFFD") {
		t.Fatalf("content must show that something was replaced: %q", m.Content)
	}
	// The JSON columns: Go never emits a raw NUL, so what reaches PostgreSQL is the ESCAPE — and jsonb is
	// what rejects it. Every jsonb field must be free of it.
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"tool_calls", m.ToolCalls},
		{"tool_results", m.ToolResults},
		{"attachments", m.Attachments},
		{"metadata", m.Metadata},
	} {
		if strings.Contains(string(c.b), `\u0000`) {
			t.Fatalf("%s still carries the \\u0000 escape jsonb rejects: %s", c.name, c.b)
		}
	}
	// The reasoning array is marshaled to jsonb AFTER sanitizing (see UpsertMessage), so what matters is both
	// the value and its marshaling.
	for i, r := range m.Reasoning {
		if strings.ContainsRune(r, 0x00) {
			t.Fatalf("reasoning[%d] still carries a raw NUL: %q", i, r)
		}
	}
	reasoningJSON, err := json.Marshal(m.Reasoning)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(reasoningJSON), `\u0000`) {
		t.Fatalf("reasoning jsonb still carries \\u0000: %s", reasoningJSON)
	}
}

// The common case must not allocate a copy or disturb a byte: a message with no NUL is written exactly as it
// was (this is on the 250ms mirror's hot path).
func TestSanitizeMessageLeavesCleanRowsAlone(t *testing.T) {
	calls := []byte(`[{"name":"bash"}]`)
	m := sanitizeMessage(MessageRow{
		Content:     "ordinary reply",
		ToolCalls:   calls,
		ToolResults: []byte(`[]`),
		Reasoning:   []string{"clean"},
	})
	if m.Content != "ordinary reply" || m.Reasoning[0] != "clean" {
		t.Fatalf("a clean row was altered: %+v", m)
	}
	if &calls[0] != &m.ToolCalls[0] {
		t.Fatal("a clean jsonb column was copied unnecessarily")
	}
}

// jsonSafe is nil-tolerant: the write path passes nil for absent columns.
func TestJSONSafeHandlesEmptyAndNil(t *testing.T) {
	if got := jsonSafe(nil); got != nil {
		t.Fatalf("jsonSafe(nil) = %v, want nil", got)
	}
	if got := jsonSafe([]byte{}); len(got) != 0 {
		t.Fatalf("jsonSafe(empty) = %v, want empty", got)
	}
	clean := []byte(`{"a":1}`)
	if got := jsonSafe(clean); &got[0] != &clean[0] {
		t.Fatal("jsonSafe copied a NUL-free payload")
	}
}
