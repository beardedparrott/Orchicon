package opencode

// askserve_test.go — THE ASK SERVE AND THE ASK'S DETAIL.
//
// Three things are asserted here: which serve an Ask conversation resolves to,
// that the Ask serve carries the interactive profile (and the worker serve still
// carries the sandbox), and that a permission.asked event reaches the consent
// layer with its detail intact rather than as a bare id.

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// The consent layer reads the detail off the adapter-NEUTRAL event, so the field
// has to exist on the scheduler contract type, not only on the opencode mapper.
var _ = func(e scheduler.SessionEvent) map[string]any { return e.Detail }

// ASK RESOLVES ITS OWN SERVE, and falls back to the worker serve when none is
// configured (the pre-split behaviour, which is what tests and an unconfigured
// plane get).
func TestChatHostSelection(t *testing.T) {
	worker := NewHostServe(slog.Default(), "/tmp/orchicon-worker-sel", "")
	ask := NewAskHostServe(slog.Default(), "/tmp/orchicon-ask-sel", "")

	if got := chatHost(ask, worker); got != ask {
		t.Errorf("with an Ask serve configured, chatHost resolved the worker serve — an Ask turn would run the worker sandbox")
	}
	if got := chatHost(ask, nil); got != ask {
		t.Errorf("chatHost(ask, nil) = %v, want the Ask serve", got)
	}
	if got := chatHost(nil, worker); got != worker {
		t.Errorf("chatHost(nil, worker) = %v, want the worker serve (the fallback)", got)
	}
	if got := chatHost(nil, nil); got != nil {
		t.Errorf("chatHost(nil, nil) = %v, want nil", got)
	}
}

// THE ASK SERVE CARRIES THE INTERACTIVE PROFILE; THE WORKER SERVE DOES NOT.
// The second half matters as much as the first: it is the leak check. the
// injected config rides the serve PROCESS, so a profile that reached the worker
// serve would sandbox-change every dispatched execution.
func TestAskHostServeCarriesInteractiveProfile(t *testing.T) {
	ask := NewAskHostServe(slog.Default(), "/tmp/orchicon-ask-profile", "")
	if got := ask.PermissionProfile(); got != ProfileInteractive {
		t.Fatalf("NewAskHostServe built profile %q, want %q", got, ProfileInteractive)
	}
	if got := externalDirOf(t, ask.serveConfig()); got != "allow" {
		t.Errorf("the Ask serve's external_directory[\"*\"] = %#v, want \"allow\" — Ask still runs inside the worker sandbox", got)
	}

	worker := NewHostServe(slog.Default(), "/tmp/orchicon-worker-profile", "")
	if got := worker.PermissionProfile(); got != "" {
		t.Errorf("the worker serve reports profile %q, want the zero value (worker)", got)
	}
	if got := externalDirOf(t, worker.serveConfig()); got != "deny" {
		t.Errorf("the worker serve's external_directory[\"*\"] = %#v, want \"deny\" — the interactive profile leaked into the worker serve", got)
	}
}

// externalDirOf unmarshals an injected config and reads external_directory["*"]
// (untyped: the value is compared as an `any`, which is the same shape the
// caller passed in).
func externalDirOf(t *testing.T, configContent string) any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(configContent), &cfg); err != nil {
		t.Fatalf("injected config is not valid JSON: %v", err)
	}
	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("injected config has no permission block, got %#v", cfg["permission"])
	}
	ext, ok := perm["external_directory"].(map[string]any)
	if !ok {
		t.Fatalf("injected config has no external_directory rule map, got %#v", perm["external_directory"])
	}
	return ext["*"]
}

// THE ASK'S DETAIL SURVIVES THE MAPPING. The consent core answers the ask from
// this detail; if classifyBusEvent dropped it, an ask would arrive carrying
// only an id and the consent layer could not say WHAT was being asked for.
func TestPermissionAskedCarriesDetail(t *testing.T) {
	props := map[string]any{
		"id":         "perm_123",
		"sessionID":  "ses_abc",
		"permission": "bash",
		"title":      "Run `rm -rf build/`",
		"pattern":    "rm *",
		"callID":     "call_9",
	}
	se := classifyBusEvent(BusEvent{Type: "permission.asked", Properties: props})
	if se == nil {
		t.Fatal("classifyBusEvent returned nil for a permission.asked event")
	}
	if se.Kind != "permission" {
		t.Errorf("kind = %q, want \"permission\"", se.Kind)
	}
	if se.PermissionID != "perm_123" {
		t.Errorf("PermissionID = %q, want perm_123", se.PermissionID)
	}
	if se.SessionID != "ses_abc" {
		t.Errorf("SessionID = %q, want ses_abc (the drain filters global permission events by session)", se.SessionID)
	}
	if se.Detail == nil {
		t.Fatal("the ask's detail was dropped — the consent layer would see only an id")
	}
	for _, k := range []string{"id", "permission", "title", "pattern", "callID"} {
		if se.Detail[k] != props[k] {
			t.Errorf("Detail[%q] = %#v, want %#v", k, se.Detail[k], props[k])
		}
	}
}
