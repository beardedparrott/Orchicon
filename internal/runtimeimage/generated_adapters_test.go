package runtimeimage

// generated_adapters_test.go — A GENERATED IMAGE MUST BE ABLE TO RUN WORK, AND IN THE RIGHT ORDER.
//
// Reported as a design gap: an operator creating a runtime image in the console got a toolchain image with
// no adapter in it, sent work to it, and discovered the hole only when the first session could not spawn.
// Nobody creating "a runtime image" is asking for one that cannot run any work.

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

// TestGeneratedAdapterOrderIsBuildToolsThenAdapters pins the ORDER, which an operator asked for explicitly
// and which is load-bearing: a toolchain line that replaces or removes node/npm would silently take adapters
// installed BEFORE it, and the damage would surface at the first session spawn rather than here.
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

// TestADockerfileOverrideIsLeftAlone: an operator who wrote their own Dockerfile has taken the decision, and
// silently appending adapters to it would be second-guessing them.
func TestADockerfileOverrideIsLeftAlone(t *testing.T) {
	const mine = "FROM base:latest\nRUN echo hello\n"
	if got := generatedDockerfile(db.RuntimeImageRow{DockerfileOverride: mine, IncludeAdapters: true}, "base:latest"); got != mine {
		t.Errorf("an override was modified:\n%q", got)
	}
}
