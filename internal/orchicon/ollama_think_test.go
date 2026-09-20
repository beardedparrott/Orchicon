package orchicon

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ollamaChunk builds one /api/chat NDJSON stream line for tests.
func ollamaChunk(t *testing.T, content string, done bool, doneReason string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"message":     map[string]any{"content": content},
		"done":        done,
		"done_reason": doneReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// collectOllamaThink runs a scripted NDJSON stream through the native
// ollama stream and flattens text/reasoning events into T:/R: segments.
func collectOllamaThink(t *testing.T, lines []string) (string, bool) {
	t.Helper()
	s := &ollamaNativeStream{sc: bufio.NewScanner(strings.NewReader(strings.Join(lines, "\n") + "\n")), think: newThinkSplitter()}
	var b strings.Builder
	kind := byte(0)
	sawFinish := false
	ctx := context.Background()
	for {
		ev, ok, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if !ok {
			break
		}
		var text, k string
		switch e := ev.(type) {
		case TextDelta:
			text, k = e.Text, "T"
		case ReasoningDelta:
			text, k = e.Text, "R"
		case Finish:
			sawFinish = true
			continue
		default:
			t.Fatalf("unexpected event %T", ev)
		}
		if kind == 0 || kind != k[0] {
			if kind != 0 {
				b.WriteString("|")
			}
			b.WriteString(k + ":")
			kind = k[0]
		}
		b.WriteString(text)
	}
	return b.String(), sawFinish
}

func TestOllamaThinkSingleChunk(t *testing.T) {
	got, finish := collectOllamaThink(t, []string{
		ollamaChunk(t, "answer before"+tOpen+"thinking hard"+tClose+"after", false, ""),
		ollamaChunk(t, "", true, "stop"),
	})
	if want := "T:answer before|R:thinking hard|T:after"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !finish {
		t.Fatal("missing Finish")
	}
}

func TestOllamaThinkSplitAcrossChunks(t *testing.T) {
	// The open tag split mid-tag across two NDJSON chunks.
	got, _ := collectOllamaThink(t, []string{
		ollamaChunk(t, "ans"+tOpen[:4], false, ""),
		ollamaChunk(t, tOpen[4:]+"hidden reasoning"+tClose+" tail", false, ""),
		ollamaChunk(t, "", true, "stop"),
	})
	if want := "T:ans|R:hidden reasoning|T: tail"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOllamaThinkUnterminatedDrainsToReasoning(t *testing.T) {
	// A think block never closed (provider truncation at the done chunk)
	// drains to reasoning, never text.
	got, finish := collectOllamaThink(t, []string{
		ollamaChunk(t, "before "+tOpen+"never closed", false, ""),
		ollamaChunk(t, "", true, "length"),
	})
	if want := "T:before |R:never closed"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !finish {
		t.Fatal("missing Finish")
	}
}

func TestOllamaThinkPassthroughNoTags(t *testing.T) {
	got, _ := collectOllamaThink(t, []string{
		ollamaChunk(t, "just plain text", false, ""),
		ollamaChunk(t, "", true, "stop"),
	})
	if want := "T:just plain text"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
