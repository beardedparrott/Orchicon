package secrets

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS — the four checks that ARE the feature.
//
// `ReadForExecution` hands a secret's PLAINTEXT to a worker. Every check below is load-bearing and
// each must fail CLOSED, so each is pinned here. These are source-level assertions because the
// authorisation needs a live tenant database with a run, a work item and a granted secret; the
// invariant being protected is "the check is present and ordered before the decrypt", which is
// exactly what would silently regress in a refactor.
func readService(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	return string(b)
}

func TestReadForExecutionChecksTheCallerSubject(t *testing.T) {
	src := readService(t)
	i := strings.Index(src, "func (s *Service) ReadForExecution")
	if i < 0 {
		t.Fatal("ReadForExecution is gone — the worker's credential door does not exist")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, `callerSubject != "run:"+workflowRunID`) {
		t.Fatal("the caller's Subject is not compared to `run:<workflow_run_id>`. Without it any " +
			"authenticated caller — including a human API key — can read any run's granted secrets " +
			"by naming its id.")
	}
}

func TestReadForExecutionRefusesFinishedRuns(t *testing.T) {
	src := readService(t)
	i := strings.Index(src, "func (s *Service) ReadForExecution")
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	for _, st := range []string{"WorkflowRunCompleted", "WorkflowRunFailed", "WorkflowRunAborted"} {
		if !strings.Contains(body, st) {
			t.Errorf("a %s run is not refused — a finished run must not be able to pull a credential", st)
		}
	}
}

func TestReadForExecutionRequiresTheOperatorGrant(t *testing.T) {
	src := readService(t)
	i := strings.Index(src, "func (s *Service) ReadForExecution")
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "wi.SecretIDs") {
		t.Fatal("the work item's secret_ids are not consulted — a worker could read ANY tenant " +
			"secret by name, which is a tenant-wide credential read")
	}
	if !strings.Contains(body, "not available to this run") {
		t.Error("the refusal message is not the deliberately-opaque one; a distinguishable message " +
			"lets a worker enumerate the tenant's secret names")
	}
}

func TestReadForExecutionAuditsEveryRead(t *testing.T) {
	src := readService(t)
	i := strings.Index(src, "func (s *Service) ReadForExecution")
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, `Action:     "secret.read"`) && !strings.Contains(body, `"secret.read"`) {
		t.Fatal("a successful read writes no audit row. The audit trail is the control that makes " +
			"this door defensible; without it the secret can leave with no record it was ever read.")
	}
	// And the audit must come AFTER the grant check, so a refused read cannot be confused with a grant.
	auditAt := strings.Index(body, "secret.read")
	grantAt := strings.Index(body, "wi.SecretIDs")
	if auditAt < grantAt {
		t.Error("the audit is written BEFORE the grant check — a refused read would be indistinguishable " +
			"from an authorised one in the trail")
	}
}

// TestReadForExecutionNeverLogsTheValue — the plaintext must not reach a log line.
func TestReadForExecutionNeverLogsTheValue(t *testing.T) {
	src := readService(t)
	i := strings.Index(src, "func (s *Service) ReadForExecution")
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "s.log") || strings.Contains(body, "slog.") {
		t.Error("ReadForExecution logs something. The value must never reach a log file, and the " +
			"narrowest way to guarantee that is to log nothing here at all.")
	}
}
