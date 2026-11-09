package runtimeimage

// generated_adapters_test.go — A GENERATED IMAGE MUST BE ABLE TO RUN WORK, AND IN THE RIGHT ORDER.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestGeneratedImageIncludesAdaptersByDefault is the reported gap, stated directly.
func TestGeneratedImageIncludesAdaptersByDefault(t *testing.T) {
	df := generatedDockerfile(db.RuntimeImageRow{
		AptPackages:     []byte(`[]`),
		Toolchains:      []byte(`[]`),
		Env:             []byte(`{}`),
		IncludeAdapters: true,
	}, "base:latest")

	for _, want := range []string{"@opencode/cli", "@anthropic-ai/claude-code"} {
		if !strings.Contains(df, want) {
			t.Errorf("a generated image omits %q — a session dispatched to it could not start\n%s", want, df)
		}
	}
	if !strings.Contains(df, "USER root") || !strings.Contains(df, "USER orchicon") {
		t.Errorf("the generated Dockerfile must drop to root to install and return to the runtime user:\n%s", df)
	}
}

// TestGeneratedImageAdaptersCanBeSwitchedOff keeps the escape hatch real: a shell-only toolchain or a build
// image is a supported shape, and the field is persisted so it is a decision rather than an omission.
func TestGeneratedImageAdaptersCanBeSwitchedOff(t *testing.T) {
	df := generatedDockerfile(db.RuntimeImageRow{
		AptPackages: []byte(`[]`), Toolchains: []byte(`[]`), Env: []byte(`{}`),
		IncludeAdapters: false,
	}, "base:latest")
	if strings.Contains(df, "@opencode/cli") || strings.Contains(df, "@anthropic-ai/claude-code") {
		t.Errorf("IncludeAdapters=false must generate no adapter install:\n%s", df)
	}
}

// TestGeneratedAdapterOrderIsBuildToolsThenAdapters pins the ORDER, which an operator asked for
// explicitly and which is load-bearing.
func TestGeneratedAdapterOrderIsBuildToolsThenAdapters(t *testing.T) {
	df := generatedDockerfile(db.RuntimeImageRow{
		AptPackages:     []byte(`["jq"]`),
		Toolchains:      []byte(`["npm install -g typescript"]`),
		Env:             []byte(`{"FOO":"bar"}`),
		IncludeAdapters: true,
	}, "base:latest")

	aptAt := strings.Index(df, "apt-get install")
	toolAt := strings.Index(df, "typescript")
	adaptAt := strings.Index(df, "@opencode/cli")
	if aptAt < 0 || toolAt < 0 || adaptAt < 0 {
		t.Fatalf("one of the three sections is missing (apt=%d tool=%d adapters=%d):\n%s", aptAt, toolAt, adaptAt, df)
	}
	if !(aptAt < toolAt && toolAt < adaptAt) {
		t.Fatalf("WRONG ORDER (apt=%d tool=%d adapters=%d) — apt packages, then the row's toolchains, then "+
			"the adapters:\n%s", aptAt, toolAt, adaptAt, df)
	}
}

// AN OVERRIDE USED TO RETURN VERBATIM, WHICH MADE include_adapters INERT.
func TestAnOverrideWithAdaptersOnGetsThemInstalled(t *testing.T) {
	const mine = "FROM base:latest\nRUN apt-get install -y jq\nUSER orchicon\nWORKDIR /home/orchicon\n"
	got := generatedDockerfile(db.RuntimeImageRow{DockerfileOverride: mine, IncludeAdapters: true}, "base:latest")

	for _, want := range []string{"@opencode/cli", "@anthropic-ai/claude-code"} {
		if !strings.Contains(got, want) {
			t.Fatalf("an override with include_adapters=true produced an image with no %q — the switch is inert "+
				"again:\n%s", want, got)
		}
	}
	// The install needs root, and the operator's own final USER must survive it.
	npAt := strings.Index(got, "npm install -g")
	rootAt := strings.LastIndex(got[:npAt], "USER root")
	if rootAt < 0 {
		t.Errorf("the adapter install does not run as root, so npm -g would fail against the operator's "+
			"runtime user:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "USER orchicon") {
		t.Errorf("the operator's final USER was not restored, so the image no longer runs as they intend:\n%s", got)
	}
}

// THE ESCAPE HATCH STAYS REAL: an override with adapters switched OFF is still byte-identical.
func TestAnOverrideWithoutAdaptersIsUntouched(t *testing.T) {
	const mine = "FROM base:latest\nRUN echo hello\n"
	if got := generatedDockerfile(db.RuntimeImageRow{DockerfileOverride: mine, IncludeAdapters: false}, "base:latest"); got != mine {
		t.Errorf("an override with include_adapters=false was modified:\n%q", got)
	}
}

// AN OVERRIDE THAT ALREADY INSTALLS THEM IS NOT DOUBLED — this is the shipped :dev / :gui shape, and a
// second npm -g is dead weight in every rebuild.
func TestAnOverrideThatAlreadyInstallsTheAdaptersIsNotDuplicated(t *testing.T) {
	const mine = "FROM base:latest\nRUN npm install -g \"@anthropic-ai/claude-code@latest\"\nUSER orchicon\n"
	got := generatedDockerfile(db.RuntimeImageRow{DockerfileOverride: mine, IncludeAdapters: true}, "base:latest")
	if n := strings.Count(got, "@anthropic-ai/claude-code"); n != 1 {
		t.Errorf("the adapter install appears %d times, want exactly 1:\n%s", n, got)
	}
}
