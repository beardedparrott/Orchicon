package antigravity

// mode_pin_test.go — the MODE PIN: agy must not run in whatever mode happens to be its default, because
// that default is not the same on Linux, macOS and Windows.
//
// THE DEFECT PROVEN HERE. `session/set_mode` had no non-test caller, so every execution inherited agy's
// own current mode. agy's `auto` mode approves edits and "safe" commands without asking and needs the
// OS terminal sandbox (macOS sandbox-exec, Linux user namespaces, Win11 24H2+), so the same worker on
// the same prompt got a different amount of autonomy per platform — a permission policy nobody wrote
// and no reviewer saw. `pinMode` fixed it by pinning `default`; these tests assert the wire actually
// carries that request, before the first prompt.

import (
	"testing"
)

// TestBridge_PinsDefaultMode is the primary AC for the pin: a session that agy opens in some OTHER mode
// must be moved to `default`, and the request must arrive BEFORE the first `session/prompt`.
func TestBridge_PinsDefaultMode(t *testing.T) {
	h := newHarness(t)
	// agy advertises `default` but is CURRENTLY in `auto` — the per-OS default that must not survive.
	h.serve(script{modes: &sessionModes{
		CurrentModeID: "auto",
		AvailableModes: []modeOption{
			{ID: "default", Name: "Default"},
			{ID: "auto", Name: "Auto"},
			{ID: "accept-edits", Name: "Accept edits"},
			{ID: "plan", Name: "Plan"},
		},
	}})
	h.startAndWait()

	proc := h.procAt(0)
	if proc == nil {
		t.Fatal("no fake child was spawned")
	}
	got := proc.modesSetSeen()
	if len(got) != 1 {
		t.Fatalf("session/set_mode was sent %d times (%v), want exactly ONE pin to %q — agy's own default "+
			"varies by OS, so an unpinned session behaves differently on Linux, macOS and Windows for the same "+
			"tenant and prompt", len(got), got, agyPinnedMode)
	}
	if got[0] != agyPinnedMode {
		t.Fatalf("session/set_mode modeId = %q, want %q — `auto` pre-approves edits and needs the OS terminal "+
			"sandbox, which is the per-platform behavioural split the pin exists to remove",
			got[0], agyPinnedMode)
	}
	// AND BEFORE THE TURN. A mode set after the first prompt would leave that prompt's tool calls in
	// agy's own mode, which is the half-pinned state that looks correct in a log and is not. The fake
	// records the methods in ARRIVAL order so this is an ordering assertion, not a counter check.
	order := proc.methodsSeen()
	setIdx, promptIdx := -1, -1
	for i, m := range order {
		if m == "session/set_mode" && setIdx < 0 {
			setIdx = i
		}
		if m == "session/prompt" && promptIdx < 0 {
			promptIdx = i
		}
	}
	if promptIdx < 0 {
		t.Fatalf("the harness never sent a prompt (order=%v); the ordering assertion would be vacuous", order)
	}
	if setIdx < 0 || setIdx > promptIdx {
		t.Fatalf("session/set_mode arrived at index %d and the first session/prompt at %d (order=%v) — the mode "+
			"must be pinned BEFORE the first prompt, or that turn's tool calls run under agy's own per-OS mode",
			setIdx, promptIdx, order)
	}
}

// TestBridge_SkipsModePinWhenAlreadyDefault is the no-op arm: a session agy already opened in `default`
// must NOT receive a redundant request. Sending it would make "this run chose a mode" and "this run
// inherited one" indistinguishable on the wire.
func TestBridge_SkipsModePinWhenAlreadyDefault(t *testing.T) {
	h := newHarness(t)
	h.serve(script{modes: &sessionModes{
		CurrentModeID: "default",
		AvailableModes: []modeOption{
			{ID: "default", Name: "Default"},
			{ID: "auto", Name: "Auto"},
		},
	}})
	h.startAndWait()

	got := h.procAt(0).modesSetSeen()
	if len(got) != 0 {
		t.Fatalf("session/set_mode was sent %v for a session already in %q; want NO request", got, agyPinnedMode)
	}
}

