package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBridgeStartRefusesNotImplemented is AC 5's unit-level half: a dispatch on this kind must fail
// with the stub's EXPLICIT "not yet implemented" message, never a nil error and never the opaque
// `no ready adapters of kind "antigravity"` the row/registration pair exists to prevent.
func TestBridgeStartRefusesNotImplemented(t *testing.T) {
	b := New(quiet())
	err := b.Start(context.Background(), db.ExecutionRow{ID: "exe_1", TenantID: "tnt_dev"},
		scheduler.ExecutionManifest{}, nil)
	if err == nil {
		t.Fatal("the stub bridge returned nil — a half-started antigravity session would look live to the reconciler")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Start error = %v, want ErrNotImplemented (the server/QA path matches on it)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "not yet implemented") {
		t.Errorf("the refusal must say %q so it is distinguishable from an unknown kind; got %q", "not yet implemented", msg)
	}
	if !strings.Contains(msg, "antigravity") {
		t.Errorf("the refusal must name the kind; got %q", msg)
	}
}

// TestSessionOwnerKind pins the ownership the session-store resolution reads.
func TestSessionOwnerKind(t *testing.T) {
	if got := New(quiet()).SessionOwnerKind(); got != adapter.KindAntigravity {
		t.Fatalf("SessionOwnerKind() = %q, want %q", got, adapter.KindAntigravity)
	}
}

// TestCapabilitiesJSONIsWellFormedAndNonEmpty: the seed row's payload must parse and must not be the
// empty string (selectAdapter reads it, and an empty payload makes capability fit unjudgeable).
func TestCapabilitiesJSONIsWellFormedAndNonEmpty(t *testing.T) {
	raw := BuildCapabilitiesJSON()
	if strings.TrimSpace(raw) == "" {
		t.Fatal("BuildCapabilitiesJSON() returned an empty payload — the seeded runtime_adapters row would be useless to selectAdapter")
	}
	var caps map[string]any
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		t.Fatalf("capabilities JSON does not parse: %v (%s)", err, raw)
	}
	for _, key := range []string{"tools", "context", "execution", "telemetry"} {
		if _, ok := caps[key]; !ok {
			t.Errorf("capabilities JSON is missing the generic key %q — the control plane reads these uniformly across kinds; got %s", key, raw)
		}
	}
}
