package runtimeimage

import (
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
)

// A stock row stores NO Dockerfile and builds from the template shipped in the binary. The failure this
// pins: the stock-dev row kept a stale stored copy whose hash marker no longer matched, the seeder
// classified it "operator-edited" and backed off forever, and builds from it produced a 14 GB image after
// the shipped template had been fixed.
func TestStockRowWithEmptyOverrideBuildsFromTheShippedTemplate(t *testing.T) {
	for _, v := range stockVariants {
		want, err := assets.RuntimeImageTemplatesFS.ReadFile(v.File)
		if err != nil {
			t.Fatalf("read %s: %v", v.File, err)
		}
		row := db.RuntimeImageRow{Source: "stock", Slug: stockSlug(v), IncludeAdapters: true}
		got := generatedDockerfile(row, "irrelevant:base")
		if !strings.HasPrefix(got, strings.TrimRight(string(want), "\n")) {
			t.Errorf("%s: a stock row with an empty override must build from %s, got a different Dockerfile", v.Variant, v.File)
		}
	}
}

// A customised stock row (non-empty override) and a custom row must be left exactly as the operator wrote
// them — following the template is only for the row that stored nothing.
func TestCustomisedAndCustomRowsKeepTheirOwnDockerfile(t *testing.T) {
	own := "FROM scratch\nRUN echo mine\n"
	for _, src := range []string{"stock", "custom"} {
		row := db.RuntimeImageRow{Source: src, Slug: "stock-dev", DockerfileOverride: own}
		if got := generatedDockerfile(row, "b"); got != own {
			t.Errorf("source=%s: the operator's Dockerfile was replaced: %q", src, got)
		}
	}
	if _, ok := stockTemplateForRow(db.RuntimeImageRow{Source: "custom", Slug: "stock-dev"}); ok {
		t.Error("a custom row must never resolve to a shipped template")
	}
}
