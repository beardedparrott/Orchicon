package main

// desktop_tag_test.go — OUR BUILD TAG MUST NOT BE ONE WAILS RESERVES.
//
// THE BUG THIS PINS, reproduced from the operator's own terminal:
//
//	$ wails build
//	• Generating bindings:  ERROR
//	    package github.com/beardedparrott/orchicon/cmd/orchicon-desktop:
//	    build constraints exclude all Go files in .../cmd/orchicon-desktop
//	    exit status 1
//
// The Wails entry point is `//go:build desktop`, and **`desktop` is a tag WAILS RESERVES**. Its bindings
// generator strips a fixed set before walking the project:
//
//	// cmd/wails/pkg/commands/bindings/bindings.go
//	tags := append(options.Tags, "bindings")
//	genModuleTags := lo.Without(tags, "desktop", "production", "debug", "dev")
//
// So the generator compiled the package WITHOUT our tag, found no Go files at all, and failed. NO FLAG CAN
// FIX IT: `-tags desktop` is stripped too, because the collision is on the NAME. It only ever worked in the
// one CI job that ran a raw `go build -tags desktop,...` — which never invokes the bindings step at all,
// and never runs `wails build` (the desktop job says so deliberately, calling the CLI "a packaging tool").
//
// The rename to `orchicon_desktop` is what fixes it, and the reason it must be ASSERTED rather than merely
// fixed is that the failure is silent in every other gate: the tag compiles, the shell's guards pass, the
// release path builds — and only `wails build`, which the operator runs by hand, fails. A reserved name is
// an invisible collision with a third-party tool's internals, so it is checked here.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wailsReservedTags is the set Wails REMOVES before generating bindings (bindings.go). A package gated on
// any of these is invisible to the bindings generator, so `wails build` fails with "build constraints
// exclude all Go files" and no flag can help.
var wailsReservedTags = []string{"desktop", "production", "debug", "dev", "bindings"}

// desktopEntryTag reads the build tag on the Wails entry point.
func desktopEntryTag(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "orchicon-desktop", "main.go"))
	if err != nil {
		t.Fatalf("read the desktop entry point: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//go:build") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "//go:build"))
	}
	t.Fatal("cmd/orchicon-desktop/main.go has NO //go:build line — if the tag is gone entirely, the file " +
		"compiles into every ordinary build, which is not what this app wants")
	return ""
}

// THE PIN: the entry point's tag is not one Wails reserves.
func TestDesktopBuildTagIsNotReservedByWails(t *testing.T) {
	tag := desktopEntryTag(t)
	// A build expression may be compound ("a && b"); every term is checked, because a reserved name
	// anywhere in the expression still removes the file from the bindings walk.
	for _, term := range strings.FieldsFunc(tag, func(r rune) bool {
		return r == '&' || r == '|' || r == '!' || r == ' ' || r == '(' || r == ')'
	}) {
		for _, reserved := range wailsReservedTags {
			if term == reserved {
				t.Errorf("the desktop entry point is gated on %q, which WAILS RESERVES.\n"+
					"  Wails strips %v before generating bindings (bindings.go:46), so the generator sees "+
					"NO Go files in this package and `wails build` fails with \"build constraints exclude "+
					"all Go files\".\n"+
					"  No flag can work around it — `-tags %s` is stripped too, because the collision is on "+
					"the NAME. Rename the tag (e.g. orchicon_desktop) in main.go, wails.json's build:tags, "+
					"the Makefile's desktop-build, and both workflows.", term, wailsReservedTags, reserved)
			}
		}
	}
}

