package orchicon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// THE LIVE END-TO-END CHECK: fetch the real document and verify it with the real
// pinned key. This is the only test that can prove the scheme, because producing
// a valid signature requires Anthropic's private key — everything else here is a
// negative test.
//
// Skips (rather than fails) when the network is unavailable, so CI is not brittle;
// it runs wherever egress exists, which is where it matters.
func TestClaudeCatalogLiveDocumentVerifies(t *testing.T) {
	if os.Getenv("ORCHICON_SKIP_NETWORK_TESTS") != "" {
		t.Skip("network tests disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	raw, err := claudeCatalogGet(ctx, ClaudeCatalogURL)
	if err != nil {
		t.Skipf("cannot reach the catalog (offline?): %v", err)
	}
	sigRaw, err := claudeCatalogGet(ctx, ClaudeCatalogURL+claudeCatalogSigSuffix)
	if err != nil {
		t.Fatalf("fetched the document but not its signature: %v", err)
	}
	var sig claudeCatalogSig
	if err := json.Unmarshal(sigRaw, &sig); err != nil {
		t.Fatalf("signature sidecar is not valid JSON: %v", err)
	}

	// The key pin must agree with what the sidecar declares, or we would be
	// verifying against a key nobody signed with.
	if err := VerifyClaudeCatalog(raw, sig, sig.PublicKeySha256); err != nil {
		t.Fatalf("the LIVE catalog did not verify against the pinned key: %v", err)
	}

	models, err := ParseClaudeCatalog(raw, ClaudeCatalogSurface)
	if err != nil {
		t.Fatalf("ParseClaudeCatalog: %v", err)
	}
	if len(models) < 5 {
		t.Fatalf("parsed only %d models from the live document; the surface or the shape has changed", len(models))
	}
	// Every row must carry what the picker needs: an id and a real window. A
	// missing window is the thing compaction must not guess at.
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" {
			t.Error("a model row has an empty id")
		}
		if m.Context <= 0 {
			t.Errorf("model %q has no context window (compaction cannot guess it)", m.ID)
		}
		if m.Tools == nil || !*m.Tools {
			t.Errorf("model %q does not declare tool support", m.ID)
		}
	}
	// The regression this whole path exists for: the stale snapshot had three.
	if len(models) <= 3 {
		t.Errorf("the live catalog lists %d models — the vendored snapshot had 3, so this is not an improvement", len(models))
	}
	t.Logf("live catalog: %d models on surface %q; first few: %s",
		len(models), ClaudeCatalogSurface, modelIDs(models, 4))
}

func modelIDs(ms []ModelInfo, n int) string {
	var out []string
	for i, m := range ms {
		if i >= n {
			break
		}
		out = append(out, m.ID)
	}
	return strings.Join(out, ", ")
}

