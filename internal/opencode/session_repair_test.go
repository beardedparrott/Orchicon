package opencode

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestIsInfraSessionError pins the infra-failure classifier that feeds the
// dispatch repair loop. The observed field class: a runtime session backend
// that process-died (dial connection refused) or stayed up but poisoned
// (POST /session returning 5xx on a corrupt session store — the
// "Failed to execute statement" incident) must be classified infra so the
// dispatch repairs the container instead of burning the step's retries.
// A 4xx or a mid-run model/send error is NOT infra.
func TestIsInfraSessionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial connection refused (serve died)", errors.New(`create session: Post "http://172.17.0.4:4096/session": dial tcp 172.17.0.4:4096: connect: connection refused`), true},
		{"session create 500 (poisoned store)", errors.New(`create session: opencode serve POST /session: http 500: {"name":"UnknownError","data":{"message":"Unexpected server error."}}`), true},
		{"session create 503", errors.New(`create session: opencode serve POST /session: http 503: unavailable`), true},
		{"no serve available", errors.New("no opencode serve available for execution 01ABC"), true},
		{"serve not healthy (health probe)", errors.New("opencode serve not healthy (http://127.0.0.1:4096)"), true},
		{"session create 400 (client error)", errors.New("opencode serve POST /session: http 400: bad request"), false},
		{"mid-session send error (worker/model)", errors.New("opencode session send: http 500"), false},
		{"transient transport read", errors.New("opencode session subscribe: http 500"), false},
		{"unrelated adapter error", errors.New("opencode binary not found"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInfraSessionError(c.err); got != c.want {
				t.Fatalf("isInfraSessionError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestSessionRepairBudgetEnv pins the repair-budget resolution: default 3
// per dispatch, env override, and < 1 = disabled (legacy backoff only).
func TestSessionRepairBudgetEnv(t *testing.T) {
	_ = os.Unsetenv("ORCHICON_SESSION_REPAIR_ATTEMPTS")
	if got := sessionRepairBudget(); got != 3 {
		t.Fatalf("default budget = %d, want 3", got)
	}
	if err := os.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "5"); err != nil {
		t.Fatal(err)
	}
	if got := sessionRepairBudget(); got != 5 {
		t.Fatalf("env budget = %d, want 5", got)
	}
	if err := os.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "0"); err != nil {
		t.Fatal(err)
	}
	if got := sessionRepairBudget(); got != 0 {
		t.Fatalf("disabled budget = %d, want 0", got)
	}
}

// TestInfraRepairThresholdEnv pins the consecutive-infra gate: default 2
// (first infra failure retried on the same container so a healthy parallel
// step isn't torn down), env override, and < 2 disables the container
// repair trigger.
func TestInfraRepairThresholdEnv(t *testing.T) {
	_ = os.Unsetenv("ORCHICON_SESSION_INFRA_THRESHOLD")
	if got := infraRepairThreshold(); got != 2 {
		t.Fatalf("default threshold = %d, want 2", got)
	}
	if err := os.Setenv("ORCHICON_SESSION_INFRA_THRESHOLD", "4"); err != nil {
		t.Fatal(err)
	}
	if got := infraRepairThreshold(); got != 4 {
		t.Fatalf("env threshold = %d, want 4", got)
	}
	if err := os.Setenv("ORCHICON_SESSION_INFRA_THRESHOLD", "0"); err != nil {
		t.Fatal(err)
	}
	if got := infraRepairThreshold(); got != 0 {
		t.Fatalf("disabled threshold = %d, want 0", got)
	}
}

// TestCapToolOutput pins the tool-output cap that keeps giant build/test
// logs and listings from re-entering the model context on every later turn:
// output under the cap passes through, output over it keeps the head + a
// truncation marker, and a disabled cap (< 1) passes everything through.
func TestCapToolOutput(t *testing.T) {
	// Force a small cap so the test doesn't need to synthesize 128k.
	if err := os.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "1024"); err != nil {
		t.Fatal(err)
	}
	small := strings.Repeat("a", 512)
	if got := capToolOutput(small); got != small {
		t.Fatalf("under-cap output was altered")
	}
	big := strings.Repeat("b", 8192)
	got := capToolOutput(big)
	if len(got) >= len(big) {
		t.Fatalf("over-cap output not truncated: in=%d out=%d", len(big), len(got))
	}
	if !strings.Contains(got, "truncated by Orchicon") {
		t.Fatalf("missing truncation marker: %q", got[len(got)-120:])
	}

	// Disabled cap → passthrough even for a huge output.
	if err := os.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "0"); err != nil {
		t.Fatal(err)
	}
	if got := capToolOutput(big); got != big {
		t.Fatalf("disabled cap altered output")
	}
}

// TestCapPartOutput pins the transcript-side cap: a tool_use part's
// state.output is capped in a copy (the live event is untouched); a part
// without tool output passes through unchanged.
func TestCapPartOutput(t *testing.T) {
	if err := os.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "1024"); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("b", 8192)
	part := map[string]any{
		"tool": "bash",
		"state": map[string]any{
			"status": "completed",
			"output": big,
		},
	}
	got := capPartOutput(part)
	cp := got.(map[string]any)
	st := cp["state"].(map[string]any)
	if out := st["output"].(string); len(out) >= len(big) {
		t.Fatalf("part output not capped: in=%d out=%d", len(big), len(out))
	}
	// The original event must be unchanged (the UI path already used it).
	if stOrig := part["state"].(map[string]any); stOrig["output"] != big {
		t.Fatalf("original event was mutated by capPartOutput")
	}

	// A non-tool part passes through.
	if got := capPartOutput("not-a-map"); got != "not-a-map" {
		t.Fatalf("non-map part was altered")
	}
}