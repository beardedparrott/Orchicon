package recovery

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestStrategyForFailureStopClass verifies WI-4 class 3: a deterministic
// config/infra failure (empty/invalid model ref, credential, image, etc.)
// resolves to the stop strategy regardless of the work item's kind — no
// resume loop — while a normal failure keeps the kind-derived strategy.
func TestStrategyForFailureStopClass(t *testing.T) {
	stopErrors := []string{
		`orchicon bridge: session: model ref "" has no provider/model (expected orchicon/<provider>/<model>)`,
		"model ref \"\" has no provider/model",
		"adapter kind \"bogus\" is not registered",
		"no suitable adapter for task",
		"no suitable worker for task",
		"provider authentication failed: credential missing",
		"runtime image not found: orchicon-runtime:ghost",
		"runtime image is not ready",
		`failed_to_start: execution never reached the model`,
	}
	for _, errText := range stopErrors {
		exec := &db.ExecutionRow{ErrorMessage: errText}
		if got := strategyForFailure(exec, domain.WorkItemKindTask); got != RecoveryStrategyStop {
			t.Errorf("deterministic stop error %q: strategy = %q, want %q", errText, got, RecoveryStrategyStop)
		}
	}

	// A work item explicitly marked recovery_stop still resolves to stop.
	if got := strategyForFailure(&db.ExecutionRow{ErrorMessage: "some transient timeout"}, domain.WorkItemKindRecoveryStop); got != RecoveryStrategyStop {
		t.Errorf("recovery_stop kind: got %q, want %q", got, RecoveryStrategyStop)
	}
}

// TestStrategyForFailureKeepsKindDefaultOnTransient verifies transient /
// runtime failures are NOT stop-classed: they keep the kind-derived default.
func TestStrategyForFailureKeepsKindDefaultOnTransient(t *testing.T) {
	transient := []string{
		"provider stream failed: Post https://opencode.ai/zen/v1/responses: context canceled",
		"stalled:missing_decision_signal:completion_probe_no_response",
		"provider stream failed: read tcp timeout",
		"context deadline exceeded",
	}
	for _, e := range transient {
		exec := &db.ExecutionRow{ErrorMessage: e}
		if got := strategyForFailure(exec, domain.WorkItemKindTask); got != RecoveryStrategySummarizeRestart {
			t.Errorf("transient error %q: strategy = %q, want summarize_restart (kind default)", e, got)
		}
	}
	// Blank-error and nil-exec keep the kind default (never stop-class).
	if got := strategyForFailure(&db.ExecutionRow{}, domain.WorkItemKindTask); got != RecoveryStrategySummarizeRestart {
		t.Errorf("blank error: got %q", got)
	}
	if got := strategyForFailure(nil, domain.WorkItemKindTask); got != RecoveryStrategySummarizeRestart {
		t.Errorf("nil exec: got %q", got)
	}
}
