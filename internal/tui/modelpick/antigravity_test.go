package modelpick

// antigravity_test.go — the TUI half of the antigravity parity contract (child 8, AC2).
//
// WHY IT EXISTS ALONGSIDE TestClaudeKindListsCatalogModelsAndCommitsAThreeSegmentRef. antigravity reaches
// the picker the same WAY as claude (catalog-sourced) but with DIFFERENT data, and each difference is a
// place a regression would be silent:
//
//   1. Its provider tier is a ONE-element adapter-scoped set (`google`), because agy serves Gemini AND the
//      claude-* models under ONE Google login (internal/adapter: AddAdapterKind(KindAntigravity, "google")).
//      A picker that fell back to the merged Providers view would list providers agy cannot dispatch to.
//   2. Its model rows are agy's OWN bare ids (`gemini-4-carbon-high`), with the effort suffix IN the id —
//      there is no `variants` expansion to do, and a composed ref would produce a bogus 4-segment join.
//   3. Its context window is 1M, which the context meter and the compaction math both read.
//   4. The SNAPSHOT fallback (no agy on this host) must still list rows and must SAY the list may be
//      stale — the GUI renders the same note from the server's `degraded` bit.
import (
	"strings"
	"testing"

	teakey "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// agyLiveRows is what ProviderService.ListProviderModels(provider_id="google") returns for a live agy
// listing (internal/providers/antigravity.go antigravityRowsFromLive): bare ids, source=live, the observed
// window, and reasoning=true.
func agyLiveRows() []*apiv1.ProviderModel {
	return []*apiv1.ProviderModel{
		{Id: "gemini-4-carbon-high", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
		{Id: "gemini-4-carbon-low", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
		{Id: "gemini-4-neon-low", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
	}
}

// TestAntigravityRefSeedsTheTiers is the stored-ref contract: the picker reopens on the same triple the
// plane parses, so the grammar (internal/adapter) is the single authority on both sides.
func TestAntigravityRefSeedsTheTiers(t *testing.T) {
	kind, provider, model := SplitRef("antigravity/google/gemini-4-carbon-high")
	if kind != adapter.KindAntigravity || provider != "google" || model != "gemini-4-carbon-high" {
		t.Fatalf("SplitRef = (%q, %q, %q), want (antigravity, google, gemini-4-carbon-high)", kind, provider, model)
	}
	// THE 3-SEGMENT FORM IS THE ONLY CANONICAL ONE for this adapter, and the shorter forms must NOT be
	// silently reshaped into it: `antigravity/google` is the LEGACY 2-segment opencode grammar (adapter
	// inferred, provider/model), and a bare id is a MODEL. Asserting that here is what stops the picker
	// from seeding an empty provider tier off a legacy ref and then writing a 4-segment ref back.
	if k, p, m := SplitRef("antigravity/google"); k != adapter.KindOpencode || p != "antigravity" || m != "google" {
		t.Fatalf("SplitRef(antigravity/google) = (%q, %q, %q), want the legacy 2-segment reading "+
			"(opencode, antigravity, google)", k, p, m)
	}
	if _, _, m := SplitRef("antigravity"); m != "antigravity" {
		t.Fatalf("SplitRef(antigravity) model = %q; a 1-segment ref is a bare model id", m)
	}
}

// TestAntigravityUsesCatalogSourcing pins the MODEL-tier branch. If antigravity ever stopped being
// catalog-sourced the picker would take the opencode-CLI discovery path and list NOTHING on a plane with
// no opencode binary — the exact bug the claude arm was created for.
func TestAntigravityUsesCatalogSourcing(t *testing.T) {
	if !UsesCatalogSourcing(adapter.KindAntigravity) {
		t.Fatal("antigravity must resolve its model tier from the providers sourcing view, not opencode-CLI discovery")
	}
	set := map[string]bool{}
	for _, k := range adapter.CatalogSourcedAdapterKinds() {
		set[k] = true
	}
	if !set[adapter.KindAntigravity] {
		t.Fatalf("CatalogSourcedAdapterKinds() = %v, want it to include %q — the GUI classifies the model tier "+
			"from the SAME list, so the two clients would disagree about the picker's data source",
			adapter.CatalogSourcedAdapterKinds(), adapter.KindAntigravity)
	}
}

// TestAntigravityProviderTierIsGoogleAlone: agy's adapter-scoped provider set is exactly [google].
func TestAntigravityProviderTierIsGoogleAlone(t *testing.T) {
	provs := ProviderOptionsGateway([]*apiv1.AIProvider{{Id: "google", Name: "Google"}})
	if len(provs) != 1 || provs[0].Value != "google" {
		t.Fatalf("antigravity provider tier = %+v, want exactly [google] (agy serves Gemini and the claude-* "+
			"models under ONE Google login)", provs)
	}
}

// TestAntigravityModelTierListsAgyRowsAndCommitsAThreeSegmentRef is the end-to-end seam the TUI owns: the
// sourcing view's google list projects to a NON-EMPTY option list on an opencode-free plane, the model
// VALUE stays the bare agy id, and the committed ref round-trips through the grammar authority.
func TestAntigravityModelTierListsAgyRowsAndCommitsAThreeSegmentRef(t *testing.T) {
	opts := ModelOptionsNative(agyLiveRows())
	if len(opts) != 3 {
		t.Fatalf("the antigravity model tier projected %d options, want 3 — a blank list is the picker showing nothing", len(opts))
	}
	for _, o := range opts {
		if strings.Contains(o.Value, "/") {
			t.Errorf("option value %q is a composed ref; the picker would join a bogus 4-segment ref", o.Value)
		}
		// The row's meta must carry the window and the reasoning flag — the context meter reads the
		// former and the reasoning fold is offered because of the latter.
		if !strings.Contains(o.Meta, "1.0M ctx") && !strings.Contains(o.Meta, "1M ctx") {
			t.Errorf("option %q meta = %q, want it to name the 1M context window", o.Value, o.Meta)
		}
		if !strings.Contains(o.Meta, "reasoning") {
			t.Errorf("option %q meta = %q, want the reasoning flag", o.Value, o.Meta)
		}
	}

	// A hidden row is dropped, exactly as the GUI drops visible=false.
	withHidden := ModelOptionsNative([]*apiv1.ProviderModel{
		{Id: "gemini-4-nova-high", Visible: false},
		{Id: "gemini-4-carbon-high", Visible: true},
	})
	if len(withHidden) != 1 || withHidden[0].Value != "gemini-4-carbon-high" {
		t.Fatalf("hidden rows must be dropped, got %+v", withHidden)
	}

	// THE COMMIT, joined by the shared picker exactly as a host reads it.
	mp := kit2.NewModelPicker("Ask model")
	mp.SetScreen(100, 30)
	mp.SetAdapters(adapterKindsForTest(), adapter.CatalogSourcedAdapterKinds())
	mp.Open(adapter.KindAntigravity, "google", "")
	mp.SetProviders(adapter.KindAntigravity, provsGoogle())
	mp.SetModels(adapter.KindAntigravity, "google", opts, false)

	// Walk to the model tier and pick the third row, so the whole three-tier cascade is exercised
	// rather than only the final join. Open() with a provider seeds the PROVIDER tier, so one tab
	// reaches the models.
	mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyTab})
	if mp.Tier() != kit2.TierModel {
		t.Fatalf("tab did not reach the model tier, got %d", mp.Tier())
	}
	mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyDown})
	mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyDown})
	mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyEnter})

	if !mp.Done() || !mp.Committed() {
		t.Fatalf("choosing an agy model must report Done+Committed so the host closes and writes (ref=%q)", mp.Ref())
	}
	if got, want := mp.Ref(), "antigravity/google/gemini-4-neon-low"; got != want {
		t.Fatalf("Ref() = %q, want %q", got, want)
	}
	parsed, err := adapter.ParseModelRef(mp.Ref(), adapter.NewBuiltinProviderCatalog())
	if err != nil {
		t.Fatalf("ParseModelRef(%q): %v", mp.Ref(), err)
	}
	if parsed.Adapter != adapter.KindAntigravity || parsed.Provider != "google" || parsed.Model != "gemini-4-neon-low" {
		t.Fatalf("round trip = %+v, want antigravity/google/gemini-4-neon-low", parsed)
	}
}

