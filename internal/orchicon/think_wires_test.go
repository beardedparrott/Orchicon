package orchicon

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// flattenWire flattens a drained stream into "T:text"/"R:reasoning"
// segments (same merge rule as collect in thinksplit_test.go) and
// reports whether a Finish event arrived.
func flattenWire(t *testing.T, ts TurnStream) (string, bool) {
	t.Helper()
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	var b strings.Builder
	kind := byte(0)
	sawFinish := false
	for _, ev := range evs {
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

func anthropicTextStream(t *testing.T, texts []string) TurnStream {
	t.Helper()
	var frames string
	for _, text := range texts {
		frames += sseEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+quoteJSON(t, text)+`}}`)
	}
	frames += sseEvent("message_stop", `{"type":"message_stop"}`)
	return newAnthropicStream(io.NopCloser(strings.NewReader(frames)))
}

func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	q, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(q)
}

func TestAnthropicThinkInlineRoutesToReasoning(t *testing.T) {
	ts := anthropicTextStream(t, []string{"answer before" + tOpen + "thinking hard" + tClose + "after"})
	got, finish := flattenWire(t, ts)
	if want := "T:answer before|R:thinking hard|T:after"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !finish {
		t.Fatal("missing Finish")
	}
}

func TestAnthropicThinkSplitAcrossDeltas(t *testing.T) {
	ts := anthropicTextStream(t, []string{"ans" + tOpen[:4], tOpen[4:] + "hidden" + tClose + " tail"})
	got, _ := flattenWire(t, ts)
	if want := "T:ans|R:hidden|T: tail"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLegacyThinkInlineRoutesToReasoning(t *testing.T) {
	body := sse(`{"type":"text-delta","delta":"answer before`+tOpen+`thinking hard`+tClose+`after"}`)
	ts := &legacyStream{r: newSSEReader(strings.NewReader(body)), think: newThinkSplitter()}
	got, finish := flattenWire(t, ts)
	if want := "T:answer before|R:thinking hard|T:after"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !finish {
		t.Fatal("missing Finish")
	}
}

func TestLegacyThinkSplitAcrossDeltas(t *testing.T) {
	body := sse(
		`{"type":"text-delta","delta":"ans`+tOpen[:4]+`"}`,
		`{"type":"text-delta","delta":"`+tOpen[4:]+`hidden`+tClose+` tail"}`,
	)
	ts := &legacyStream{r: newSSEReader(strings.NewReader(body)), think: newThinkSplitter()}
	got, _ := flattenWire(t, ts)
	if want := "T:ans|R:hidden|T: tail"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
