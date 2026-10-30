package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestParseRuntimeImageSelection pins the install's multi-select contract:
// default NONE, "all", explicit indexes, and the base auto-include that keeps
// a variant from being fetched without the image it derives FROM.
func TestParseRuntimeImageSelection(t *testing.T) {
	cases := []struct {
		in      string
		n       int
		want    []int
		wantErr bool
	}{
		{"", 4, nil, false},
		{"none", 4, nil, false},
		{"all", 4, []int{0, 1, 2, 3}, false},
		{"1,3", 4, []int{0, 2}, false},
		{"1 3", 4, []int{0, 2}, false},
		{"2", 4, []int{0, 1}, false}, // gui alone pulls the base with it
		{"1", 4, []int{0}, false},
		{"0", 4, nil, true},
		{"9", 4, nil, true},
		{"abc", 4, nil, true},
	}
	for _, c := range cases {
		got, err := parseRuntimeImageSelection(c.in, c.n)
		if c.wantErr {
			if err == nil {
				t.Errorf("parse(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parse(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parse(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestInstallOffersEveryRuntimeImageAndDeclinesByDefault is AC 15/17: every
// shipped runtime image is offered with its size, and an install that answers
// nothing (a non-interactive install, EOF) downloads NOTHING.
func TestInstallOffersEveryRuntimeImageAndDeclinesByDefault(t *testing.T) {
	choices := installRuntimeImageChoices("latest")
	// web-research is deliberately ABSENT: its tag is not published (see the comment on
	// installRuntimeImageChoices), so offering it promised a download that could only fail.
	want := []string{"base", "gui", "dev"}
	if len(choices) != len(want) {
		t.Fatalf("install offers %d runtime images, want %d", len(choices), len(want))
	}
	for i, v := range want {
		if choices[i].Variant != v {
			t.Errorf("choice %d = %q, want %q", i, choices[i].Variant, v)
		}
		if choices[i].SizeBytes <= 0 {
			t.Errorf("choice %q shows no size — the choice would not be informed", choices[i].Variant)
		}
		if !strings.Contains(choices[i].Ref, "orchicon-runtime") || !strings.HasSuffix(choices[i].Ref, ":"+variantTag(v)) {
			t.Errorf("choice %q ref %q does not match the published tag", choices[i].Variant, choices[i].Ref)
		}
	}

	var shown strings.Builder
	got, err := resolveInstallRuntimeImages(&shown, strings.NewReader(""), "latest")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty answer downloaded %d images, want none", len(got))
	}
	for _, v := range want {
		if !strings.Contains(shown.String(), v) {
			t.Errorf("the prompt does not mention the %q variant:\n%s", v, shown.String())
		}
	}
	if !strings.Contains(shown.String(), "[none]") {
		t.Errorf("the prompt does not show that declining is the default:\n%s", shown.String())
	}
}

func variantTag(v string) string {
	if v == "base" {
		return "latest"
	}
	return v
}

// TestInstallImageRefsAgreeWithThePublishPolicy is the installer's half of the publish/build split.
//
// It used to assert that release.yml publishes the gui and dev variant tags, because the install PULLED them.
// It does not any more: those images carry third-party adapter CLIs, so release.yml publishes the base alone
// and the installer BUILDS the variants from the template embedded in the binary.
//
// So the assertion inverts. What must hold now is that each choice is OBTAINABLE by the route it names:
// a published choice is a registry ref that release.yml actually pushes, and an unpublished one is a LOCAL
// name with a build context — never a registry ref nothing publishes, which is the "manifest unknown" failure
// an operator reported.
func TestInstallImageRefsAgreeWithThePublishPolicy(t *testing.T) {
	src, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	wf := string(src)

	choices := installRuntimeImageChoices("latest")
	if len(choices) == 0 {
		t.Fatal("no install image choices")
	}
	// The base must be first: every variant derives FROM it, and the prompt says so.
	if choices[0].Variant != "base" || !choices[0].Published {
		t.Fatalf("the first choice must be the published base, got %+v", choices[0])
	}
	for _, c := range choices {
		isRegistry := strings.Contains(c.Ref[:strings.LastIndex(c.Ref, ":")], "/")
		switch {
		case c.Published && !isRegistry:
			t.Errorf("%s is marked published but %q is a LOCAL name — it would be built, and the published "+
				"image never used", c.Variant, c.Ref)
		case !c.Published && isRegistry:
			t.Errorf("%s is marked unpublished but %q is a REGISTRY ref — the fetch would pull something "+
				"nothing publishes", c.Variant, c.Ref)
		case !c.Published && c.BuildContext == "":
			t.Errorf("%s is built locally but names no build context", c.Variant)
		case c.Published:
			tag := c.Ref[strings.LastIndex(c.Ref, ":")+1:]
			if !strings.Contains(wf, "orchicon-runtime:"+tag) {
				t.Errorf("%s is published at %q, but release.yml never pushes that tag", c.Variant, tag)
			}
		}
	}
}

// TestAssumeYesAnswersTheRuntimeImagePrompt is AC 19 for the affirmative half: `--yes` reaches this prompt
// through ORCHICON_ASSUME_YES (scripts/install.sh exports it, because the setup step runs `orchicon install`
// with no flags), answers ALL of them, prints the total size BEFORE anything is fetched, and never overrides
// an answer the instance already recorded.
func TestAssumeYesAnswersTheRuntimeImagePrompt(t *testing.T) {
	t.Setenv("ORCHICON_ASSUME_YES", "1")
	var shown strings.Builder
	got, answered := assumeYesRuntimeImages(&shown, "latest")
	if !answered || len(got) != len(installRuntimeImageChoices("latest")) {
		t.Fatalf("--yes answered %v (answered=%v), want every choice", got, answered)
	}
	if !strings.Contains(shown.String(), "~") || !strings.Contains(shown.String(), "--yes") {
		t.Errorf("the affirmative answer did not name the total size before fetching:\n%s", shown.String())
	}

	// Unset: no opinion, and no output claiming one.
	t.Setenv("ORCHICON_ASSUME_YES", "")
	var quiet strings.Builder
	if _, answered := assumeYesRuntimeImages(&quiet, "latest"); answered {
		t.Errorf("an unset ORCHICON_ASSUME_YES answered the prompt: %q", quiet.String())
	}

	// A RECORDED ANSWER WINS: --yes must not turn every rebuild of a declined instance into a multi-GB
	// download, because the record (not the flag) is what this instance last chose.
	t.Setenv("ORCHICON_ASSUME_YES", "1")
	t.Setenv("ORCHICON_INSTALL_INTERACTIVE", "1")
	t.Setenv("HOME", t.TempDir())
	recordRuntimeImagesAnswer("dev", "none")
	got, err := resolveInstallRuntimeImagesFor("dev", io.Discard, strings.NewReader("all"), "latest", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("--yes overrode the recorded \"none\": got %d image(s), want none", len(got))
	}
}
