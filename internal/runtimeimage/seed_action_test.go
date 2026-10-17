package runtimeimage

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// TestDecideSeedActionAbsentStartsNoBuild is AC 10/11: the criterion that stops
// the on-demand-image change from making the install SLOWER instead of
// lighter. With nothing downloaded, the row must be marked absent and NO build
// may be started — the decision returns seedMarkAbsent, never seedBuild.
func TestDecideSeedActionAbsentStartsNoBuild(t *testing.T) {
	variant := stockVariants[0]
	row := db.RuntimeImageRow{Source: "stock", Status: "draft", Version: 1}
	missing := runtime.ImageInfo{Ref: "orchicon-runtime:latest"} // Present=false
	if got := decideSeedAction(row, missing, variant, true, true); got != seedMarkAbsent {
		t.Fatalf("absent image on a fresh row = %v, want seedMarkAbsent (no build)", got)
	}
	// Same verdict for a row that was already marked absent: idempotent, still
	// no build, so a reboot loop never turns into a rebuild loop.
	row.Status = RuntimeImageStatusAbsent
	if got := decideSeedAction(row, missing, variant, true, true); got != seedMarkAbsent {
		t.Fatalf("absent image on an absent row = %v, want seedMarkAbsent", got)
	}
}

// TestDecideSeedAction distinguishes absent-and-not-building from
// stale-needing-roll-forward: a PRESENT but stale image keeps the pre-change
// auto-build.
func TestDecideSeedActionStalePresentStillBuilds(t *testing.T) {
	variant := stockVariants[0]
	row := db.RuntimeImageRow{Source: "stock", Status: "draft", Version: 2}
	stale := runtime.ImageInfo{Ref: "orchicon-runtime:latest", Present: true, RuntimeVersion: "999.0.0"}
	if got := decideSeedAction(row, stale, variant, true, false); got != seedBuild {
		t.Fatalf("present-but-stale image = %v, want seedBuild (roll-forward still auto-builds)", got)
	}
	// Present but carrying no version label: a locally-built image, which also
	// keeps the old auto-build behaviour. Only true absence changes.
	unlabelled := runtime.ImageInfo{Ref: "orchicon-runtime:latest", Present: true}
	if got := decideSeedAction(row, unlabelled, variant, true, true); got != seedBuild {
		t.Fatalf("present-without-labels = %v, want seedBuild", got)
	}
}

// TestDecideSeedActionTable pins the remaining branches.
func TestDecideSeedActionTable(t *testing.T) {
	variant := stockVariants[0]
	current := runtime.ImageInfo{Ref: "orchicon-runtime:latest", Present: true}
	missing := runtime.ImageInfo{}

	cases := []struct {
		name              string
		row               db.RuntimeImageRow
		info              runtime.ImageInfo
		pristine, current bool
		want              seedAction
	}{
		{"a user-owned row is never touched", db.RuntimeImageRow{Source: "tenant", Status: "draft", Version: 1}, current, true, true, seedSkip},
		{"a diverged seed is never touched", db.RuntimeImageRow{Source: "stock", Status: "draft", Version: 1}, current, false, true, seedSkip},
		{"a failed build is never auto-retried", db.RuntimeImageRow{Source: "stock", Status: "failed", Version: 1}, current, true, true, seedSkip},
		{"a failed build is not reclassified as absent", db.RuntimeImageRow{Source: "stock", Status: "failed", Version: 1}, missing, true, true, seedSkip},
		{"a current pristine row is left alone", db.RuntimeImageRow{Source: "stock", Status: "ready", Version: 3, BuiltVersion: 3}, current, true, true, seedSkip},
		{"a ready row whose built version is behind still rebuilds", db.RuntimeImageRow{Source: "stock", Status: "ready", Version: 3, BuiltVersion: 2}, current, true, true, seedBuild},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideSeedAction(c.row, c.info, variant, c.pristine, c.current); got != c.want {
				t.Fatalf("decideSeedAction = %v, want %v", got, c.want)
			}
		})
	}
}

// TestStockVariantsIncludeWebResearch is AC 1's fourth variant: the seeder must
// know :web-research, or the four shipped images cannot all be seeded.
func TestStockVariantsIncludeWebResearch(t *testing.T) {
	found := false
	for _, v := range stockVariants {
		if v.Variant == "web-research" {
			found = true
		}
	}
	if !found {
		t.Fatal("stockVariants has no web-research entry")
	}
	if _, ok := resolveVariant("ghcr.io/beardedparrott/orchicon-runtime:web-research", "ghcr.io/beardedparrott/orchicon-runtime:latest"); !ok {
		t.Fatal("resolveVariant does not map the :web-research tag to a stock variant")
	}
}
