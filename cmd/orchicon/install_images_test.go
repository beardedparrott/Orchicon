package main

import (
	"os"
	"strings"
	"testing"
)

// TestRuntimeVariantRefMatchesReleaseWorkflow is the cross-file agreement gate
// for the runtime image variants.
//
// This is the bug that broke `orchicon install` on every fresh host: the refs
// were built as "<suffix>-<tag>", so the DEFAULT tag "latest" produced
// ":gui-latest" — a tag release.yml never publishes — and the installer aborted
// at the image pull before creating anything. The registry publishes
// "<suffix>-<version>" plus a FLOATING ":gui"/":dev".
//
// release.yml is parsed rather than trusted, so a change to the published tag
// convention fails here instead of on a new user's machine.
func TestRuntimeVariantRefMatchesReleaseWorkflow(t *testing.T) {
	src, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	wf := string(src)

	// The publisher must still create the pinned AND the floating tag for each
	// variant; without both, one of the installer's two shapes is wrong.
	for _, want := range []string{
		"ghcr.io/beardedparrott/orchicon-runtime:gui-${ORCHICON_VERSION}",
		"ghcr.io/beardedparrott/orchicon-runtime:dev-${ORCHICON_VERSION}",
		"docker tag \"ghcr.io/beardedparrott/orchicon-runtime:gui-${ORCHICON_VERSION}\" ghcr.io/beardedparrott/orchicon-runtime:gui",
		"docker tag \"ghcr.io/beardedparrott/orchicon-runtime:dev-${ORCHICON_VERSION}\" ghcr.io/beardedparrott/orchicon-runtime:dev",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml no longer publishes %q — the installer's variant refs would drift from the registry", want)
		}
	}

	// The floating ref the installer builds for the default tag.
	for _, suffix := range []string{"gui", "dev"} {
		got := runtimeVariantRef(suffix, "latest")
		want := "ghcr.io/beardedparrott/orchicon-runtime:" + suffix
		if got != want {
			t.Errorf("runtimeVariantRef(%q, \"latest\") = %q, want %q (the floating tag; NOT %q)", suffix, got, want, want+"-latest")
		}
		// An empty tag must behave as the default — an operator who sets
		// ORCHICON_IMAGE_TAG="" must not get a dangling ref.
		if got := runtimeVariantRef(suffix, ""); got != want {
			t.Errorf("runtimeVariantRef(%q, \"\") = %q, want %q", suffix, got, want)
		}
	}

	// A PINNED tag uses the version-suffixed ref, which is what the workflow
	// publishes for a specific release.
	for _, tc := range []struct{ suffix, tag, want string }{
		{"gui", "v0.4.5", "ghcr.io/beardedparrott/orchicon-runtime:gui-v0.4.5"},
		{"dev", "v0.4.5", "ghcr.io/beardedparrott/orchicon-runtime:dev-v0.4.5"},
	} {
		if got := runtimeVariantRef(tc.suffix, tc.tag); got != tc.want {
			t.Errorf("runtimeVariantRef(%q, %q) = %q, want %q", tc.suffix, tc.tag, got, tc.want)
		}
	}
}

// TestRuntimeVariantRefNeverBuildsLatestSuffix is the direct regression for the
// shipped bug: no tag the installer builds may end in "-latest", because the
// registry has no such variant. Stated as its own assertion because it is the
// exact string a fresh install died on.
func TestRuntimeVariantRefNeverBuildsLatestSuffix(t *testing.T) {
	for _, tag := range []string{"", "latest"} {
		for _, suffix := range []string{"gui", "dev"} {
			got := runtimeVariantRef(suffix, tag)
			if strings.HasSuffix(got, "-latest") {
				t.Errorf("runtimeVariantRef(%q, %q) = %q — that tag is never published", suffix, tag, got)
			}
		}
	}
}
