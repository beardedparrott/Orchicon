package providers_test

// antigravity_test.go — the regression suite for the `google` provider tier.
//
// THE BUG THESE PIN: after task 5 landed the real antigravity bridge, a worker could run on
// `antigravity/google/<model>` but NO operator could configure one, because selecting the provider pill the
// picker offers (`google`, the antigravity adapter's only provider) produced `[not_found] db: not found`:
// ListProviderModels' first statement is EffectiveProfile, and `google` is neither a built-in
// orchicon.Profile nor a catalog provider. The tests below call the SAME service method the RPC calls and
// assert the errno, the ids, the source and the auth mapping — not a helper standing in for them.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/providers"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// googleProviderID is the provider id the antigravity adapter kind exposes. Spelled literally (not copied
// from the package constant) so the test fails if someone changes the wiring rather than the id.
const googleProviderID = "google"

// TestListProviderModels_GoogleDoesNotReturnNotFound is AC3: with NO provider_settings row for `google` and
// no lister wired, the call must not surface db.ErrNotFound (the `[not_found] db: not found` the operator
// saw) and must return a populated list instead.
func TestListProviderModels_GoogleDoesNotReturnNotFound(t *testing.T) {
	svc, pool := newProvidersTestService(t)
	ctx := context.Background()
	ensureTenant(t, pool, testTenant)

	res, err := svc.ListProviderModels(ctx, testTenant, googleProviderID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			t.Fatalf("ListProviderModels(%q) returned db.ErrNotFound — this is EXACTLY the `[not_found] db: "+
				"not found` the operator saw; the branch must run before EffectiveProfile", googleProviderID)
		}
		t.Fatalf("ListProviderModels(%q): %v", googleProviderID, err)
	}
	if !res.Enabled {
		t.Errorf("Enabled = false for %q, want true (the tier is available; only its source varies)", googleProviderID)
	}
	if len(res.Models) == 0 {
		t.Fatal("no models listed — an empty list reads as \"agy has no models\", which is the wrong answer")
	}
	for _, m := range res.Models {
		if strings.Contains(m.ID, "/") {
			t.Errorf("model id %q contains a '/' — ids must be BARE agy modelIds; the picker composes "+
				"antigravity/google/<id> itself, so a full ref here yields a 4-segment ref", m.ID)
		}
	}
	if res.Models[0].Source != "snapshot" {
		t.Errorf("Source = %q, want %q (no lister is wired in this test, so the vendored snapshot answers)",
			res.Models[0].Source, "snapshot")
	}
}

// TestListProviderModels_GoogleNeedsNoDatabase pins the structural half of the fix: the `google` tier must
// answer with NO pool at all, because it must not reach EffectiveProfile. It runs everywhere (no
// ORCHICON_TEST_DSN needed), which is what makes the AC4 comment-out proof reproducible in any environment.
func TestListProviderModels_GoogleNeedsNoDatabase(t *testing.T) {
	// A nil-pool service PANICS rather than erroring if the tier ever reaches EffectiveProfile, so the
	// panic is converted into the diagnostic instead of taking the whole package down with it.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ListProviderModels(%q) panicked over a nil pool (%v) — the antigravity branch must "+
				"answer BEFORE EffectiveProfile", googleProviderID, r)
		}
	}()
	svc := providers.New(nil, nil, nil)
	res, err := svc.ListProviderModels(context.Background(), testTenant, googleProviderID)
	if err != nil {
		t.Fatalf("ListProviderModels(%q) over a nil pool: %v — the antigravity branch must not touch the DB",
			googleProviderID, err)
	}
	if len(res.Models) == 0 {
		t.Fatal("no models listed from the vendored snapshot")
	}
}

// TestListProviderModels_GoogleSnapshotMatchesAgyRecording proves the snapshot is the MEASURED catalogue and
// not a hand-typed guess: its ids must be exactly the `models.availableModels` ids agy 1.5.13 returned in
// internal/antigravity/testdata/acp/handshake_auth.jsonl.
func TestListProviderModels_GoogleSnapshotMatchesAgyRecording(t *testing.T) {
	want := recordedAgyModelIDs(t)
	svc := providers.New(nil, nil, nil)
	res, err := svc.ListProviderModels(context.Background(), testTenant, googleProviderID)
	if err != nil {
		t.Fatalf("ListProviderModels(%q): %v", googleProviderID, err)
	}
	got := make(map[string]bool, len(res.Models))
	for _, m := range res.Models {
		got[m.ID] = true
	}
	if len(got) != len(want) {
		t.Errorf("snapshot has %d models, the recording has %d", len(got), len(want))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("the recording's model %q is missing from the snapshot", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("the snapshot lists %q, which agy 1.5.13 never offered", id)
		}
	}
}

