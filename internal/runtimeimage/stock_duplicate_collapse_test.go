package runtimeimage

// stock_duplicate_collapse_test.go — TWO ROWS FOR ONE VARIANT IS THE BUG; THE COLLAPSE MUST STICK.
//
// Reported from a long-lived instance whose Runtime Images page showed the development image twice:
//
//	stock-dev   not downloaded   v1
//	stock-dev   ready            v9 · built v9
//
// It is not cosmetic: with two rows for one variant, "fetch this one" can build a row that is not the one bound
// to a work item, and from then on the two diverge silently.
//
// The collapse already existed in stockRowForVariant, but the READ path — the only path a running instance
// exercises, since it is not re-seeded — rolled the transaction back on the next line. So the duplicate was
// found, staged for removal, and discarded, on every single page load.

import (
	"os"
	"strings"
	"testing"
)

// TestTheReadPathCommitsTheDuplicateCollapse is the wiring assertion: EnsureCatalogRows must COMMIT when it
// removed rows, not roll back.
//
// It reads the source because the behaviour needs a database and a seeded duplicate to observe, and the defect
// is precisely an ordering between two calls two lines apart — which is what a reader can check and a unit test
// without a DB cannot.
func TestTheReadPathCommitsTheDuplicateCollapse(t *testing.T) {
	b, err := os.ReadFile("seed.go")
	if err != nil {
		t.Fatalf("read seed.go: %v", err)
	}
	src := string(b)

	i := strings.Index(src, "func EnsureCatalogRows")
	if i < 0 {
		t.Fatal("EnsureCatalogRows is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\n// cannedRowRow"); j > 0 {
		body = body[:j]
	}

	if !strings.Contains(body, "removed, err := stockRowForVariant") &&
		!strings.Contains(body, "removed, err = stockRowForVariant") {
		t.Fatal("EnsureCatalogRows no longer observes how many rows the collapse removed — it cannot know " +
			"whether there is anything to commit")
	}
	if !strings.Contains(body, "if removed > 0") {
		t.Error("EnsureCatalogRows does not branch on removed > 0 — a collapse would be rolled back with " +
			"the rest of the read transaction, which is exactly why the duplicate survived")
	}
	if !strings.Contains(body, "ttx.Commit(ctx)") {
		t.Error("EnsureCatalogRows never commits — the staged duplicate deletes are discarded")
	}

	// AND THE COLLAPSE MUST STILL REFUSE A DIVERGED ROW. Deleting a row to satisfy a constraint would destroy
	// operator edits; the seeder already knows this and the read path inherits it through the same function.
	if !strings.Contains(src, "keeping a diverged duplicate") {
		t.Error("the diverged-row guard is gone: a collapse would now delete a row carrying operator edits")
	}
}

// TestTheDatabaseEnforcesOneStockRowPerSlug is the root cause, asserted where it lives.
//
// Application cleanup runs only when something calls it — and the reported instance showed the duplicate for
// long enough to reach a screenshot. A partial unique index makes the state unrepresentable instead.
func TestTheDatabaseEnforcesOneStockRowPerSlug(t *testing.T) {
	entries, err := os.ReadDir("../../db/migrations")
	if err != nil {
		t.Skipf("migrations not readable: %v", err)
	}
	var found string
	for _, e := range entries {
		if !strings.Contains(e.Name(), "stock_slug_unique") {
			continue
		}
		b, rerr := os.ReadFile("../../db/migrations/" + e.Name())
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		found = string(b)
	}
	if found == "" {
		t.Fatal("no migration adds a unique index on (tenant_id, slug) for source='stock' — nothing at the " +
			"database level prevents the reported duplicate")
	}
	for _, want := range []string{"CREATE UNIQUE INDEX", "WHERE source = 'stock'"} {
		if !strings.Contains(found, want) {
			t.Errorf("the migration is missing %q — a GLOBAL unique index would also block two tenants "+
				"sharing a custom-image name", want)
		}
	}
}
