package main

// install_runtime_images_env_test.go — a GRAPHICAL installer's answer has somewhere to go.
//
// THE GAP THIS CLOSES. Every way of choosing runtime images assumed a human or a
// blanket "yes": the prompt needs a terminal, and ORCHICON_ASSUME_YES means ALL.
// The Windows MSI asks for a multi-select in its own UI and then runs
// `orchicon install` with no console — and the non-interactive arm returns "none"
// BEFORE the recorded-answer file is read, so a wizard's choice of "base + dev"
// silently fetched nothing. Not an error: an install that just quietly skipped the
// thing the operator had ticked.
//
// The grammar is the prompt's own (parseRuntimeImageSelection), so a command line,
// the interactive prompt and an installer cannot disagree about "1,3".

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// envAnswersRuntimeImages clears any ambient value first: this suite runs in a
// shell that may have exported one, and a test that silently inherits it would
// pass or fail for a reason it never states.
func envAnswersRuntimeImages(t *testing.T, value string) {
	t.Helper()
	t.Setenv(runtimeImagesEnv, value)
	// The interactivity flag must NOT be what decides this, so make it explicit
	// that a non-interactive run is the case under test.
	t.Setenv("ORCHICON_INSTALL_INTERACTIVE", "0")
	t.Setenv("ORCHICON_ASSUME_YES", "")
}

func TestExplicitRuntimeImagesAnswersNonInteractively(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want []string // variants, in order
	}{
		{"all", "all", []string{"base", "gui", "dev"}},
		{"none", "none", nil},
		{"two of three, base auto-included", "2,3", []string{"base", "gui", "dev"}},
		{"just the base", "1", []string{"base"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envAnswersRuntimeImages(t, tc.env)
			var out bytes.Buffer
			choices, answered, err := explicitRuntimeImages(&out, "test-tag")
			if err != nil {
				t.Fatalf("explicitRuntimeImages(%q): %v", tc.env, err)
			}
			if !answered {
				t.Fatalf("%s=%q was treated as NO ANSWER — the installer's choice would be discarded",
					runtimeImagesEnv, tc.env)
			}
			if len(choices) != len(tc.want) {
				t.Fatalf("%s=%q gave %d choice(s), want %d (%v)",
					runtimeImagesEnv, tc.env, len(choices), len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if choices[i].Variant != want {
					t.Errorf("%s=%q choice %d = %q, want %q",
						runtimeImagesEnv, tc.env, i, choices[i].Variant, want)
				}
			}
		})
	}
}

// TestExplicitRuntimeImagesUnsetIsNoAnswer — unset must fall THROUGH, not read as
// "none": an ordinary install that has not been told anything still has to reach
// the recorded-answer path and the prompt.
func TestExplicitRuntimeImagesUnsetIsNoAnswer(t *testing.T) {
	for _, value := range []string{"", "   "} {
		envAnswersRuntimeImages(t, value)
		var out bytes.Buffer
		choices, answered, err := explicitRuntimeImages(&out, "test-tag")
		if err != nil {
			t.Fatalf("explicitRuntimeImages(%q): %v", value, err)
		}
		if answered || choices != nil {
			t.Errorf("unset/blank %s was treated as an ANSWER (%v) — it must fall through",
				runtimeImagesEnv, choices)
		}
	}
}

// TestExplicitRuntimeImagesRejectsGarbage — an installer that asked a question and
// then fetched nothing is worse than one that fails and says why.
func TestExplicitRuntimeImagesRejectsGarbage(t *testing.T) {
	for _, value := range []string{"0", "9", "base", "1,x"} {
		envAnswersRuntimeImages(t, value)
		var out bytes.Buffer
		if _, _, err := explicitRuntimeImages(&out, "test-tag"); err == nil {
			t.Errorf("%s=%q was accepted; it must be refused rather than silently meaning none",
				runtimeImagesEnv, value)
		}
	}
}

// TestExplicitRuntimeImagesOutranksTheInteractivityGate is the regression itself:
// resolved through the real entry point, on a NON-INTERACTIVE run, the answer must
// survive. Before this, the non-interactive arm returned "none" first.
func TestExplicitRuntimeImagesOutranksTheInteractivityGate(t *testing.T) {
	envAnswersRuntimeImages(t, "1")
	var out bytes.Buffer
	// answerIn is empty on purpose: a non-interactive run must not read it.
	got, err := resolveInstallRuntimeImagesFor("tnt_msi_test", &out, strings.NewReader(""), "test-tag", false)
	if err != nil {
		t.Fatalf("resolveInstallRuntimeImagesFor: %v", err)
	}
	if len(got) != 1 || got[0].Variant != "base" {
		t.Fatalf("non-interactive install with %s=1 resolved to %v, want the base variant — "+
			"an installer's multi-select must not be discarded by the interactivity gate",
			runtimeImagesEnv, got)
	}
	if !strings.Contains(out.String(), runtimeImagesEnv) {
		t.Errorf("the resolution did not name %s in its output, so an operator cannot tell why "+
			"images were fetched: %q", runtimeImagesEnv, out.String())
	}
}

// TestExplicitRuntimeImagesUnsetStaysNonInteractive — the surrounding behaviour must
// not change: with no answer and no terminal, nothing is fetched and nothing blocks.
func TestExplicitRuntimeImagesUnsetStaysNonInteractive(t *testing.T) {
	envAnswersRuntimeImages(t, "")
	var out bytes.Buffer
	got, err := resolveInstallRuntimeImagesFor("tnt_msi_test_unset", &out, strings.NewReader(""), "test-tag", false)
	if err != nil {
		t.Fatalf("resolveInstallRuntimeImagesFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a non-interactive install with no answer resolved to %v, want none", got)
	}
	if !strings.Contains(out.String(), "not asking") {
		t.Errorf("a non-interactive install no longer says it is not asking: %q", out.String())
	}
	// And the ambient environment really was cleared for this test.
	if os.Getenv(runtimeImagesEnv) != "" {
		t.Fatalf("test fixture leaked %s", runtimeImagesEnv)
	}
}
