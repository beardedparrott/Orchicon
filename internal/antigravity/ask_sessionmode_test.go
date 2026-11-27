package antigravity

// ask_sessionmode_test.go — the agy EXECUTION MODE, which is what actually stops a write in a mode that
// may not act.
//
// THE GAP THESE TESTS CLOSE. The Ask mode boundary was enforced by the permission handler alone, plus a
// `--settings` deny list. That is insufficient on agy 1.5.13, measured twice against the real binary: agy
// does NOT send `session/request_permission` before a file edit in its default mode, so the handler is
// never consulted and the write lands; and the `--settings` `permissions.deny` key is inert, so the deny
// list does not prevent it either. `--mode plan` DOES: reads still work, the write never reaches disk.
//
// So there are now three assertions that matter, and each one can fail independently:
//  1. the argv carries `--mode plan` (before `acp`) for a mode that may not act, and carries no `--mode`
//     for one that may;
//  2. a mid-conversation mode change is issued as `session/set_mode`, because argv is fixed at spawn;
//  3. the in-handler gate still refuses WITHOUT a card — that half was always right and must not regress.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskModeGate_SessionModeIsSetOnTheSession is AC3's spawn half, and it asserts the ONLY mechanism that
// works.
//
// WHY NOT THE ARGV. `agy --mode plan acp` is ACCEPTED (exit 0) but INEFFECTIVE: a free probe of
// `initialize` + `session/new` against agy 1.5.13 reports `modes.currentModeId = "default"` with the flag
// exactly as without it, and a LIVE turn with the flag still wrote its file. `session/set_mode` on the live
// session is what actually changes the mode, so that is what these tests demand — an argv assertion would
// pass while the boundary was breached.
func TestAskModeGate_SessionModeIsSetOnTheSession(t *testing.T) {
	cases := []struct {
		mode string
		want []string // the exact set_mode calls the spawn must issue
		why  string
	}{
		{askmode.Brainstorm, []string{agySessionModePlan},
			"Brainstorm may not act, and agy never cards a file edit, so the write must be PREVENTED"},
		{askmode.QuickWork, []string{agySessionModePlan},
			"Quick Work dispatches rather than acts — the same boundary as Brainstorm"},
		{askmode.Iteration, nil,
			"Iteration is the hands: it must be able to write, so agy's default mode is left alone"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			h := newAskHarness(t, script{})
			h.create(tc.mode)
			got := h.proc(0).modeIdSets()
			if len(got) != len(tc.want) {
				t.Fatalf("%s: session/set_mode calls = %v, want %v (%s)", tc.mode, got, tc.want, tc.why)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: session/set_mode calls = %v, want %v (%s)", tc.mode, got, tc.want, tc.why)
				}
			}
		})
	}
}

// TestAskModeGate_ModeSwitchRestrictsTheChild is the mid-conversation half: an operator who opens in
// Iteration and switches to Brainstorm must end up with a session that CANNOT write, or the boundary is a
// claim rather than an enforcement.
func TestAskModeGate_ModeSwitchRestrictsTheChild(t *testing.T) {
	h := newAskHarness(t, script{})
	// Open in Iteration: agy's own default, so no set_mode at all.
	h.create(askmode.Iteration)
	if got := h.proc(0).modeIdSets(); len(got) != 0 {
		t.Fatalf("opening in Iteration issued session/set_mode %v; it is agy's default already", got)
	}

	// The SAME conversation, now driven in Brainstorm.
	errCh := h.sendTurn(h.turnCtx(askmode.Brainstorm), h.sessionID(), askmode.Brainstorm, "hello")
	h.procOrNil(0).release()
	if err := <-errCh; err != nil {
		t.Fatalf("the Brainstorm turn failed: %v", err)
	}

	// Whichever child is serving the conversation now (a mode change to a may-not-act mode also changes the
	// MCP set and can respawn) must be in plan mode. The harness keeps every fake, so the LAST is the live one.
	live := h.latestProc()
	got := live.modeIdSets()
	if len(got) == 0 || got[len(got)-1] != agySessionModePlan {
		t.Fatalf("the child serving the Brainstorm turn received session/set_mode %v; it must end in %q, or "+
			"a write lands uncarded in a mode that forbids writes", got, agySessionModePlan)
	}
}

// TestAskModeGate_PrepareTurnModeIssuesSetModeOnce asserts the two properties of prepareTurnMode itself: it
// issues the switch when the mode CHANGED, and never re-issues it on later turns of the same mode.
func TestAskModeGate_PrepareTurnModeIssuesSetModeOnce(t *testing.T) {
	h := newAskHarness(t, script{})
	h.create(askmode.Brainstorm)
	fp := h.proc(0)
	// The spawn already applied it (that is what makes the FIRST turn constrained).
	if got := fp.modeIdSets(); len(got) != 1 || got[0] != agySessionModePlan {
		t.Fatalf("the spawn issued session/set_mode %v, want exactly [%s]", got, agySessionModePlan)
	}

	s := h.b.ensureAskSession(h.con)
	// A second turn in the same mode must not re-send it.
	if err := s.prepareTurnMode(h.turnCtx(askmode.Brainstorm)); err != nil {
		t.Fatalf("prepareTurnMode: %v", err)
	}
	if got := fp.modeIdSets(); len(got) != 1 {
		t.Fatalf("session/set_mode calls = %v after a turn in the mode already applied, want one", got)
	}

	// And a change back to a mode that may act must switch the session out of plan, not leave it stuck.
	s.mu.Lock()
	s.mode = askmode.Iteration
	s.mu.Unlock()
	if err := s.prepareTurnMode(h.turnCtx(askmode.Iteration)); err != nil {
		t.Fatalf("prepareTurnMode (iteration): %v", err)
	}
	got := fp.modeIdSets()
	if len(got) != 2 || got[1] != agySessionModeDefault {
		t.Fatalf("session/set_mode calls = %v, want a second call with modeId %q so an Iteration turn is not "+
			"stuck in plan", got, agySessionModeDefault)
	}
}

// TestAskModeGate_UnknownModeDoesNotRestrict pins the default: an unstamped mode must not invent `plan`,
// because that would silently stop a conversation nobody told us was Brainstorm from writing at all.
func TestAskModeGate_UnknownModeDoesNotRestrict(t *testing.T) {
	if got := agySessionMode(""); got != "" {
		t.Fatalf("agySessionMode(\"\") = %q, want \"\" (no override for an unknown mode)", got)
	}
	if got := agySessionMode("not-a-mode"); got != "" {
		t.Fatalf("agySessionMode(unknown) = %q, want \"\" (askmode.MayAct allows an unlisted mode)", got)
	}
	if got := agySessionMode(askmode.Iteration); got != "" {
		t.Fatalf("agySessionMode(iteration) = %q, want \"\"", got)
	}
	for _, m := range []string{askmode.Brainstorm, askmode.QuickWork} {
		if got := agySessionMode(m); got != agySessionModePlan {
			t.Fatalf("agySessionMode(%s) = %q, want %q", m, got, agySessionModePlan)
		}
	}
}

// minInt is the two-argument minimum, so a slice bound in a failure message cannot panic. It is NOT named
// `min` because Go 1.21+ has that as a builtin and shadowing it here would be a trap.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
