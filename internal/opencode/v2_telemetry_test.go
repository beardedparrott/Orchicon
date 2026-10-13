package opencode

// v2_telemetry_test.go — THE 2.x TELEMETRY MAPPING, and the stall bug it fixes.
//
// busevent_v2_test.go pinned the 2.x ENVELOPE (`data` not `properties`) and the
// turn-terminal vocabulary, because those made Ask hang. This file pins the
// TELEMETRY vocabulary, because its absence made a 2.x WORKER SESSION get killed
// as `text_loop` at exactly its 10-minute window while it was working.
//
// The chain, each link measured on execution 01M4SEJ37CZZ700HRQEPEVGV7C:
//
//	1. 2.x moved tools, steps, reasoning and diffs out of `message.part.updated`
//	   into their own event family (session.tool.*, session.step.*,
//	   session.reasoning.*, session.diff).
//	2. None of them were classified, so the progress monitor's
//	   lastMeaningfulAction — advanced ONLY by tool_use, step_finish and
//	   file_diff — could never advance on a 2.x serve.
//	3. Text deltas still arrived and kept `no_progress` fresh (400s window), so
//	   the FIRST signal to fire was text_loop at its 10-minute default.
//	4. The transcript shows three text parts reporting files written and a commit
//	   pending, ZERO tool rows, zero tokens, and a kill at 10:00.0.
//
// Shapes below are from the 2.x binary's own event schema. The acceptance test is
// the last one: a realistic 2.x stream must NOT look like a text loop — and the
// SAME stream with the telemetry arms removed still must, so the test
// discriminates rather than merely passing.

import (
	"encoding/json"
	"testing"
	"time"
)

// v2Bus unmarshals a v2 event the way the SSE reader does.
func v2Bus(t *testing.T, raw string) BusEvent {
	t.Helper()
	var evt BusEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return evt
}

const (
	// A completed model turn: the ONLY carrier of token usage and cost on 2.x.
	v2StepEnded = `{"type":"session.step.ended","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","finish":"stop","cost":0.0123,"tokens":{"input":1200,"output":340,"reasoning":80,"cache":{"read":900,"write":50}}}}`
	// The step token counters under their flat spelling.
	v2StepEndedFlat = `{"type":"session.step.ended","data":{"sessionID":"ses_a","cost":0.5,"tokens":{"tokens_input":11,"tokens_output":22,"tokens_reasoning":3,"tokens_cache_read":4}}}`
	// A tool call, announced across three events: the name, the input, then the id.
	v2ToolInputStarted = `{"type":"session.tool.input.started","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","id":"call_1","name":"shell"}}`
	v2ToolCalled       = `{"type":"session.tool.called","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","id":"call_1","input":{"command":"go test ./..."}}}`
	v2ToolSuccess      = `{"type":"session.tool.success","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","id":"call_1","content":[{"type":"text","text":"ok"}]}}`
	v2ToolFailed       = `{"type":"session.tool.failed","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","id":"call_2","error":{"message":"boom"}}}`
	// Reasoning has its own family on 2.x.
	v2ReasoningDelta = `{"type":"session.reasoning.delta","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","ordinal":0,"delta":"weighing the options"}}`
	v2ReasoningEnded = `{"type":"session.reasoning.ended","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","ordinal":0,"text":"weighing the options carefully"}}`
	// A turn's structured per-file diff.
	v2Diff = `{"type":"session.diff","data":{"sessionID":"ses_a","diff":[{"path":"frontend/src/App.tsx","additions":12,"deletions":3}]}}`
)

// TestV2StepEndedFeedsTheUsageRecorder is the token/cost half: it is why every
// 2.x execution recorded zero tokens, and it is the dimension the budget ladder
// and the compaction gate both read.
func TestV2StepEndedFeedsTheUsageRecorder(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"nested cache shape", v2StepEnded},
		{"flat token spelling", v2StepEndedFlat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, ok := LegacyEventFromBus(v2Bus(t, tc.raw))
			if !ok {
				t.Fatal("session.step.ended was not classified — the turn's completion and its spend are both lost")
			}
			if got, _ := legacy["type"].(string); got != "step_finish" {
				t.Fatalf("type = %q, want step_finish", got)
			}
			part, _ := legacy["part"].(map[string]any)
			tokens, _ := part["tokens"].(map[string]any)
			if tokens == nil {
				t.Fatal("no tokens on the step_finish part — recordUsage would record 0")
			}
			// The keys recordUsage reads, by name.
			for _, k := range []string{"input", "output"} {
				if _, ok := tokens[k].(float64); !ok {
					t.Fatalf("tokens[%q] is missing/not numeric: %#v — recordUsage reads exactly this key", k, tokens)
				}
			}
			if cache, _ := tokens["cache"].(map[string]any); cache == nil {
				t.Fatalf("no cache sub-object: %#v — cacheToken reads cache.read / cache.write", tokens)
			}
			if allTokensZero(tokens) {
				t.Fatalf("a real completion read as all-zero: %#v", tokens)
			}
			if _, ok := part["cost"].(float64); !ok {
				t.Errorf("cost missing from the step_finish part: %#v", part)
			}
		})
	}
}

