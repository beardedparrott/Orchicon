package chat

// execsession_compacted_test.go — a mid-run collapse must be a ROW, on both surfaces.
//
// REPORTED (a worker execution that failed ~4 minutes and 30 tool rounds in with a provider 400 on
// `reasoning_content`): the run's history had been collapsed under it mid-run and NOTHING said so. The
// opencode and claude adapters had always written a `compacted` session part, the native engine wrote none,
// and — this is the half that made the failure undiagnosable — neither renderer drew the part anyway, so it
// existed for all three adapters and was visible for none.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)



// THE NATIVE ENGINE'S OWN PAYLOAD: it names what it evicted and the spend that fired the trigger.
func TestHistoryItemsRendersANativeCompactionAsANotice(t *testing.T) {
	items := HistoryItems([]*apiv1.ExecutionSessionPart{
		part(1, "user_message", `{"text":"add location search","source":"goal"}`),
		part(2, "compacted", `{"step":29,"reason":"budget:tokens:escalate","messages_before":61,"messages_after":12,"tool_rounds_evicted":27,"fresh_tokens":268000,"cost_usd":0.31}`),
	})
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (goal + the notice): %+v", len(items), items)
	}
	notice := items[1]
	if notice.Kind != KindNotice {
		t.Fatalf("the compaction rendered as %q, want %q — the platform did this, not the model", notice.Kind, KindNotice)
	}
	for _, want := range []string{
		"Context compacted mid-run at step 29",
		"61 messages collapsed to 12",
		"27 tool round(s) were moved to the offload file",
		"Trigger: budget:tokens:escalate",
		"carries the collapsed history",
	} {
		if !strings.Contains(notice.Text, want) {
			t.Errorf("notice text is missing %q:\n%s", want, notice.Text)
		}
	}
}

// THE OTHER ADAPTERS' PAYLOADS MUST RENDER TOO — the part was written by opencode and claude long before this
// and drew nowhere. Fixing only the native writer would leave two adapters still silent.
func TestHistoryItemsRendersOpencodeAndClaudeCompactions(t *testing.T) {
	cases := map[string]string{
		"opencode": `{"step":12,"reason":"window_pressure","provider":"opencode-go","model":"deepseek-v4.1-flash"}`,
		"claude":   `{"step":4,"reason":"manual","provider":"anthropic","model":"claude-sonnet-4","scope":"session"}`,
		"claude auto": `{"source":"claude_auto_compact"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			items := HistoryItems([]*apiv1.ExecutionSessionPart{part(7, "compacted", payload)})
			if len(items) != 1 || items[0].Kind != KindNotice {
				t.Fatalf("got %+v, want one notice", items)
			}
			if !strings.Contains(items[0].Text, "Context compacted mid-run") {
				t.Errorf("notice text does not say what happened: %q", items[0].Text)
			}
		})
	}
}

// AND A PART THAT NAMES NOTHING RECOGNISABLE STILL RENDERS: losing the fact that context was collapsed because
// an adapter used different field names is exactly how it went unseen.
func TestHistoryItemsRendersAnOpaqueCompaction(t *testing.T) {
	items := HistoryItems([]*apiv1.ExecutionSessionPart{part(3, "compacted", `{}`)})
	if len(items) != 1 || items[0].Kind != KindNotice {
		t.Fatalf("an empty compaction payload rendered %+v, want one notice", items)
	}
}
