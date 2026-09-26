package askorchicon

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNativeToolResolutionBackfillsArgumentsAndResult pins the fix for the
// missing card: an adapter that reports a RESOLVED call through the typed,
// adapter-neutral fields (SessionEvent Kind "tool_result") must land in the
// ledger with its real arguments and its real result — the same ledger the
// opencode-shaped Part map lands in.
//
// Before this, the native Ask adapter emitted only the tool START, so the ledger
// kept the "{}" placeholder forever and the terminal write synthesised "tool
// call aborted" for every call. An ask_user card had no question and no options
// to render, and every tool result in the transcript read as aborted.
//
// This is deliberately a DIFFERENT test from
// TestAskUserCallCollectedAndRecordedInLedger, which drives the opencode-shaped
// assist (busAskCompleted builds a message.part.updated Part map with
// state.input). That test passed the whole time the native path was broken,
// because it exercised the other dialect — which is exactly how this defect
// survived.
func TestNativeToolResolutionBackfillsArgumentsAndResult(t *testing.T) {
	led := newToolLedger()
	// The start carries only the tool name — all the native bus emitted.
	led.recordStart("ask_user")

	args := `{"question":"Which branch should the run clone off?","options":[{"label":"develop","description":"the integration branch"},{"label":"main","description":"the release branch"}]}`
	led.recordToolResolution("ask_user", args, `{"recorded":true}`, false)

	calls, results := led.snapshot()

	var parsedCalls []struct {
		FunctionName string `json:"function_name"`
		Arguments    string `json:"arguments"`
	}
	if err := json.Unmarshal(calls, &parsedCalls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v", err)
	}
	if len(parsedCalls) != 1 || parsedCalls[0].FunctionName != "ask_user" {
		t.Fatalf("tool_calls = %s, want exactly one ask_user call", calls)
	}

	// THE ASSERTION THE CARD DEPENDS ON: the recorded call round-trips into the
	// exact structure the client's card builder parses out of `arguments`.
	var got struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(parsedCalls[0].Arguments), &got); err != nil {
		t.Fatalf("recorded arguments are not the call object (%q): %v", parsedCalls[0].Arguments, err)
	}
	if !strings.Contains(got.Question, "clone off") {
		t.Errorf("recorded question = %q, want it intact", got.Question)
	}
	if len(got.Options) != 2 {
		t.Fatalf("recorded options = %d, want 2 — a card with no options is unanswerable", len(got.Options))
	}

	var parsedResults []struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(results, &parsedResults); err != nil {
		t.Fatalf("unmarshal tool_results: %v", err)
	}
	if len(parsedResults) != 1 || parsedResults[0].Output != `{"recorded":true}` {
		t.Fatalf("tool_results = %s, want the real result rather than the aborted placeholder", results)
	}
	if parsedResults[0].IsError {
		t.Error("a successful resolution must not be recorded as an error")
	}
}

// TestToolResolutionKeepsPlaceholderWhenNoArgumentsArrive: a resolution with no
// usable arguments must not rewrite a recorded call's arguments into something
// emptier. An adapter without arguments support leaves the placeholder alone.
func TestToolResolutionKeepsPlaceholderWhenNoArgumentsArrive(t *testing.T) {
	led := newToolLedger()
	led.recordStart("bash")
	led.recordToolResolution("bash", "{}", "ok", false)

	calls, _ := led.snapshot()
	var parsed []struct {
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(calls, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 || parsed[0].Arguments != "{}" {
		t.Fatalf("arguments = %q, want the placeholder left alone", parsed[0].Arguments)
	}
}

// TestToolResolutionWithoutStartKeepsTheResult: a resolution whose start event
// was missed must still be recorded, with the arguments the resolution carries.
// Same rule recordResolve applies for the opencode dialect.
func TestToolResolutionWithoutStartKeepsTheResult(t *testing.T) {
	led := newToolLedger()
	led.recordToolResolution("bash", `{"command":"ls"}`, "file.txt", false)

	calls, results := led.snapshot()
	var parsedCalls []struct {
		FunctionName string `json:"function_name"`
		Arguments    string `json:"arguments"`
	}
	if err := json.Unmarshal(calls, &parsedCalls); err != nil {
		t.Fatal(err)
	}
	if len(parsedCalls) != 1 || parsedCalls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("want a synthesized call carrying the args, got %s", calls)
	}
	var parsedResults []struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(results, &parsedResults); err != nil {
		t.Fatal(err)
	}
	if len(parsedResults) != 1 || parsedResults[0].Output != "file.txt" {
		t.Fatalf("want the result kept, got %s", results)
	}
}

// TestToolResolutionMarksErrors: a failed call must not be recorded as a
// success, or a client renders a failed tool as though it worked.
func TestToolResolutionMarksErrors(t *testing.T) {
	led := newToolLedger()
	led.recordStart("bash")
	led.recordToolResolution("bash", `{"command":"false"}`, "exit status 1", true)

	_, results := led.snapshot()
	var parsed []struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(results, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 || !parsed[0].IsError {
		t.Fatalf("a failed call must be recorded as an error, got %s", results)
	}
}

// TestToolResolutionResolvesOnlyOneCallPerEvent: two identical tool calls in one
// round (a real pattern — two bash calls) must resolve one each, not both from
// the first resolution. Otherwise the second call stays unresolved and is
// written back as "aborted" while its result is attributed to the first.
func TestToolResolutionResolvesOnlyOneCallPerEvent(t *testing.T) {
	led := newToolLedger()
	led.recordStart("bash")
	led.recordStart("bash")
	led.recordToolResolution("bash", `{"command":"first"}`, "one", false)
	led.recordToolResolution("bash", `{"command":"second"}`, "two", false)

	calls, results := led.snapshot()
	var parsedCalls []struct {
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(calls, &parsedCalls); err != nil {
		t.Fatal(err)
	}
	if len(parsedCalls) != 2 {
		t.Fatalf("want 2 recorded calls, got %d", len(parsedCalls))
	}
	if parsedCalls[0].Arguments != `{"command":"first"}` || parsedCalls[1].Arguments != `{"command":"second"}` {
		t.Fatalf("each resolution must attach to its own call, got %q then %q",
			parsedCalls[0].Arguments, parsedCalls[1].Arguments)
	}
	var parsedResults []struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(results, &parsedResults); err != nil {
		t.Fatal(err)
	}
	if len(parsedResults) != 2 || parsedResults[0].Output != "one" || parsedResults[1].Output != "two" {
		t.Fatalf("results misattributed: %s", results)
	}
}
