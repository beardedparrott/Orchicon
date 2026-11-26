package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBridgeStartRequiresAnExecutionID pins the ONE refusal Start still owes its caller: an execution with no
// identity could attribute no callback, no usage row and no ledger entry, so it must fail before anything is
// spawned.
//
// THE STUB'S OLD CONTRACT IS GONE ON PURPOSE. This test used to assert `ErrNotImplemented`; the bridge now
// implements the session path, so a dispatch that still refused would be the regression. What replaces it is
// the identity check plus the full suite of behaviour tests in this package.
func TestBridgeStartRequiresAnExecutionID(t *testing.T) {
	b := New(quiet())
	err := b.Start(context.Background(), db.ExecutionRow{}, scheduler.ExecutionManifest{}, nil)
	if err == nil {
		t.Fatal("Start with no execution id returned nil — the callbacks and usage rows would be attributed to nothing")
	}
	if !strings.Contains(err.Error(), "execution id") {
		t.Errorf("Start error = %q, want it to name the missing execution id", err)
	}
}

// TestSessionOwnerKind pins the ownership the session-store resolution reads.
func TestSessionOwnerKind(t *testing.T) {
	if got := New(quiet()).SessionOwnerKind(); got != adapter.KindAntigravity {
		t.Fatalf("SessionOwnerKind() = %q, want %q", got, adapter.KindAntigravity)
	}
}

// TestCapabilitiesJSONIsWellFormedAndNonEmpty: the seed row's payload must parse and must not be the empty
// string (selectAdapter reads it, and an empty payload makes capability fit unjudgeable).
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

// TestCapabilitiesNeverOverclaim pins the two capability values this adapter must NOT advertise, because the
// recording proves the surface does not exist.
func TestCapabilitiesNeverOverclaim(t *testing.T) {
	for _, forbidden := range []string{"transcript_jsonl", "reasoning"} {
		if capabilityAdvertisedAnywhere(forbidden) {
			t.Errorf("capabilities advertise %q, but agy 1.5.13 provides no such surface — see "+
				"docs/adapters/antigravity-acp.md §7 and §3.1. Payload: %s", forbidden, BuildCapabilitiesJSON())
		}
	}
	for _, required := range []string{"cancellation", "resume", "mid_run_injection", "context_compaction"} {
		if !capabilitiesAdvertises("execution", required) {
			t.Errorf("capabilities do not advertise the execution capability %q, which the bridge implements. Payload: %s",
				required, BuildCapabilitiesJSON())
		}
	}
	for _, required := range []string{"tool_calls_streamed", "file_diffs", "usage"} {
		if !capabilitiesAdvertises("telemetry", required) {
			t.Errorf("capabilities do not advertise the telemetry %q, which the mapper implements. Payload: %s",
				required, BuildCapabilitiesJSON())
		}
	}
}
