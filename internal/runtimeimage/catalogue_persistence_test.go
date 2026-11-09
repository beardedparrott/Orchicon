package runtimeimage

// catalogue_persistence_test.go — the catalogue must OUTLIVE a delete, and a delete must reach the
// disk.

import (
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
)

// TestADeleteLeavesAStockRowInTheNotDownloadedState — the row survives, in the state the page draws.
func TestADeleteLeavesAStockRowInTheNotDownloadedState(t *testing.T) {
	if !deleteUndownloadsInsteadOfDeleting("stock") {
		t.Fatal("deleting a stock row must move it to `absent`, not remove it — a catalogue entry that " +
			"disappears takes its Fetch button with it, and the next boot brings it back anyway")
	}
	if deleteUndownloadsInsteadOfDeleting("custom") || deleteUndownloadsInsteadOfDeleting("") {
		t.Error("a tenant-authored row has no published counterpart to re-seed from, so IT must really be deleted")
	}
}

// TestADeleteAlwaysReachesTheDisk — the operator's "delete it off my computer" means the IMAGE.
func TestADeleteAlwaysReachesTheDisk(t *testing.T) {
	// The two exemptions that made the old delete a no-op on the disk.
	if !removesLocalImage("stock", "ghcr.io/beardedparrott/orchicon-runtime:dev") {
		t.Error("a stock row's image must be removed — skipping it is what left the artifact on the host " +
			"while the catalogue entry was destroyed instead")
	}
	if !removesLocalImage("custom", "ghcr.io/beardedparrott/orchicon-runtime:latest") {
		t.Error("the base image must be removable like any other: a built variant stands alone, the base " +
			"re-fetches from the registry, and Deploy already refuses a variant whose base is missing")
	}
	// A row with no tag has nothing to name in an rmi call.
	if removesLocalImage("stock", "") {
		t.Error("a row with no tag must not call rmi — there is no reference to remove")
	}
}

// TestACatalogRowIsNotDownloadedAndIsNotUserDiverged — the row a page-load reconcile creates.
func TestACatalogRowIsNotDownloadedAndIsNotUserDiverged(t *testing.T) {
	variant := stockVariants[1] // :gui — a non-base variant, so the base's special cases are not in play
	content, err := assets.RuntimeImageTemplatesFS.ReadFile(variant.File)
	if err != nil {
		t.Fatalf("read template %s: %v", variant.File, err)
	}
	ref := "ghcr.io/beardedparrott/orchicon-runtime:gui"
	row := cannedRowRow(variant, ref, content, firstPublishedTag())

	if row.Status != RuntimeImageStatusAbsent {
		t.Errorf("status = %q, want %q — a row created without a local image is not downloaded, and the "+
			"page draws exactly that badge", row.Status, RuntimeImageStatusAbsent)
	}
	if row.BuiltVersion != 0 {
		t.Errorf("built_version = %d, want 0: nothing has been built or fetched for this row", row.BuiltVersion)
	}
	if row.Source != "stock" {
		t.Errorf("source = %q, want stock — it is a catalogue entry, which is what makes the next delete "+
			"move it back to absent rather than destroying it", row.Source)
	}
	if row.Slug != stockSlug(variant) {
		t.Errorf("slug = %q, want %q — the slug is the row's identity, and the boot seeder looks rows up "+
			"by it, so a mismatch seeds the same variant twice", row.Slug, stockSlug(variant))
	}
	if row.Tag != ref {
		t.Errorf("tag = %q, want %q", row.Tag, ref)
	}
	pristine, current := seedState(row.DockerfileOverride, content)
	if !pristine || !current {
		t.Fatalf("round-tripping the shipped template produced pristine=%v current=%v, want true/true.\n"+
			"  A row that reads as user-diverged is one the boot seeder refuses to touch for ever.", pristine, current)
	}
}

// TestTheCatalogIsTheSameSetWhicheverPathSeedsIt — the boot seeder and the page-load reconcile must not
// disagree about what the catalogue IS.
func TestTheCatalogIsTheSameSetWhicheverPathSeedsIt(t *testing.T) {
	base := firstPublishedTag()
	//
	// THE QUESTION IS "WHAT CAN THIS HOST END UP WITH", NOT "WHAT IS PUBLISHED". Those were the same
	// question while every variant was released; now only the base is published and the variants are
	// BUILT here, so enumerating the published set would report the catalogue as missing gui and dev —
	// which is what this assertion caught when the split landed.
	catalogued := map[string]bool{}
	for _, tag := range catalogStockTags() {
		v, ok := resolveVariant(tag, base)
		if !ok {
			t.Fatalf("catalogue tag %q does not resolve to a shipped variant", tag)
		}
		catalogued[stockSlug(v)] = true
	}
	// WEB-RESEARCH IS IN, and that reverses an earlier decision deliberately.
	if !catalogued["stock-web-research"] {
		t.Error("web-research has a shipped template and can be built locally, so it belongs in the catalogue")
	}
	for _, want := range []string{"stock-base", "stock-gui", "stock-dev"} {
		if !catalogued[want] {
			t.Errorf("%q is missing from the catalogue — the page would not offer it at all", want)
		}
	}
}

// TestACatalogSlugIsStableAcrossTags — one variant is one row, whichever tag it is known by.
func TestACatalogSlugIsStableAcrossTags(t *testing.T) {
	for _, v := range stockVariants {
		slug := stockSlug(v)
		if slug != "stock-"+v.Variant {
			t.Errorf("stockSlug(%q) = %q", v.Variant, slug)
		}
		if strings.Contains(slug, ":") || strings.Contains(slug, "ghcr.io") {
			t.Errorf("slug %q is derived from a TAG; the row identity must not move when the tag does", slug)
		}
	}
	if stockSlug(stockVariants[0]) == stockSlug(stockVariants[1]) {
		t.Error("two variants share a slug — they would collapse into one row")
	}
}