// TestListProviderModels_GoogleLiveModels proves a wired lister wins over the snapshot and that its rows are
// marked live with bare ids (AC7 for the live path).
func TestListProviderModels_GoogleLiveModels(t *testing.T) {
	svc := providers.New(nil, nil, nil)
	svc.SetAntigravityModelLister(func(context.Context, string) ([]providers.AgyModel, error) {
		return []providers.AgyModel{
			{ID: "gemini-4-carbon-high", Name: "Gemini 4 Carbon (High)"},
			{ID: "gemini-pro-orchestrator", Name: "Orchestrator (Carbon)"},
			{ID: "gemini-4-carbon-high", Name: "duplicate — must collapse"},
		}, nil
	})
	res, err := svc.ListProviderModels(context.Background(), testTenant, googleProviderID)
	if err != nil {
		t.Fatalf("ListProviderModels(%q): %v", googleProviderID, err)
	}
	if res.Degraded {
		t.Error("Degraded = true for a successful live listing")
	}
	if len(res.Models) != 2 {
		t.Fatalf("got %d models, want 2 (the duplicate must collapse): %+v", len(res.Models), res.Models)
	}
	for _, m := range res.Models {
		if m.Source != "live" {
			t.Errorf("model %q Source = %q, want live", m.ID, m.Source)
		}
		if strings.Contains(m.ID, "/") {
			t.Errorf("model id %q contains a '/'", m.ID)
		}
	}
	// The effort suffix is what the TUI renders as a reasoning flag.
	if !res.Models[0].Reasoning && !res.Models[1].Reasoning {
		t.Error("no row was marked Reasoning; agy encodes effort as an id suffix (gemini-4-carbon-high)")
	}
}

// TestListProviderModels_GoogleNonAuthFailureFallsBackToSnapshot proves a timeout/wedged-child failure
// degrades to the snapshot WITH Degraded=true, rather than failing the whole picker.
func TestListProviderModels_GoogleNonAuthFailureFallsBackToSnapshot(t *testing.T) {
	svc := providers.New(nil, nil, nil)
	svc.SetAntigravityModelLister(func(context.Context, string) ([]providers.AgyModel, error) {
		return nil, errors.New("antigravity: session/new timed out after 20s")
	})
	res, err := svc.ListProviderModels(context.Background(), testTenant, googleProviderID)
	if err != nil {
		t.Fatalf("a non-auth failure must fall back to the snapshot, got error: %v", err)
	}
	if !res.Degraded {
		t.Error("Degraded = false for a snapshot fallback — the picker must be able to say the list is stale")
	}
	if len(res.Models) == 0 {
		t.Fatal("no models in the snapshot fallback")
	}
}

// TestListProviderModels_GoogleAuthFailureIsNotMasked is the other half of AC5 at the service layer: a
// signed-out host (or an absent agy) must never be answered from the snapshot. The error must carry the
// actionable message and satisfy errors.Is(err, providers.ErrAntigravityAuth).
func TestListProviderModels_GoogleAuthFailureIsNotMasked(t *testing.T) {
	svc := providers.New(nil, nil, nil)
	svc.SetAntigravityModelLister(func(context.Context, string) ([]providers.AgyModel, error) {
		return nil, providers.ErrAntigravityAuth
	})
	res, err := svc.ListProviderModels(context.Background(), testTenant, googleProviderID)
	if err == nil {
		t.Fatalf("a signed-out host returned %d models and no error — a populated list on a signed-out host "+
			"tells the operator the tier works when every turn against it fails", len(res.Models))
	}
	if !errors.Is(err, providers.ErrAntigravityAuth) {
		t.Errorf("error %v does not match providers.ErrAntigravityAuth", err)
	}
	if errors.Is(err, db.ErrNotFound) {
		t.Error("the auth failure also matched db.ErrNotFound — it would map to CodeNotFound")
	}
	if !strings.Contains(err.Error(), adapter.AntigravityAuthRequiredMessage) {
		t.Errorf("error %q does not carry adapter.AntigravityAuthRequiredMessage", err.Error())
	}
	if strings.Contains(err.Error(), "not_found") {
		t.Errorf("error %q carries a not_found prefix", err.Error())
	}
}

