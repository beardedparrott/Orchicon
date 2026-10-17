package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// absentLifecycle is a RuntimeLifecycle whose image presence probe answers
// from a fixed (present, err) pair. It exists so the missing-image fail-fast is
// testable without a daemon, a DB, or a container runtime.
type absentLifecycle struct {
	stubLifecycle
	present bool
	err     error
	asked   []string
}

func (l *absentLifecycle) ImagePresent(_ context.Context, ref string) (bool, error) {
	l.asked = append(l.asked, ref)
	return l.present, l.err
}

// TestRuntimeImageAbsentIsAPermanentDispatchFailure pins AC 14's
// classification: a missing runtime image is a CONFIGURATION failure, so the
// dispatch is blocked with the reason instead of being requeued into a
// ready→failed→ready loop. The predicate is the SHARED one (the test must not
// re-implement it), and the reason must name the exact ref and the fix.
func TestRuntimeImageAbsentIsAPermanentDispatchFailure(t *testing.T) {
	const ref = "ghcr.io/beardedparrott/orchicon-runtime:gui"
	err := runtimeImageAbsentError(ref)

	if !errors.Is(err, ErrRuntimeImageAbsent) {
		t.Fatalf("runtimeImageAbsentError must wrap ErrRuntimeImageAbsent, got %v", err)
	}
	if !permanentDispatchFailure(err) {
		t.Fatalf("a missing runtime image must be a PERMANENT dispatch failure (blocked, not requeued): %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, ref) {
		t.Errorf("the failure must name the exact image ref %q; got %q", ref, msg)
	}
	if !strings.Contains(msg, "Fetch") || !strings.Contains(msg, "podman pull") {
		t.Errorf("the failure must name the action that fixes it (Fetch / podman pull); got %q", msg)
	}
	// The pre-existing permanent classes must keep working (no regression).
	if !permanentDispatchFailure(ErrAdapterDisabled) || !permanentDispatchFailure(ErrAdapterKindUnregistered) {
		t.Error("the existing permanent-dispatch classes must remain classified as permanent")
	}
	// A transient failure must NOT be classified as permanent.
	if permanentDispatchFailure(errors.New("adapter unreachable")) {
		t.Error("a transient dispatch error must not be classified permanent")
	}
}

// TestRuntimeImageAbsentReason covers the run-start seam in all three states.
// ABSENT must report a reason (so ensureRunContainerFailover fails the run at
// start instead of falling back to the base image); PRESENT and a probe ERROR
// must not — a daemon fault is not the operator's missing image.
func TestRuntimeImageAbsentReason(t *testing.T) {
	const ref = "orchicon-runtime:gui"

	absent := &absentLifecycle{present: false}
	r := &WorkflowReconciler{runtime: absent, log: slog.Default()}
	reason, stop := r.runtimeImageAbsentReason(context.Background(), ref)
	if !stop {
		t.Fatal("an absent image must stop the run at start (no base fallback)")
	}
	if !strings.Contains(reason, ref) {
		t.Errorf("the reason must name the ref %q; got %q", ref, reason)
	}
	if len(absent.asked) != 1 || absent.asked[0] != ref {
		t.Errorf("the probe must ask about exactly the resolved ref, got %v", absent.asked)
	}

	present := &absentLifecycle{present: true}
	r = &WorkflowReconciler{runtime: present, log: slog.Default()}
	if _, stop := r.runtimeImageAbsentReason(context.Background(), ref); stop {
		t.Error("a present image must NOT fail the run at start")
	}

	broken := &absentLifecycle{err: errors.New("daemon unreachable")}
	r = &WorkflowReconciler{runtime: broken, log: slog.Default()}
	if _, stop := r.runtimeImageAbsentReason(context.Background(), ref); stop {
		t.Error("a probe ERROR must fall through to the existing retry path, not be reported as a missing image")
	}

	// Headless (no daemon wired): the probe must not claim absence.
	r = &WorkflowReconciler{log: slog.Default()}
	if _, stop := r.runtimeImageAbsentReason(context.Background(), ref); stop {
		t.Error("a reconciler with no runtime wired must not report a missing image")
	}

	_ = db.BaseRuntimeImage
}
