package runtimeimage

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
)

func TestResolveVariant(t *testing.T) {
	cases := []struct {
		ref, defaultRef string
		wantVariant     string
		wantOK          bool
	}{
		{ref: "orchicon-runtime:local", defaultRef: "orchicon-runtime:local", wantVariant: "base", wantOK: true},
		{ref: "ghcr.io/beardedparrott/orchicon-runtime:latest", defaultRef: "ghcr.io/beardedparrott/orchicon-runtime:latest", wantVariant: "base", wantOK: true},
		{ref: "orchicon-runtime:local-gui", defaultRef: "orchicon-runtime:local", wantVariant: "gui", wantOK: true},
		{ref: "orchicon-runtime:gui", defaultRef: "orchicon-runtime:local", wantVariant: "gui", wantOK: true},
		{ref: "ghcr.io/beardedparrott/orchicon-runtime:gui-v0.1.190", defaultRef: "ghcr.io/beardedparrott/orchicon-runtime:latest", wantVariant: "gui", wantOK: true},
		{ref: "orchicon-runtime:orchicon-dev", defaultRef: "orchicon-runtime:local", wantVariant: "dev", wantOK: true},
		{ref: "ghcr.io/beardedparrott/orchicon-runtime:dev-v0.1.190", defaultRef: "ghcr.io/beardedparrott/orchicon-runtime:latest", wantVariant: "dev", wantOK: true},
		{ref: "my-custom:latest", defaultRef: "orchicon-runtime:local", wantVariant: "", wantOK: false},
	}
	for _, c := range cases {
		got, ok := resolveVariant(c.ref, c.defaultRef)
		if ok != c.wantOK {
			t.Errorf("resolveVariant(%q) ok=%v want %v", c.ref, ok, c.wantOK)
			continue
		}
		if ok && got.Variant != c.wantVariant {
			t.Errorf("resolveVariant(%q) variant=%q want %q", c.ref, got.Variant, c.wantVariant)
		}
	}
}

func TestSeedVersionStableAndContentSensitive(t *testing.T) {
	a := seedVersion([]byte("FROM debian:bookworm-slim"))
	b := seedVersion([]byte("FROM debian:bookworm-slim"))
	if a != b {
		t.Fatalf("seedVersion not stable: %q != %q", a, b)
	}
	c := seedVersion([]byte("FROM debian:trixie"))
	if a == c {
		t.Fatalf("seedVersion not content-sensitive")
	}
	if len(a) != 12 {
		t.Fatalf("seedVersion length = %d, want 12", len(a))
	}
}

func TestSeedState(t *testing.T) {
	content := []byte("FROM debian:bookworm-slim\nRUN echo hi\n")
	seed := string(content) + "# " + cannedSeedMarker + "=" + seedVersion(content) + "\n"

	editedKeptMarker := string(content) + "RUN echo extra\n" + "# " + cannedSeedMarker + "=" + seedVersion(content) + "\n"
	editedDroppedMarker := string(content) + "RUN echo extra\n"

	oldContent := []byte("FROM debian:trixie\n")
	oldSeed := string(oldContent) + "# " + cannedSeedMarker + "=" + seedVersion(oldContent) + "\n"

	// Content appended AFTER the marker (the UI textarea cursor lands at the
	// end) still leaves the marker self-consistent — but it is a user edit.
	appendedAfterMarker := seed + "# user tweak\n"

	cases := []struct {
		name     string
		override string
		wantPri  bool
		wantCur  bool
	}{
		{name: "current seed", override: seed, wantPri: true, wantCur: true},
		{name: "older-generation seed", override: oldSeed, wantPri: true, wantCur: false},
		{name: "edited body, marker kept", override: editedKeptMarker, wantPri: false, wantCur: false},
		{name: "edited body, marker dropped", override: editedDroppedMarker, wantPri: false, wantCur: false},
		{name: "content after marker", override: appendedAfterMarker, wantPri: false, wantCur: false},
		{name: "no marker", override: string(content), wantPri: false, wantCur: false},
		{name: "empty", override: "", wantPri: false, wantCur: false},
	}
	for _, c := range cases {
		pri, cur := seedState(c.override, content)
		if pri != c.wantPri || cur != c.wantCur {
			t.Errorf("%s: seedState = (pristine=%v, current=%v), want (%v, %v)", c.name, pri, cur, c.wantPri, c.wantCur)
		}
	}
}

func TestStockImageCurrent(t *testing.T) {
	app := "dev" // version.Current().Tag defaults to dev in test builds
	base := stockVariants[0]
	gui := stockVariants[1]
	baseSha := sha12Of(base.File)
	guiSha := sha12Of(gui.File)

	cases := []struct {
		name    string
		info    runtime.ImageInfo
		variant stockVariant
		want    bool
	}{
		{name: "missing image", info: runtime.ImageInfo{}, variant: base, want: false},
		{name: "release base label", info: runtime.ImageInfo{RuntimeVersion: app}, variant: base, want: true},
		{name: "local base label", info: runtime.ImageInfo{RuntimeVersion: app + "-" + baseSha}, variant: base, want: true},
		{name: "release gui label", info: runtime.ImageInfo{RuntimeVersion: app + "-gui"}, variant: gui, want: true},
		{name: "local gui label", info: runtime.ImageInfo{RuntimeVersion: app + "-" + baseSha + "-" + guiSha}, variant: gui, want: true},
		{name: "stale label", info: runtime.ImageInfo{RuntimeVersion: "v0.0.0-deadbeef"}, variant: base, want: false},
		{name: "tenant build (spec-version only)", info: runtime.ImageInfo{SpecVersion: "7"}, variant: base, want: false},
	}
	for _, c := range cases {
		if got := stockImageCurrent(c.info, c.variant); got != c.want {
			t.Errorf("%s: stockImageCurrent = %v, want %v", c.name, got, c.want)
		}
	}
}
