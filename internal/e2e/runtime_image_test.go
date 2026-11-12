package e2e

// runtime_image_test.go — THE HARNESS MUST NOT ASSUME ONE MACHINE'S IMAGE TAG.
//
// REPORTED: "the e2e test is using the image name I have on Linux but on Windows it is different" — and
// "recently we actually had runtime images working on windows. This is some kind of regression."
//
// It was not the product. The harness defaulted -runtime-image to "orchicon-runtime:orchicon-dev", the tag a
// LINUX dev host happens to build. The same variant on a Windows plane is catalogued as
// "orchicon-runtime:dev", so every runtime task there failed at image lookup — and, before failRunAtStart
// recorded its reason, with no error in the UI at all. Two symptoms, one literal.
//
// The variant is the IDENTITY; the tag is how a particular instance ADDRESSES it, and only the instance knows
// that. So the plane is asked.

import (
	"os"
	"strings"
	"testing"
)

// NO LITERAL DEFAULT — the flag must be empty so the plane is always consulted.
func TestTheHarnessHasNoHardcodedRuntimeImage(t *testing.T) {
	b, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read run.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, `fs.String("runtime-image"`)
	if i < 0 {
		t.Fatal("the -runtime-image flag is gone — re-establish this assertion or explain the replacement")
	}
	line := src[i:]
	if j := strings.Index(line, "\n"); j > 0 {
		line = line[:j]
	}
	if !strings.Contains(line, `""`) {
		t.Fatalf("-runtime-image still carries a default (%s). A default tag exists on ONE machine: "+
			"orchicon-runtime:orchicon-dev is what a Linux host builds, while a Windows plane catalogues the "+
			"same variant as orchicon-runtime:dev — so runtime tasks there fail at lookup. Leave it empty and "+
			"resolve from ListRuntimeImages.", strings.TrimSpace(line))
	}
}

// AND IT IS RESOLVED FROM THE PLANE, AFTER probe.
func TestTheHarnessResolvesTheImageFromThePlane(t *testing.T) {
	b, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read run.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "resolveRuntimeImage(ctx, cs,") {
		t.Fatal("run.go never calls resolveRuntimeImage, so the image would be empty or a literal")
	}
	// Order matters: the call needs a reachable plane, so it must come after probe.
	pi := strings.Index(src, "probe(ctx, cs)")
	ri := strings.Index(src, "resolveRuntimeImage(ctx, cs,")
	if pi < 0 || ri < 0 || ri < pi {
		t.Errorf("resolveRuntimeImage is called before probe (probe at %d, resolve at %d) — it needs a "+
			"reachable plane to list images from", pi, ri)
	}
}

// AN EXPLICIT IMAGE ALWAYS WINS: an operator testing a custom image must not be overridden by the harness.
func TestAnExplicitImageIsNotOverridden(t *testing.T) {
	b, err := os.ReadFile("support.go")
	if err != nil {
		t.Fatalf("read support.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "func resolveRuntimeImage(")
	if i < 0 {
		t.Fatal("resolveRuntimeImage is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "override") || !strings.Contains(body, "return override") {
		t.Error("resolveRuntimeImage does not honour an explicit override — an operator testing a custom " +
			"image would silently get the catalogue's instead")
	}
	// And the dev-variant match tolerates BOTH spellings, or it would miss one of the two hosts it exists for.
	if !strings.Contains(body, `strings.Contains(slug, "dev")`) {
		t.Error("the dev-variant match does not accept both :dev and :orchicon-dev, so one of the two hosts " +
			"this fix exists for would find nothing")
	}
}
