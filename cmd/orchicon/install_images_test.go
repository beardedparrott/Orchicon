package main

import (
	"strings"
	"testing"
)

// TestRuntimeVariantRefMatchesReleaseWorkflow used to be the cross-file agreement gate between the
// installer's variant refs and what release.yml publishes.
func TestRuntimeVariantRefIsAWellFormedRegistryRef(t *testing.T) {
	for _, suffix := range []string{"gui", "dev"} {
		got := runtimeVariantRef(suffix, "latest")
		want := "ghcr.io/beardedparrott/orchicon-runtime:" + suffix
		if got != want {
			t.Errorf("runtimeVariantRef(%q, \"latest\") = %q, want %q (the floating tag; NOT %q)",
				suffix, got, want, want+"-latest")
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

// TestRuntimeVariantRefNeverBuildsLatestSuffix is the direct regression for the shipped bug: no tag
// the installer builds may end in "-latest", because the registry has no such variant.
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
