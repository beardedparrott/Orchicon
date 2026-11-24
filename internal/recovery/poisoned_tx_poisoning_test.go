package recovery

// poisoned_tx_poisoning_test.go — A SOFT TELEMETRY READ MUST NOT BE ABLE TO DESTROY A RECOVERY.
//
// THE INCIDENT (task 01M84X6FD9X2N5FQ534GK91THR, execution 01M85DCK1YSAB2C83K9YC1GYCD).
//
// The control plane restarted, the reaper correctly failed the in-flight execution with
// "execution lost: control plane restarted or runtime container gone", and recovery fired — as it
// should. It then died 190ms later with an EMPTY summary and every step still `pending`:
//
//	"recovery reconcile failed", error="mark step failed: db: update recovery step run:
//	 ERROR: current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)"
//	"recovery quarantined (poisoned tx)"
//
// The cause was stepCapture's usage-sum read, written to be NON-FATAL:
//
//	if t, c, uerr := db.SumUsageForExecution(ctx, tx, ...); uerr == nil { tokens, cost = t, c }
//
// The error was deliberately swallowed — but a failed statement ABORTS THE POSTGRES TRANSACTION
// regardless of what Go does with the error, so every later statement in progressRecovery (including
// the write that records the failure) returned 25P02. quarantineRecovery then terminally failed the
// recovery and recorded nothing.
//
// These pin the three corrections:
//   1. the soft read does not share the write transaction;
//   2. L3, the ladder's ceiling, BLOCKS for a human instead of terminally failing;
//   3. a quarantine and a poisoned step both RECORD their cause.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// (1) THE READ IS OFF THE WRITE TRANSACTION. source-level, because the defect is WHICH connection the
// statement runs on — something no unit test of the sum can observe.
func TestTheSoftUsageReadDoesNotShareTheRecoveryTransaction(t *testing.T) {
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "sumUsageOffTx") {
		t.Fatal("the usage sum no longer goes through sumUsageOffTx — if it is back on the caller's tx, " +
			"one telemetry failure aborts the recovery's own writes and the recovery is quarantined")
	}

	// The poisoning call shape must be GONE: SumUsageForExecution taking the recovery's own tx.
	start := strings.Index(src, "func (r *Reconciler) stepCapture")
	end := strings.Index(src[start:], "\n}\n") + start
	body := src[start:end]
	if strings.Contains(body, "SumUsageForExecution(ctx, tx,") {
		t.Error("stepCapture is summing usage on the WRITE transaction again. A failure there aborts the " +
			"tx, and the step that would record the failure then fails with 25P02 — the exact incident.")
	}
	if !strings.Contains(body, "sumUsageOffTx") {
		t.Error("stepCapture does not use the off-transaction sum, so a soft read error can still poison it")
	}
}

// (2) L3 IS A BLOCK, NOT A DEAD END. The old arm marked the recovery failed, which made every later
// attempt escalate into "cannot escalate further" and die — the item could never recover again.
func TestL3BlocksForAHumanInsteadOfTerminalFailure(t *testing.T) {
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	src := string(raw)

	i := strings.Index(src, "// L3 IS THE CEILING OF THE LADDER, NOT A TERMINAL FAILURE.")
	if i < 0 {
		t.Fatal("the L3 arm lost its rationale comment — check it was not reverted to the terminal failure")
	}
	arm := src[i:]
	arm = arm[:strings.Index(arm, "\n\t}")]

	// The L3 arm must park the recovery for approval, never fail it.
	if !strings.Contains(arm, "RecoveryBlocked") || !strings.Contains(arm, "NeedsHumanApproval") {
		t.Errorf("the L3 arm does not BLOCK for human approval. L3 means human escalation (docs/06 §7); "+
			"failing here removes the only remaining route and makes the item permanently unrecoverable.\n%s", arm)
	}
	if strings.Contains(arm, "strPtr(domain.RecoveryFailed)") {
		t.Error("the L3 arm still terminally FAILS the recovery — that is the dead end: every subsequent " +
			"attempt escalates to L3 and dies, so the task can never recover again")
	}
	// And it must carry a cause, which is what an operator acts on.
	if !strings.Contains(arm, "Cause:") {
		t.Error("the L3 block carries no cause in its summary, so an operator sees a parked recovery with " +
			"nothing to act on")
	}
}

// (3) THE QUARANTINE RECORDS ITS CAUSE. It used to leave an empty summary, so a quarantined recovery
// looked identical to one that never ran.
func TestAQuarantineRecordsTheCauseOnTheRow(t *testing.T) {
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "func (r *Reconciler) quarantineRecovery")
	if i < 0 {
		t.Fatal("quarantineRecovery is gone")
	}
	body := src[i:]
	body = body[:strings.Index(body, "\n}\n")]

	if !strings.Contains(body, "Summary:") || !strings.Contains(body, "cause.Error()") {
		t.Error("quarantineRecovery does not record the cause in the recovery's summary. The row is the " +
			"operator's evidence; the log line is not enough, and an empty summary is indistinguishable " +
			"from a recovery that never ran.")
	}
}

// AND A POISONED STEP SAYS SO, rather than storing "{}" and leaving the cause to a log line.
func TestAPoisonedStepResultNamesThePoisoning(t *testing.T) {
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "isPoisonedRecoveryTx(stepErr)") {
		t.Error("runStep no longer distinguishes a poisoned transaction from a step failure — the step " +
			"result would be \"{}\" and the real cause (an earlier statement) would be invisible")
	}
	if !strings.Contains(src, "25P02") {
		t.Error("the poisoned-step result does not name SQLSTATE 25P02, so the next reader has to " +
			"rediscover what '{}' meant")
	}
}

// A VISIBILITY GUARD on the classification the whole design rests on: the quarantine predicate must
// recognise the error Postgres actually returns, or the recovery is left wedged as `running` instead.
func TestTheQuarantinePredicateMatchesPostgres(t *testing.T) {
	const real = `ERROR: current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)`
	if !isPoisonedRecoveryTx(errFromString(real)) {
		t.Fatal("isPoisonedRecoveryTx does not recognise the message Postgres actually returns")
	}
}

// errFromString is a minimal error carrying a message (avoids pulling in fmt for one call).
type strErr string

func (e strErr) Error() string { return string(e) }

func errFromString(s string) error { return strErr(s) }

var _ = json.Marshal
var _ = domain.RecoveryBlocked
