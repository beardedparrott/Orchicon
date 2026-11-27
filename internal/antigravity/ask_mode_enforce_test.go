package antigravity

// ask_mode_enforce_test.go — the mode boundary's PRE-ENFORCEMENT, which turned out to be the only part of it
// agy 1.5.13 actually honours.
//
// THE DEFECT THESE TESTS PIN, measured against the real binary. A Brainstorm conversation was asked to create
// a file. It created the file: no permission request arrived (agy never asks about an edit), so the
// in-handler gate in ask_permission.go was never reached, and the `permissions.deny` document the transport
// renders into `--settings` was accepted on argv and IGNORED. "Brainstorm cannot write" was therefore a claim
// the transport did not enforce — the worst kind of gap, because every log line looked correct.
//
// `session/set_mode "plan"` IS honoured (measured: the write is refused, the file is not created, and the
// model is told to plan instead), so the fix is to put an act-denied conversation into it before the first
// prompt. These tests assert the WIRE (a `session/set_mode` was sent, with `plan`) rather than agy's prose,
// because that is the property this transport owns.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskModeForcesAgysPlanModeWhenTheModeMayNotAct is the regression test for the defect above.
func TestAskModeForcesAgysPlanModeWhenTheModeMayNotAct(t *testing.T) {
	for _, mode := range []string{askmode.Brainstorm, askmode.QuickWork} {
		t.Run(mode, func(t *testing.T) {
			h := newAskHarness(t, script{})
			h.create(mode)

			ids := h.proc(0).modeIdSets()
			if len(ids) != 1 {
				t.Fatalf("session/set_mode called %v, want exactly one call with %q — without it a %s "+
					"conversation can write, because agy neither asks about an edit nor honours permissions.deny",
					ids, "plan", mode)
			}
			if ids[0] != "plan" {
				t.Fatalf("session/set_mode = %q, want %q (agy's own read-only mode is the only pre-emptive "+
					"refusal agy 1.5.13 honours)", ids[0], "plan")
			}
		})
	}
}

// TestAskModeLeavesAnActingModeAlones: Iteration is the hands. It must NOT be forced into plan mode, because
// its writes are the operator's to approve by card — forcing plan would silently remove the very capability
// the mode exists to provide.
func TestAskModeLeavesAnActingModeAlones(t *testing.T) {
	h := newAskHarness(t, script{})
	h.create(askmode.Iteration)

	if ids := h.proc(0).modeIdSets(); len(ids) != 0 {
		t.Fatalf("session/set_mode called %v in Iteration, want none — Iteration may write and run, so it "+
			"keeps agy's default mode and its writes raise cards instead", ids)
	}
}

// TestAskModeSetModeHappensBeforeTheFirstPrompt: the ordering is the whole enforcement. A set_mode issued
// after the prompt would arrive after the model had already chosen its tool.
func TestAskModeSetModeHappensBeforeTheFirstPrompt(t *testing.T) {
	h := newAskHarness(t, script{})
	sid := h.create(askmode.Brainstorm)
	fp := h.proc(0)

	if ids := fp.modeIdSets(); len(ids) == 0 {
		t.Fatal("no session/set_mode was sent before the conversation was usable")
	}
	// The conversation was created eagerly and has NOT been prompted yet; that is exactly the window the
	// plan mode has to be in place by.
	if n := len(fp.promptsSeen()); n != 0 {
		t.Fatalf("%d prompt(s) reached agy before the mode was enforced", n)
	}
	if sid == "" {
		t.Fatal("no session to enforce the mode on")
	}
}

// TestAskModeUnsetModeSendsNoSetMode: no mode stamped means no policy (the same "unknown mode allows" rule
// askmode.Allows and claude's gate apply), so the transport must not invent one.
func TestAskModeUnsetModeSendsNoSetMode(t *testing.T) {
	h := newAskHarness(t, script{})
	// An empty mode: CreateConversationSession with no mode stamped on the context.
	if _, err := h.b.CreateConversationSession(t.Context(), h.con, "ask-orchicon:"+h.con); err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if ids := h.proc(0).modeIdSets(); len(ids) != 0 {
		t.Fatalf("session/set_mode called %v with no Ask mode stamped, want none", ids)
	}
}

// TestAgyModeIDForAsk pins the mapping itself.
func TestAgyModeIDForAsk(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{askmode.Brainstorm, "plan"},
		{askmode.QuickWork, "plan"},
		{askmode.Iteration, ""},
		{"", ""},
		{"an-unknown-mode", ""},
	} {
		if got := agyModeIDForAsk(tc.mode); got != tc.want {
			t.Errorf("agyModeIDForAsk(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}
