package runtimeimage

// catalogue_persistence_test.go — the catalogue must OUTLIVE a delete, and a delete must reach the disk.
//
// REPORTED: "I saw there was a delete command in the runtime images section. It did remove them from the
// console, and then on restart they came back. The only one that did not come back was the web research
// container for some reason. I think we need to make the runtime image page pull every runtime image
// record that is available in our repo and if they don't exist (been deleted) add them in a non-downloaded
// state. This should happen upon launching the runtime images page in the web, desktop or TUI pages. Also,
// the delete button should definitely delete them off of someone's computer if it doesn't now."
//
// Every clause of that is pinned below. The old behaviour was the INVERSE of all of it: delete removed the
// ROW (the catalogue entry) and kept the IMAGE (the artifact), and the next boot re-derived the rows from
// what the daemon could see on the local disk — which is why three of them "came back" and the fourth,
// having no published tag, did not.

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
//
// THE SECOND ASSERTION IS THE LOAD-BEARING ONE. The boot seeder backs off a row whose template no longer
// hashes to its own marker ("user-diverged — never touch"). A reconciled row must therefore be written as a
// PRISTINE seed, or it would be treated as an operator edit for ever: it would never be rolled forward,
// never re-tagged, and never marked ready even after its image was fetched.
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
	published := map[string]bool{}
	for _, tag := range publishedStockTags() {
		v, ok := resolveVariant(tag, base)
		if !ok {
			t.Fatalf("published tag %q does not resolve to a shipped variant", tag)
		}
		published[stockSlug(v)] = true
	}
	// web-research has a shipped template but NO published tag, so it is not in the downloadable
	// catalogue: seeding it would create a row whose only possible action is a guaranteed failure.
	if published["stock-web-research"] {
		t.Error("web-research is not published by release.yml, so it must not be in the fetched catalogue")
	}
	for _, want := range []string{"stock-base", "stock-gui", "stock-dev"} {
		if !published[want] {
			t.Errorf("%q is missing from the catalogue — the page would not offer it at all", want)
		}
	}
}

// TestACatalogSlugIsStableAcrossTags — one variant is one row, whichever tag it is known by.
//
// The reported screenshot was SIX rows for THREE images: one "not downloaded" per variant from the
// published tag, and one "ready" from the local tag. Keying the row on its tag creates a new row per
// spelling; keying it on the variant is what collapses them.
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