// TestV2ToolResolutionIsShapedForTheConsumers pins the tool half: the transcript
// row, the progress monitor's meaningful-action signal and the repetition
// signature all read this v1 shape.
func TestV2ToolResolutionIsShapedForTheConsumers(t *testing.T) {
	legacy, ok := LegacyEventFromBus(v2Bus(t, v2ToolSuccess))
	if !ok {
		t.Fatal("session.tool.success was not classified — tool calls never reach the transcript")
	}
	if got, _ := legacy["type"].(string); got != "tool_use" {
		t.Fatalf("type = %q, want tool_use", got)
	}
	part, _ := legacy["part"].(map[string]any)
	state, _ := part["state"].(map[string]any)
	if got, _ := state["status"].(string); got != "completed" {
		t.Fatalf("state.status = %q, want completed (the monitor's tool-hang and repetition arms both read it)", got)
	}
	if id, _ := part["callID"].(string); id != "call_1" {
		t.Fatalf("callID = %q, want call_1 — correlation by id is how the name and input are recovered", id)
	}

	// A FAILED call must read as an error, or tier-1 repetition (which counts
	// error-status repeats) can never see a 2.x failure loop.
	failed, _ := LegacyEventFromBus(v2Bus(t, v2ToolFailed))
	fpart, _ := failed["part"].(map[string]any)
	fstate, _ := fpart["state"].(map[string]any)
	if got, _ := fstate["status"].(string); got != "error" {
		t.Fatalf("a failed tool call has status %q, want error", got)
	}
}

// TestV2ToolNameAndInputAreCorrelated is the piece a single-event mapper cannot
// do: 2.x carries the name on input.started and the input on called, and only the
// id on the resolution. Losing the INPUT is not cosmetic — the repetition
// signature is derived from it, so without it every call of one tool shares one
// signature and a busy worker trips the repetition stall.
func TestV2ToolNameAndInputAreCorrelated(t *testing.T) {
	r := &sessionRun{}

	// The stream, in the order a 2.x serve emits it.
	for _, raw := range []string{v2ToolInputStarted, v2ToolCalled} {
		evt := v2Bus(t, raw)
		props := evt.Props()
		switch evt.Type {
		case "session.tool.input.started":
			r.rememberV2Tool(props["id"], props["name"], nil)
		case "session.tool.called":
			r.rememberV2Tool(props["id"], nil, props["input"])
		}
	}

	legacy, ok := LegacyEventFromBus(v2Bus(t, v2ToolSuccess))
	if !ok {
		t.Fatal("the resolution was not classified")
	}
	r.fillV2Tool(legacy)

	part, _ := legacy["part"].(map[string]any)
	if got, _ := part["tool"].(string); got != "shell" {
		t.Fatalf("tool = %q, want shell — the name arrives on a DIFFERENT event and must be correlated by id", got)
	}
	state, _ := part["state"].(map[string]any)
	in, _ := state["input"].(map[string]any)
	if got, _ := in["command"].(string); got != "go test ./..." {
		t.Fatalf("state.input = %#v, want the input announced by session.tool.called", state["input"])
	}

	// The signature must differ per call, not collapse to one per tool.
	sig := toolUseSignature(part)
	if sig == "shell|null" || sig == "shell|<nil>" {
		t.Fatalf("the repetition signature collapsed (%q) — every call of this tool would share it", sig)
	}

	// The entry is consumed, so the map cannot grow without bound over a long run.
	r.hangMu.Lock()
	left := len(r.v2Tools)
	r.hangMu.Unlock()
	if left != 0 {
		t.Fatalf("%d remembered tool call(s) survived resolution — the map would grow for the life of the session", left)
	}
}

func TestV2ToolStartArmsTheHangWatchdog(t *testing.T) {
	for _, raw := range []string{v2ToolCalled, v2ToolInputStarted} {
		evt := v2Bus(t, raw)
		if _, isStart := ToolStartFromBus(evt); !isStart {
			t.Fatalf("%s did not arm the tool-hang window — a hung 2.x tool could never be interrupted", evt.Type)
		}
	}
	// A RESOLUTION is not a start, or the window would re-arm as it completes.
	if _, isStart := ToolStartFromBus(v2Bus(t, v2ToolSuccess)); isStart {
		t.Error("a resolved tool call armed the hang window")
	}
}

func TestV2ReasoningIsClassified(t *testing.T) {
	// The delta is liveness, and on 2.x it arrives under its own family.
	if delta, kind, ok := TokenDeltaInfoFromBus(v2Bus(t, v2ReasoningDelta)); !ok || delta == "" {
		t.Fatalf("session.reasoning.delta produced (%q, %q, %v) — a reasoning-heavy turn would have no liveness evidence", delta, kind, ok)
	} else if kind != "reasoning" {
		t.Errorf("kind = %q, want reasoning", kind)
	}
	// The completed block is the transcript entry.
	legacy, ok := LegacyEventFromBus(v2Bus(t, v2ReasoningEnded))
	if !ok {
		t.Fatal("session.reasoning.ended was not classified — reasoning would never appear in the transcript")
	}
	if got, _ := legacy["type"].(string); got != "reasoning" {
		t.Fatalf("type = %q, want reasoning", got)
	}
}