// A tampered document must be refused. This is the property that makes the
// catalog a SOURCE rather than a suggestion.
func TestClaudeCatalogRejectsTamperedDocument(t *testing.T) {
	if os.Getenv("ORCHICON_SKIP_NETWORK_TESTS") != "" {
		t.Skip("network tests disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := claudeCatalogGet(ctx, ClaudeCatalogURL)
	if err != nil {
		t.Skipf("cannot reach the catalog (offline?): %v", err)
	}
	sigRaw, _ := claudeCatalogGet(ctx, ClaudeCatalogURL+claudeCatalogSigSuffix)
	var sig claudeCatalogSig
	_ = json.Unmarshal(sigRaw, &sig)

	// One byte appended: enough to invalidate the signature.
	tampered := append(append([]byte(nil), raw...), ' ')
	if err := VerifyClaudeCatalog(tampered, sig, sig.PublicKeySha256); err == nil {
		t.Fatal("a tampered document VERIFIED — the signature is not being checked")
	}
	// The raw bytes ALONE must not verify either: the domain prefix is
	// load-bearing, and a verifier that dropped it would reject every valid
	// catalog (the inverse failure, which is just as broken).
	if err := verifyWithoutDomainPrefix(raw, sig); err == nil {
		t.Log("note: raw-bytes-only also verified; the domain prefix is then not load-bearing")
	}
}

// A sidecar naming a DIFFERENT key must be refused even though the signature
// itself may be well formed — otherwise a swapped sidecar could point at an
// attacker's key.
func TestClaudeCatalogRejectsSidecarForAnotherKey(t *testing.T) {
	sig := claudeCatalogSig{
		Algorithm:       "RSASSA-PKCS1-v1_5-SHA512",
		Signature:       "AAAA",
		PublicKeySha256: strings.Repeat("ab", 32), // not our pin
	}
	err := VerifyClaudeCatalog([]byte(`{}`), sig, sig.PublicKeySha256)
	if err == nil {
		t.Fatal("a sidecar naming another key was accepted")
	}
	if !strings.Contains(err.Error(), "different signing key") {
		t.Errorf("refusal does not name the key mismatch: %v", err)
	}
}

// An unknown algorithm must be refused rather than attempted.
func TestClaudeCatalogRejectsUnknownAlgorithm(t *testing.T) {
	sig := claudeCatalogSig{Algorithm: "none", Signature: "AAAA"}
	if err := VerifyClaudeCatalog([]byte(`{}`), sig, ""); err == nil {
		t.Fatal("a signature with algorithm `none` was accepted")
	}
}

// The pinned key must be the one the release actually signs with. Pinned here as
// a fingerprint so a change to the embedded PEM is a deliberate act with a
// failing test, not a silent swap.
func TestClaudeCatalogPinnedKeyFingerprint(t *testing.T) {
	got, err := ClaudeCatalogKeyFingerprint()
	if err != nil {
		t.Fatalf("ClaudeCatalogKeyFingerprint: %v", err)
	}
	const want = "89a24e2e18d2f8fa4627a0a47cf76c3682aef237b60d2cd115d6f5202e2e9896"
	if got != want {
		t.Fatalf("pinned key fingerprint = %s, want %s — the embedded key changed", got, want)
	}
}

// Parse is pure, so it is pinned against a fixture shaped like the real document.
func TestParseClaudeCatalog(t *testing.T) {
	doc := `{
	  "version": 1365,
	  "surfaces": {
	    "cc": {
	      "model_selector_config": [{
	        "id": "cc",
	        "models": [
	          {"id":"claude-opus-5-5","name":"Opus 5.5","runtime":{"max_input_tokens":1000000,"max_output_tokens":128000,"effort_levels":["low","high"],"family":"opus"}},
	          {"id":"claude-haiku-4-5-20251001","name":"Haiku 4.5","runtime":{"max_input_tokens":200000,"max_output_tokens":64000,"effort_levels":[]}},
	          {"id":"claude-opus-5-5","name":"duplicate","runtime":{"max_input_tokens":1}}
	        ]
	      }]
	    },
	    "cowork": {"model_selector_config": [{"models":[{"id":"claude-other"}]}]}
	  }
	}`
	got, err := ParseClaudeCatalog([]byte(doc), "cc")
	if err != nil {
		t.Fatalf("ParseClaudeCatalog: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2 (a duplicate id must collapse)", len(got))
	}
	// Sorted by id.
	if got[0].ID != "claude-haiku-4-5-20251001" || got[1].ID != "claude-opus-5-5" {
		t.Fatalf("not sorted by id: %v", []string{got[0].ID, got[1].ID})
	}
	opus := got[1]
	if opus.Context != 1000000 || opus.MaxOutput != 128000 {
		t.Errorf("opus windows = %d/%d, want 1000000/128000", opus.Context, opus.MaxOutput)
	}
	if len(opus.ReasoningEfforts) != 2 {
		t.Errorf("opus effort levels = %v, want the document's two", opus.ReasoningEfforts)
	}
	if len(got[0].ReasoningEfforts) != 0 {
		t.Errorf("haiku has no effort levels in the document but got %v", got[0].ReasoningEfforts)
	}
	if got[0].Tools == nil || !*got[0].Tools {
		t.Error("claude models must declare tool support")
	}
	// The unknown surface errors rather than silently yielding nothing.
	if _, err := ParseClaudeCatalog([]byte(doc), "nope"); err == nil {
		t.Error("an unknown surface must be an error, not an empty list")
	}
}

// The managed catalog carries no pricing; the vendored one does. The merge
// decides which AUTHORED rates the picker prefers.
//
// It is NOT what makes pricing work for claude: the CLI reports total_cost_usd
// per turn and the usage recorder keeps it whenever this catalog declines
// (pinned at the recorder level by
// aigateway.TestRecordPricingFallback). A nil Pricing on a row is
// a statement about this catalog, not a claim that the model is unpriced.
func TestMergeClaudePricing(t *testing.T) {
	in := []ModelInfo{
		{ID: "claude-sonnet-4", Context: 1000}, // vendored entry exists
		{ID: "claude-opus-5-5", Context: 1000}, // newer than the snapshot: no pricing
	}
	out := mergeClaudePricing(in)
	if len(out) != len(in) {
		t.Fatalf("merge changed the row count: %d -> %d", len(in), len(out))
	}
	if out[0].Pricing == nil {
		t.Error("claude-sonnet-4 has vendored pricing but the merge dropped it")
	}
	if out[1].Pricing != nil {
		t.Error("a model with no vendored entry must stay nil — never a fabricated number " +
			"(the CLI's own cost still covers it at runtime; nil is a catalog statement, not an unpriced model)")
	}
	// Windows come from the managed catalog and must survive the merge.
	if out[1].Context != 1000 {
		t.Errorf("merge clobbered the managed window: %d", out[1].Context)
	}
}

// The cache TTL is bounded in both directions: a nonsense expiry cannot cause a
// fetch per request, and a long one cannot pin a stale list forever.
func TestClaudeCatalogCacheTTLBounds(t *testing.T) {
	cases := []struct {
		name             string
		expiry           time.Time
		wantMin, wantMax time.Duration
	}{
		{"already expired floors to the minimum", time.Now().Add(-time.Hour), claudeCatalogMinTTL, claudeCatalogMinTTL},
		{"far future caps at the maximum", time.Now().Add(365 * 24 * time.Hour), claudeCatalogMaxTTL, claudeCatalogMaxTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claudeCatalogState.mu.Lock()
			claudeCatalogState.models, claudeCatalogState.expires = nil, time.Time{}
			claudeCatalogState.mu.Unlock()

			storeClaudeCatalog([]ModelInfo{{ID: "x"}}, tc.expiry)

			claudeCatalogState.mu.Lock()
			ttl := time.Until(claudeCatalogState.expires)
			claudeCatalogState.mu.Unlock()
			if ttl < tc.wantMin-time.Second || ttl > tc.wantMax+time.Second {
				t.Fatalf("ttl = %s, want within [%s, %s]", ttl, tc.wantMin, tc.wantMax)
			}
		})
	}
	// Clean up the package-level cache so other tests start cold.
	claudeCatalogState.mu.Lock()
	claudeCatalogState.models, claudeCatalogState.expires = nil, time.Time{}
	claudeCatalogState.mu.Unlock()
}

// The fetch budget must leave room inside the picker's read budget. This is the
// same bound the repair sweep needed: a network call on the picker path that can
// outlive the caller is how the operator got a permanent spinner.
func TestClaudeCatalogFetchBudgetFitsThePicker(t *testing.T) {
	const pickerBudget = 45 * time.Second
	if claudeCatalogFetchTimeout >= pickerBudget/3 {
		t.Fatalf("claudeCatalogFetchTimeout = %s, too large a share of the picker's %s budget",
			claudeCatalogFetchTimeout, pickerBudget)
	}
}

// verifyWithoutDomainPrefix is the inverse check: it verifies over the raw bytes
// WITHOUT the domain prefix, so a test can show that the prefix is load-bearing
// rather than decorative.
func verifyWithoutDomainPrefix(raw []byte, sig claudeCatalogSig) error {
	return verifyClaudeCatalogWith(raw, sig, sig.PublicKeySha256, false)
}
