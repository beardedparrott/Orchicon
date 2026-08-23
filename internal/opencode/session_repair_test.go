package opencode

import (
	"errors"
	"os"
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