func TestV2DiffBecomesAFileDiff(t *testing.T) {
	legacy, ok := LegacyEventFromBus(v2Bus(t, v2Diff))
	if !ok {
		t.Fatal("session.diff was not classified — no_file_progress can never clear")
	}
	if got, _ := legacy["type"].(string); got != "file_diff" {
		t.Fatalf("type = %q, want file_diff", got)
	}
	part, _ := legacy["part"].(map[string]any)
	if got, _ := part["path"].(string); got != "frontend/src/App.tsx" {
		t.Fatalf("path = %q, want the changed file", got)
	}
}

// v2Monitor drives a monitor from RAW 2.x bus events through the real mappers,
// exactly as session_run.handleEvent does: deltas become the "text" liveness
// observation, everything else becomes its legacy event.
func v2Monitor(m *progressMonitor, raw string) {
	var evt BusEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		return
	}
	if _, ok := TokenDeltaFromBus(evt); ok {
		m.observe("text", nil)
		return
	}
	if legacy, ok := LegacyEventFromBus(evt); ok {
		et, _ := legacy["type"].(string)
		part, _ := legacy["part"].(map[string]any)
		m.observe(et, part)
	}
}

// TestAV2WorkingStreamIsNotATextLoop is THE ACCEPTANCE TEST for the bug this file
// exists for: a healthy 2.x worker must not be killed as `text_loop`.
//
// It runs the tenant's real windows (no_progress 400s, text_loop at its 10-minute
// default) against a realistic 2.x stream — streaming text, tool calls that
// resolve, steps that end, a diff — and requires NO stall to fire.
func TestAV2WorkingStreamIsNotATextLoop(t *testing.T) {
	w := stallWindows{
		noProgress:  400 * time.Second,
		noFileDiff:  15 * time.Minute,
		textLoop:    10 * time.Minute,
		repetitionN: 5,
		repetitionW: 300 * time.Second,
	}
	c := &clock{t: time.Now()}
	m := newProgressMonitor("exe_v2_ok", w)
	m.now = c.now
	m.startedAt, m.lastStepFinish, m.lastFileDiff, m.lastMeaningfulAction = c.t, c.t, c.t, c.t

	// 11 minutes — PAST the 10-minute text_loop window that killed the real run.
	for elapsed := 0; elapsed < 11*60; elapsed += 30 {
		c.t = c.t.Add(30 * time.Second)
		// Text streams continuously (this is what keeps no_progress quiet, and
		// what made the OLD behaviour look like a pure text loop).
		v2Monitor(m, v2TextDelta)
		switch elapsed {
		case 0, 120, 240, 360, 480, 600:
			// A tool call, announced and resolved: the meaningful action.
			v2Monitor(m, v2ToolInputStarted)
			v2Monitor(m, v2ToolCalled)
			v2Monitor(m, v2ToolSuccess)
		case 180, 420, 540:
			// A completed turn: tokens and cost.
			v2Monitor(m, v2StepEnded)
		case 300:
			v2Monitor(m, v2Diff)
		}
		if reason := m.check(); reason != "" {
			t.Fatalf("a healthy 2.x stream tripped a stall at %s elapsed: %q", time.Duration(elapsed)*time.Second, reason)
		}
	}
}

// TestAV2StreamWithOnlyTextWouldHaveTripped is the CONTRAST that makes the test
// above mean something: with the telemetry arms absent — the state of the world
// that killed the real execution — the very same streaming text IS a text loop.
// If this ever stops tripping, the test above has stopped discriminating.
func TestAV2StreamWithOnlyTextWouldHaveTripped(t *testing.T) {
	w := stallWindows{
		noProgress:  400 * time.Second,
		noFileDiff:  15 * time.Minute,
		textLoop:    10 * time.Minute,
		repetitionN: 5,
		repetitionW: 300 * time.Second,
	}
	c := &clock{t: time.Now()}
	m := newProgressMonitor("exe_v2_old", w)
	m.now = c.now
	m.startedAt, m.lastStepFinish, m.lastFileDiff, m.lastMeaningfulAction = c.t, c.t, c.t, c.t

	// ONLY the events the old mapping classified: text deltas. Everything else
	// was dropped on the floor.
	for elapsed := 0; elapsed < 11*60; elapsed += 30 {
		c.t = c.t.Add(30 * time.Second)
		m.observe("text", nil) // what a 2.x text delta used to reduce to
	}

	if reason := m.check(); reason == "" {
		t.Fatal("text-only streaming did NOT trip text_loop — this test no longer demonstrates the bug it is the contrast for")
	} else if len(reason) < len("stalled:text_loop") || reason[:len("stalled:text_loop")] != "stalled:text_loop" {
		t.Fatalf("expected a text_loop trip, got %q", reason)
	}
}
