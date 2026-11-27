package secrets

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE REPLACED execution_read_test.go.
//
// The first version of this door was EXECUTION-SCOPED: it required a workflow-run id and compared the
// caller's Subject to `run:<id>`. That looked like tight authorisation and was in fact a coupling bug
// — the platform only mints a plane credential for a worker with a role_ref, so fetching a DEV secret
// required a PROD role binding. Two unrelated entitlements, joined by accident, and the operator
// called it out: "the roles have nothing to do with this. The roles would pertain to the prod
// environment. The secret allows the workers to access DEV not prod."
//
// A secret is a value in the caller's tenant. Reading it needs the caller's ordinary credential. What
// makes the door defensible is not identity gymnastics — it is that the value is never stored where
// it leaks silently, and every read is audited.

func readServiceSrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	return string(b)
}

func readSecretBody(t *testing.T) string {
	t.Helper()
	src := readServiceSrc(t)
	i := strings.Index(src, "func (s *Service) ReadSecretByName")
	if i < 0 {
		t.Fatal("ReadSecretByName is gone — the secret door does not exist")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	return body
}

// TestSecretReadIsNotExecutionScoped is the correction itself: NO run id, NO subject check.
func TestSecretReadIsNotExecutionScoped(t *testing.T) {
	body := readSecretBody(t)
	for _, coupling := range []string{`"run:"`, "callerSubject", "workflowRunID", "WorkflowRun"} {
		if strings.Contains(body, coupling) {
			t.Errorf("ReadSecretByName still references %q. Reading a secret must not depend on an "+
				"execution or a run id — that coupling is what made a DEV secret need a PROD role binding.",
				coupling)
		}
	}
}

// TestSecretReadAuditsEveryRead — the control that makes the door defensible.
func TestSecretReadAuditsEveryRead(t *testing.T) {
	body := readSecretBody(t)
	if !strings.Contains(body, "secret.read") {
		t.Fatal("a successful read writes no audit row. The audit trail is the whole control: without " +
			"it the value can leave with no record it was ever read.")
	}
}

// TestSecretReadNeverLogsTheValue — plaintext must not reach a log file.
func TestSecretReadNeverLogsTheValue(t *testing.T) {
	body := readSecretBody(t)
	if strings.Contains(body, "s.log") || strings.Contains(body, "slog.") {
		t.Error("ReadSecretByName logs something. The value must never reach a log file, and the " +
			"narrowest guarantee is to log nothing here at all.")
	}
}