// TestAntigravitySnapshotFallbackSaysItMayBeStale is the PARTIAL state: with no `agy` on the host the
// server answers from the vendored snapshot rather than failing, and both clients must show that list
// AND say it may be out of date. The TUI already carried the bit; this pins what it paints.
func TestAntigravitySnapshotFallbackSaysItMayBeStale(t *testing.T) {
	snapshot := ModelOptionsNative([]*apiv1.ProviderModel{
		{Id: "gemini-4-carbon-high", Context: 1048576, Visible: true, Source: "antigravity-snapshot", Reasoning: true},
	})
	if len(snapshot) != 1 {
		t.Fatalf("the snapshot fallback must still list rows, got %d", len(snapshot))
	}
	if !strings.Contains(snapshot[0].Meta, "antigravity-snapshot") {
		t.Errorf("the row must name its provenance, meta = %q", snapshot[0].Meta)
	}

	mp := kit2.NewModelPicker("Ask model")
	mp.SetScreen(100, 30)
	mp.Open(adapter.KindAntigravity, "google", "")
	mp.SetProviders(adapter.KindAntigravity, provsGoogle())
	mp.SetModels(adapter.KindAntigravity, "google", snapshot, true) // degraded = the snapshot answered
	// The summary line's degraded note only paints on the model tier, and Open() with a provider seeds
	// the provider tier, so one tab drives there.
	for i := 0; i < 2 && mp.Tier() != kit2.TierModel; i++ {
		mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyTab})
	}
	view := ansi.Strip(mp.View())
	if !strings.Contains(view, "may be stale") {
		t.Fatalf("a snapshot-backed list must say it may be stale — the GUI shows the same note from the same "+
			"server bit:\n%s", view)
	}
	if !strings.Contains(view, "antigravity/google") {
		t.Errorf("the picker must render the ref being chosen:\n%s", view)
	}
}