// TestBridge_ModePinToleratesOlderAgy is the compatibility arm: an agy build that does not advertise
// `default` must not be handed a request it would reject with -32602. The run proceeds (with a warning
// in the log) rather than failing, which is the pre-existing behaviour for such a build.
func TestBridge_ModePinToleratesOlderAgy(t *testing.T) {
	h := newHarness(t)
	h.serve(script{modes: &sessionModes{
		// An older/limited build: only one mode, and it is not the pinned one.
		CurrentModeID:  "accept-edits",
		AvailableModes: []modeOption{{ID: "accept-edits", Name: "Accept edits"}},
	}})
	h.startAndWait()

	proc := h.procAt(0)
	if got := proc.modesSetSeen(); len(got) != 0 {
		t.Fatalf("session/set_mode was sent %v for an unadvertised mode; agy would reject it with -32602 and "+
			"fail a run that has no reason to fail", got)
	}
	// THE RUN STILL COMPLETES. Tolerating the absence must not mean hanging or erroring.
	res := h.awaitResult()
	if !res.OK {
		t.Fatalf("the execution did not complete with an agy that omits the pinned mode: %+v", res)
	}
}

// TestParseSessionModes covers the decoder the pin relies on, including the blobs agy actually sends.
func TestParseSessionModes(t *testing.T) {
	// The exact `modes` object recorded from agy 1.5.13 (simple_turn.jsonl).
	recorded := `{"currentModeId":"default","availableModes":[{"id":"default","name":"Default",` +
		`"description":"Ask before edits and commands, per your permission settings."},` +
		`{"id":"accept-edits","name":"Accept edits"},{"id":"auto","name":"Auto"},{"id":"plan","name":"Plan"}]}`
	m := parseSessionModes([]byte(recorded))
	if m.CurrentModeID != "default" {
		t.Errorf("CurrentModeID = %q, want default", m.CurrentModeID)
	}
	if len(m.AvailableModes) != 4 {
		t.Fatalf("parsed %d modes, want 4: %+v", len(m.AvailableModes), m.AvailableModes)
	}
	for _, want := range []string{"default", "accept-edits", "auto", "plan"} {
		if !m.advertisesMode(want) {
			t.Errorf("advertisesMode(%q) = false, want true", want)
		}
	}
	if m.advertisesMode("plan-mode") {
		t.Error("advertisesMode(plan-mode) = true, want false — a partial id must not match")
	}

	// An absent blob is the zero value, not a panic: an older agy may omit `modes` entirely.
	for _, raw := range []string{``, `null`, `{}`, `not json`} {
		got := parseSessionModes([]byte(raw))
		if got.advertisesMode(agyPinnedMode) {
			t.Errorf("parseSessionModes(%q) advertises %q; an undecodable blob must advertise nothing", raw, agyPinnedMode)
		}
	}

	// A MODE ID IS MATCHED CASE-INSENSITIVELY, because agy title-cases names and an id that is spelled
	// `Default` in a future release must still pin rather than silently skipping.
	if !(sessionModes{AvailableModes: []modeOption{{ID: "Default"}}}).advertisesMode(agyPinnedMode) {
		t.Error("a mode id spelled `Default` must match the pinned `default`")
	}
}

// TestAgyPinnedModeIsDefault pins the DECISION itself so a future edit has to argue with this comment:
// `auto` is the mode that needs a per-OS terminal sandbox, so it can never be the pin.
func TestAgyPinnedModeIsDefault(t *testing.T) {
	if agyPinnedMode != "default" {
		t.Fatalf("agyPinnedMode = %q, want \"default\" — every other agy mode either pre-approves edits "+
			"(`auto`, `accept-edits`) or suppresses tool use (`plan`), and `auto` additionally requires the OS "+
			"terminal sandbox, which is the cross-platform divergence this work item removes", agyPinnedMode)
	}
}