// AND wails.json AGREES WITH main.go. `wails build` with no flags takes its tags from `build:tags`, which
// is what the operator runs; if the two disagree, the documented local build fails while CI stays green —
// which is precisely the shape of the bug above.
func TestWailsJSONTagsMatchTheEntryPointTag(t *testing.T) {
	tag := desktopEntryTag(t)
	// The project's own tag is the one that is not a Wails internal or a platform selector.
	own := ""
	for _, term := range strings.FieldsFunc(tag, func(r rune) bool {
		return r == '&' || r == '|' || r == '!' || r == ' ' || r == '(' || r == ')'
	}) {
		if strings.HasPrefix(term, "orchicon") {
			own = term
		}
	}
	if own == "" {
		t.Fatalf("the entry point's build tag %q carries no project-specific term (expected something "+
			"like orchicon_desktop) — a tag that is ONLY a Wails internal or a platform selector cannot "+
			"be set from wails.json without colliding with Wails' own names", tag)
	}

	b, err := os.ReadFile(filepath.Join("..", "orchicon-desktop", "wails.json"))
	if err != nil {
		t.Fatalf("read wails.json: %v", err)
	}
	var cfg struct {
		BuildTags string `json:"build:tags"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse wails.json: %v", err)
	}
	if cfg.BuildTags == "" {
		t.Fatalf("wails.json sets no `build:tags`, so a bare `wails build` — the command the USER runs — " +
			"compiles without the project's tag and fails with \"build constraints exclude all Go files\". " +
			"CI's raw `go build -tags ...` cannot catch this, because it never runs the bindings step")
	}
	if !strings.Contains(cfg.BuildTags, own) {
		t.Errorf("wails.json build:tags = %q, which does not include the entry point's own tag %q — a bare "+
			"`wails build` would then compile without it", cfg.BuildTags, own)
	}
}

// TestWailsJSONCarriesTheReleaseTags — wails.json must supply the SAME tags the
// release builds with, not merely the project's own.
//
// THE BUG THIS PINS, from the operator's own terminal:
//
//	unable to finish request: StatusCodes not supported: 307 - Temporary Redirect
//
// The shell's asset handler answers every request with a 307 to its own origin,
// which is a legitimate design and works — but ONLY on Wails' modern asset
// backend. Wails selects the WebKitGTK ABI by BUILD TAG, and the legacy 4.0
// backend refuses any status but 200 (webkit2_legacy.go:40 `if code !=
// http.StatusOK` → "StatusCodes not supported"). With `build:tags` naming only
// `orchicon_desktop`, a bare `wails build` produced a binary linked against
// libwebkit2gtk-4.0 — verified with ldd on the artifact — so the shell loaded
// and then could not answer a single request.
//
// The three PACKAGING jobs (desktop-linux/.deb + AppImage, desktop-macos,
// desktop-windows) pass no `-tags` of their own — they inherit this file. So a
// wails.json missing `webkit2_41` does not merely break the developer's local
// build: it ships installers with the same defect. Wails MERGES these with any
// CLI tags (cmd/wails/build.go: `compiledTags := append(projectTags, userTags...)`),
// so stating them here is safe alongside a job that also passes them.
func TestWailsJSONCarriesTheReleaseTags(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "orchicon-desktop", "wails.json"))
	if err != nil {
		t.Fatalf("read wails.json: %v", err)
	}
	var cfg struct {
		BuildTags string `json:"build:tags"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse wails.json: %v", err)
	}
	have := map[string]bool{}
	for _, tag := range strings.Split(cfg.BuildTags, ",") {
		have[strings.TrimSpace(tag)] = true
	}
	// production: the release flavour (no dev tooling / dev server path).
	// webkit2_41:  the 4.1 ABI, whose asset backend can return a redirect.
	for _, want := range []string{"orchicon_desktop", "production", "webkit2_41"} {
		if !have[want] {
			t.Errorf("wails.json build:tags = %q is missing %q.\n"+
				"Every `wails build` inherits this file — including the three packaging jobs that pass no\n"+
				"-tags of their own — so a missing %q either fails the build outright (a missing\n"+
				"orchicon_desktop) or ships installers with the legacy WebKitGTK 4.0 backend, which\n"+
				"cannot answer the shell's 307 (\"StatusCodes not supported: 307\").",
				cfg.BuildTags, want, want)
		}
	}
}