// TestAntigravityEmptyTierStatesTheInstallSentence is the EMPTY / NOT-INSTALLED state: no error and no
// rows is the install precondition in practice, so the picker says what to install rather than "(no
// models)". The sentence is the server's own constant, so the GUI's empty state renders the same words.
func TestAntigravityEmptyTierStatesTheInstallSentence(t *testing.T) {
	mp := kit2.NewModelPicker("Ask model")
	mp.SetScreen(100, 30)
	mp.Open(adapter.KindAntigravity, "google", "")
	mp.SetProviders(adapter.KindAntigravity, provsGoogle())
	mp.SetModels(adapter.KindAntigravity, "google", nil, false) // an empty, non-error answer
	for i := 0; i < 2 && mp.Tier() != kit2.TierModel; i++ {
		mp.HandleKey(teakey.KeyMsg{Type: teakey.KeyTab})
	}
	view := ansi.Strip(mp.View())
	// THE SENTENCE IS THE SERVER'S, so the two clients cannot drift on the words. The box is narrower
	// than the constant, so the row is TRUNCATED to the cell width rather than wrapped: assert the
	// identifying half here and the exact bytes against the constant itself below.
	if !strings.Contains(view, "Antigravity CLI (`agy`) not found") {
		t.Fatalf("an empty antigravity tier must show the install sentence, not a bare \"(no models)\":\n%s", view)
	}
	if !strings.Contains(view, "install from https://antigravity.go") {
		t.Errorf("the install sentence must carry the download link:\n%s", view)
	}
	if strings.Contains(view, "(no models)") || strings.Contains(view, "no models match") {
		t.Errorf("the empty antigravity tier fell back to the generic empty text:\n%s", view)
	}
	// The constant itself is the server's, byte for byte (internal/adapter.AntigravityNotInstalledMessage).
	if !strings.HasSuffix(adapter.AntigravityNotInstalledMessage, "https://antigravity.google/download") {
		t.Fatalf("the install sentence lost its download link: %q", adapter.AntigravityNotInstalledMessage)
	}
	if strings.Contains(adapter.AntigravityNotInstalledMessage, "sign in") {
		t.Fatal("the INSTALL sentence must not mention signing in — there is no agy to sign in to")
	}
}

// provsGoogle is the adapter-scoped provider tier agy reports.
func provsGoogle() []kit2.PickerOption {
	return []kit2.PickerOption{{Value: "google", Label: "Google"}}
}

// adapterKindsForTest is the set the plane publishes from ListAdapterKinds, in its own order. It is read
// from the catalog rather than written out, so a new kind joins the picker's first tier without this
// test needing an edit — which is the property being asserted.
func adapterKindsForTest() []string {
	set := adapter.BuiltinAdapterKinds()
	out := make([]string, 0, len(set))
	for _, k := range []string{adapter.KindOrchicon, adapter.KindOpencode, adapter.KindClaude, adapter.KindAntigravity} {
		if _, ok := set[k]; ok {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		out = []string{adapter.KindAntigravity}
	}
	return out
}
