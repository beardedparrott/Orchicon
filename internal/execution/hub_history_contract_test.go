package execution

// THE HISTORY HALF MUST SURVIVE THE HUB CHANGE.
//
// The execution page already shows full history plus live output: SessionChatPane fetches the durable
// transcript with GetExecutionSession and merges it with the live stream by timestamp
// (mergeSessionItems: `live.filter(i => itemAt(i) >= lastHistoryAt)`). Nothing in this PR replaces that —
// but the hub DOES change what "live" means, in two ways that could silently break the merge:
//
//  1. A tab that opens mid-run now receives a REPLAY of the hub's retained window before live traffic.
//     Those events are older than some of the transcript. They are still wanted (they cover the window
//     between the transcript's last flush and attach), and the merge drops anything the transcript
//     already covers — so the replay is safe BECAUSE the merge is timestamp-based, not arrival-based.
//  2. The stream now ENDS on terminal status. The pane must therefore fall back to the durable
//     transcript when the stream closes, or a finished execution would show an empty view.
//
// These assertions pin the contract between the two halves so a future change to either cannot break the
// pairing unnoticed.

import (
	"os"
	"strings"
	"testing"
)

func readFrontend(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// The merge must stay TIMESTAMP-based. If it ever becomes arrival-order-based, the hub's replay would
// duplicate or misorder history for a tab that opens mid-run.
func TestTheFrontendMergeStaysTimestampBased(t *testing.T) {
	src := readFrontend(t, "frontend/src/components/executions/sessionItems.ts")
	i := strings.Index(src, "export function mergeSessionItems")
	if i < 0 {
		t.Fatal("mergeSessionItems is gone — the execution page's history+live merge needs a replacement " +
			"before the hub's replay window can be trusted")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "itemAt(") {
		t.Error("mergeSessionItems no longer compares items by TIME. The hub replays retained events before " +
			"live traffic, so an arrival-order merge would duplicate history the transcript already " +
			"contains.")
	}
	if !strings.Contains(body, "lastHistoryAt") {
		t.Error("mergeSessionItems no longer filters live items against the transcript's latest item, so " +
			"the replay would be appended wholesale")
	}
}

// Terminal executions must render the durable transcript, not the (now closed) stream.
func TestThePaneFallsBackToTheTranscriptWhenTheStreamCloses(t *testing.T) {
	src := readFrontend(t, "frontend/src/components/executions/SessionChatPane.tsx")
	i := strings.Index(src, "const items = useMemo<ChatItem[]>(")
	if i < 0 {
		t.Fatal("the pane's item selection is gone — re-establish how it chooses transcript vs live")
	}
	body := src[i:]
	if j := strings.Index(body, "}, ["); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "mergeSessionItems(history, live)") {
		t.Error("a RUNNING execution must merge transcript history with live events, or a tab opened " +
			"mid-run shows only what happened after it attached")
	}
	if !strings.Contains(body, "history.length > 0") {
		t.Error("a TERMINAL execution must fall back to the durable transcript. The hub now CLOSES the " +
			"stream at terminal status, so a pane that rendered only the stream would show nothing once " +
			"the run finished.")
	}
}

// THE HUB NOW ENDS THE STREAM AT TERMINAL STATUS, so the pane MUST refetch the transcript on that
// transition — the runner does a final flush at the end, and the in-stream tail is now guaranteed to stop
// BEFORE that flush lands. Without this effect a finished execution would render the tail as of the last
// 2s poll, permanently.
func TestThePaneRefetchesTheTranscriptWhenTheRunEnds(t *testing.T) {
	src := readFrontend(t, "frontend/src/components/executions/SessionChatPane.tsx")
	if !strings.Contains(src, "wasRunning") {
		t.Fatal("the pane no longer refetches the transcript on the running→terminal edge. The hub " +
			"closes the stream at terminal status, so the final transcript flush would never be fetched " +
			"and a completed execution would show a truncated conversation.")
	}
	// And the polling that keeps a LIVE execution current must exist too.
	if !strings.Contains(src, "window.setInterval(() => void refetchTranscript()") {
		t.Error("the pane no longer polls the transcript while running, so a mid-run viewer would only " +
			"see the live stream's window and not the flushed transcript")
	}
}

// The durable transcript must still be fetched — that is the history the operator asked to see.
func TestThePageStillFetchesTheDurableTranscript(t *testing.T) {
	src := readFrontend(t, "frontend/src/components/executions/SessionChatPane.tsx")
	if !strings.Contains(src, "useGetExecutionSession(") {
		t.Error("the pane no longer fetches the durable session transcript (GetExecutionSession), so " +
			"\"all history from the start\" is gone")
	}
	api := readFrontend(t, "frontend/src/api/executions.ts")
	if !strings.Contains(api, "getExecutionSession(") {
		t.Error("useGetExecutionSession no longer calls GetExecutionSession")
	}
}