// TestListProviderModels_GoogleAuthFailureMapsToFailedPrecondition is AC5 through the real RPC handler:
// connect.CodeFailedPrecondition whose message IS the actionable sentence, not `[not_found] db: not found`.
func TestListProviderModels_GoogleAuthFailureMapsToFailedPrecondition(t *testing.T) {
	h := newProvidersTestHandler(t)
	h.Service().SetAntigravityModelLister(func(context.Context, string) ([]providers.AgyModel, error) {
		return nil, providers.ErrAntigravityAuth
	})
	ctx := tenant.WithID(context.Background(), testTenant)

	resp, err := h.ListProviderModels(ctx, connect.NewRequest(&apiv1.ProviderModelsRequest{ProviderId: googleProviderID}))
	if err == nil {
		t.Fatalf("a signed-out host returned %d models and no error", len(resp.Msg.GetModels()))
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("error %v is not a *connect.Error", err)
	}
	if connErr.Code() != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want CodeFailedPrecondition (a signed-out host is a fixable precondition, and "+
			"CodeNotFound is the bug being fixed)", connErr.Code())
	}
	if got := connErr.Message(); got != adapter.AntigravityAuthRequiredMessage {
		t.Errorf("message = %q, want adapter.AntigravityAuthRequiredMessage verbatim", got)
	}
	if strings.Contains(connErr.Message(), "not_found") {
		t.Errorf("message %q carries a not_found prefix", connErr.Message())
	}
}

// TestListProviderModels_GoogleHandlerReturnsModels is AC1's shape without a live agy: the RPC must return
// a populated model tier for provider `google`, not an error.
func TestListProviderModels_GoogleHandlerReturnsModels(t *testing.T) {
	h := newProvidersTestHandler(t)
	ctx := tenant.WithID(context.Background(), testTenant)

	resp, err := h.ListProviderModels(ctx, connect.NewRequest(&apiv1.ProviderModelsRequest{ProviderId: googleProviderID}))
	if err != nil {
		t.Fatalf("ListProviderModels(%q) RPC: %v", googleProviderID, err)
	}
	if len(resp.Msg.GetModels()) == 0 {
		t.Fatal("the RPC returned an empty model list")
	}
	if !resp.Msg.GetEnabled() {
		t.Error("enabled = false, want true")
	}
	for _, m := range resp.Msg.GetModels() {
		if strings.Contains(m.GetId(), "/") {
			t.Errorf("model id %q contains a '/' — it must be a bare agy modelId", m.GetId())
		}
	}
}

// TestListProviderModels_OtherProvidersStillNotFound pins that the branch is NARROW: a genuinely unknown
// provider keeps its ErrNotFound, so the fix does not turn every unknown id into a model list.
func TestListProviderModels_OtherProvidersStillNotFound(t *testing.T) {
	svc, pool := newProvidersTestService(t)
	ensureTenant(t, pool, testTenant)
	if _, err := svc.ListProviderModels(context.Background(), testTenant, "definitely-not-a-provider"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown provider error = %v, want db.ErrNotFound (the branch must be keyed on %q only)",
			err, googleProviderID)
	}
}

// newProvidersTestHandler builds the real RPC handler the GUI and the TUI call, over a nil pool: every test
// that uses it drives the antigravity branch, which must answer without a database.
func newProvidersTestHandler(t *testing.T) *providers.Handler {
	t.Helper()
	return providers.NewHandler(nil, nil, nil)
}

// recordedAgyModelIDs parses agy 1.5.13's own recorded `session/new` result and returns the
// `models.availableModels` ids. Fixtures are the measurement; the snapshot must agree with them.
func recordedAgyModelIDs(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "antigravity", "testdata", "acp", "handshake_auth.jsonl"))
	if err != nil {
		t.Fatalf("read the acp recording: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `availableModels`) || !strings.Contains(line, `"s2c"`) {
			continue
		}
		var entry struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		var doc struct {
			Result struct {
				Models struct {
					AvailableModels []struct {
						ModelID string `json:"modelId"`
					} `json:"availableModels"`
				} `json:"models"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(entry.Line), &doc); err != nil {
			continue
		}
		ids := map[string]bool{}
		for _, m := range doc.Result.Models.AvailableModels {
			if m.ModelID != "" {
				ids[m.ModelID] = true
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	t.Fatal("the acp recording carries no session/new availableModels — the snapshot has no measurement to agree with")
	return nil
}
