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
	"regexp"
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

// TestWailsJSONCarriesTheProjectTags — wails.json supplies the PROJECT's identity
// and release flavour, and must NOT supply the WebKitGTK ABI.
//
// THIS TEST WAS WRONG WHEN FIRST WRITTEN, and the correction is the point. It used
// to require `webkit2_41` here, on the reasoning that the shell's asset handler
// answered with a 307 and WebKitGTK 4.0's backend refuses any status but 200
// (`webkit2_legacy.go:40`). BOTH halves of that reasoning have since moved:
//
//  1. the handler no longer redirects at all — it serves a 200 bootstrap document
//     that navigates itself (internal/desktop BootstrapHTML), so the 4.0 backend is
//     perfectly able to answer it; and
//  2. the ABI is a property of the BUILD HOST, not of the project. Wails APPENDS
//     the tags from this file to any CLI tags (`compiledTags := append(projectTags,
//     userTags...)`), so it can add but never SUBTRACT — meaning a tag here applies
//     to EVERY `wails build`, including the .deb job on ubuntu-22.04, which has
//     libwebkit2gtk-4.0-dev and NOT 4.1. Requiring it here made that job
//     impossible to build while deploy/desktop/nfpm.yaml went on declaring a 4.0
//     dependency (see cmd/orchicon/webkit_abi_consistency_test.go).
//
// So: `orchicon_desktop` (the project's own tag — without it the build excludes
// every file) and `production` (the release flavour). The ABI is detected by
// `make desktop-build` and named explicitly by the packaging job.
func TestWailsJSONCarriesTheProjectTags(t *testing.T) {
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
	for _, want := range []string{"orchicon_desktop", "production"} {
		if !have[want] {
			t.Errorf("wails.json build:tags = %q is missing %q.\n"+
				"Every `wails build` inherits this file: without orchicon_desktop the build excludes "+
				"every file in cmd/orchicon-desktop (\"build constraints exclude all Go files\"), and "+
				"without production the shell ships the development flavour.",
				cfg.BuildTags, want)
		}
	}
	// THE ABI MUST NOT BE HERE — see this test's header. The pairing with the
	// package's declared dependency is asserted in webkit_abi_consistency_test.go.
	for _, tag := range []string{"webkit2_41", "webkit2_40", "webkit2_36"} {
		if have[tag] {
			t.Errorf("wails.json build:tags = %q pins the WebKitGTK ABI.\n"+
				"EVERY `wails build` inherits this file and Wails appends CLI tags, so it cannot "+
				"subtract one: the .deb job on ubuntu-22.04 (libwebkit2gtk-4.0-dev) would then demand "+
				"webkit2gtk-4.1's pkg-config file, which that image does not have. Detect the ABI in "+
				"`make desktop-build` and state it explicitly in the packaging job.", cfg.BuildTags)
		}
	}
}

// TestDesktopShipsOurIcon — the app must carry the ORCHICON mark, not Wails'.
//
// THE BUG THIS PINS. `cmd/orchicon-desktop/build/appicon.png` did not exist, so
// Wails fell back to its own default: a generic yellow square. The operator saw
// exactly that on the task bar and asked for our logo.
//
// WHY AN ASSERTION AND NOT JUST THE FILE. Two silent ways for it to disappear:
// `build/` is a gitignored BINARY directory, so a `.gitignore` edit (or a clean
// clone that never had the exception) drops the icon from the tree without
// anything failing; and Wails does not warn — it substitutes its default. The
// app then looks unbranded and no gate notices.
//
// PNG specifically: Wails generates the .ico/.icns and the window icon FROM this
// file, and it must be a raster image. A committed .svg here would be ignored by
// the CLI just as quietly.
func TestDesktopShipsOurIcon(t *testing.T) {
	path := filepath.Join("..", "orchicon-desktop", "build", "appicon.png")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nWithout this file Wails draws its DEFAULT icon (a yellow square), so the "+
			"desktop app ships unbranded. It is source, not build output: it lives under build/ because "+
			"that is where the Wails CLI looks, and .gitignore re-includes it by name.", path, err)
	}
	// PNG magic: \x89PNG\r\n\x1a\n
	if len(b) < 8 || string(b[1:4]) != "PNG" {
		t.Fatalf("%s is not a PNG (first bytes %q); Wails needs a raster image it can resample", path, b[:minInt(8, len(b))])
	}
	// The CLI resamples down to window/taskbar sizes, so a small source loses.
	// 1024 is the size the icon was rasterised at, and the size Wails documents.
	if w, h := pngSize(t, b); w < 256 || h < 256 {
		t.Errorf("%s is %dx%d; a source icon below 256px cannot be resampled up for the taskbar "+
			"and installer images", path, w, h)
	}
}

// pngSize reads the IHDR width/height without pulling in an image decoder for a
// test that only needs two integers.
func pngSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	// 8-byte signature, then a 4-byte length + "IHDR", then width and height.
	if len(b) < 24 {
		t.Fatalf("truncated PNG (%d bytes)", len(b))
	}
	be := func(p []byte) int { return int(p[0])<<24 | int(p[1])<<16 | int(p[2])<<8 | int(p[3]) }
	return be(b[16:20]), be(b[20:24])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestDesktopWindowIconIsSet — the RUNNING window must carry our icon, not the
// desktop environment's fallback.
//
// THE BUG THIS PINS. build/appicon.png fed only the PACKAGER (the .ico, the .icns,
// the .deb/.AppImage art). On Linux the window icon is options.Linux.Icon — a
// []byte the app must supply — and we passed no Linux options at all, so the
// window had none and the task bar drew a generic square. The operator saw the
// Wails-yellow default while a packaged build would have shown our mark.
//
// Asserted at the source for the same reason the tag tests are: nothing else in
// the gate can see it. The app compiles, the shell guards pass, `wails build`
// packages a correctly-branded .deb — and the running window is still icon-less.
func TestDesktopWindowIconIsSet(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "orchicon-desktop", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/orchicon-desktop/main.go: %v", err)
	}
	src := string(b)

	// The icon must be EMBEDDED, so `go build` (make desktop-build) has it too —
	// a path read at runtime would break the moment the binary moves.
	if !strings.Contains(src, "//go:embed build/appicon.png") {
		t.Error("main.go does not embed build/appicon.png; `make desktop-build` is a plain `go build`, " +
			"which never runs the packager, so the window would have no icon")
	}
	// ...and handed to Wails as the Linux window icon.
	if !strings.Contains(src, "Linux:") {
		t.Fatal("main.go passes no options.Linux. On Linux the window icon comes from " +
			"options.Linux.Icon alone; without it the desktop environment draws its own fallback.")
	}
	if !strings.Contains(src, "Icon:") || !strings.Contains(src, "appIcon") {
		t.Error("options.Linux sets no Icon from the embedded appIcon")
	}
	// Setting options.Linux non-nil CHANGES the WebviewGpuPolicy default (Wails
	// documents nil => Never, wails#2977). Leaving it unstated would silently turn
	// hardware acceleration back on.
	if !strings.Contains(src, "WebviewGpuPolicy") {
		t.Error("options.Linux does not state WebviewGpuPolicy. Passing a non-nil options.Linux " +
			"changes the default away from WebviewGpuPolicyNever (wails#2977), so adding an icon " +
			"would also change how the app renders.")
	}
}

// TestDevRebuildRefreshesThePlaneBinary — `make rebuild-dev` must repoint the
// `orchicon` a THIN CLIENT will run.
//
// THE BUG THIS PINS, and it is the most expensive one in this batch because it
// made a successful rebuild look like a bad build:
//
//	$ make rebuild-dev          # builds bin/orchicon, restarts dev
//	$ orchicon --version        # canary, built six days earlier
//	web app:  "vCanary", the OLD telemetry page, Grafana still in the nav
//	desktop :  v0.5.105
//	Reports :  "Could not load reports [unimplemented] HTTP 404"
//
// THREE symptoms, ONE cause. internal/planecontrol's resolvePlaneBinary runs the
// plane from $PATH whenever the caller is not itself `orchicon` — the TUI and the
// desktop app are exactly that shape ("the thin-client case, and it is the normal
// one for two of the three surfaces"). So "start dev" from Manage Server launched
// the stale regular file in ~/.local/bin, which nothing refreshed: this target
// symlinked only `orch`. The instance then ran a six-day-old binary whose embedded
// SPA predates the native telemetry page and which has no GetTelemetryReport at
// all — hence `unimplemented`.
func TestDevRebuildRefreshesThePlaneBinary(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	src := string(b)

	// Isolate the dev launcher target: the prod one deliberately keeps a snapshot.
	start := strings.Index(src, "orch-launcher-dev:")
	if start < 0 {
		t.Fatal("Makefile has no orch-launcher-dev target")
	}
	end := strings.Index(src[start:], "\n# orch-launcher-prod")
	if end < 0 {
		t.Fatalf("cannot find the end of orch-launcher-dev")
	}
	body := src[start : start+end]

	// THE MECHANISM MOVED, THE PROPERTY DID NOT.
	//
	// This used to assert the literal `ln -sfn "$(BIN_DIR)/orchicon" "$(LAUNCHER_DIR)/orchicon"` inside the
	// target. That direct symlink is exactly what repointed the operator's launchers into a THROWAWAY
	// WORKTREE when a rebuild ran from one — so deleting the worktree deleted them ("my orch binary has been
	// deleted"). The linking now goes through scripts/dev/install-launcher.sh, which decides WHICH CHECKOUT a
	// launcher may point at.
	//
	// So the assertion splits in two, over the two places the property now lives:
	//   1. the TARGET must still refresh orchicon (the stale-plane bug this guard was written for);
	//   2. the SCRIPT must still create a SYMLINK to a repo build rather than a copy (a copy goes stale).
	if !strings.Contains(body, "install-launcher.sh") || !strings.Contains(body, "orchicon") {
		t.Errorf("orch-launcher-dev does not refresh the orchicon launcher.\n"+
			"A dev rebuild then leaves the plane binary a THIN CLIENT resolves from $PATH untouched, so\n"+
			"`make rebuild-dev` reports success while dev keeps serving a stale binary: old embedded SPA,\n"+
			"a version that disagrees with the desktop app, and RPCs it does not implement.\n"+
			"target body:\n%s", body)
	}

	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "dev", "install-launcher.sh"))
	if err != nil {
		t.Fatalf("read install-launcher.sh: %v", err)
	}
	scriptSrc := string(script)
	if !strings.Contains(scriptSrc, "ln -sfn") {
		t.Error("install-launcher.sh should SYMLINK the launcher at the repo build (like `orch`), not copy " +
			"it — a copy becomes stale the moment the next build lands")
	}
	if !strings.Contains(scriptSrc, `.orchicon-worktrees`) {
		t.Error("install-launcher.sh does not refuse a worktree as the link target. That is the defect " +
			"that deleted the operator's launchers when a worktree was removed.")
	}
}

// TestDesktopEntryMatchesTheAppID — the .desktop must key off the SAME app_id the
// app sets, and must name an icon.
//
// THE BUG THIS PINS, on the operator's Wayland session: the taskbar showed the
// generic (Wails) icon while the binary embedded our mark correctly. Wails sets
// the window icon with gtk_window_set_icon, which publishes the X11
// _NET_WM_ICON property — and a Wayland compositor IGNORES it. KDE resolves a
// task's icon as app_id -> .desktop -> Icon=, so with no .desktop installed the
// entry falls back regardless of what the binary carries. Launching from a
// terminal makes it more visible, not more true: the association is the same.
//
// The app_id is options.Linux.ProgramName. If these two ever disagree the icon
// silently stops resolving, which is why it is asserted rather than documented.
func TestDesktopEntryMatchesTheAppID(t *testing.T) {
	mainSrc, err := os.ReadFile(filepath.Join("..", "orchicon-desktop", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	m := regexp.MustCompile(`ProgramName:\s*"([^"]+)"`).FindStringSubmatch(string(mainSrc))
	if m == nil {
		t.Fatal("main.go sets no options.Linux.ProgramName — without it the app_id is the executable " +
			"name, which will not match the .desktop after any rename")
	}
	appID := m[1]

	entry, err := os.ReadFile(filepath.Join("..", "..", "deploy", "desktop", "linux", "orchicon-desktop.desktop"))
	if err != nil {
		t.Fatalf("read the .desktop: %v", err)
	}
	entrySrc := string(entry)

	if !strings.Contains(entrySrc, "StartupWMClass="+appID) {
		t.Errorf(".desktop does not declare StartupWMClass=%s (options.Linux.ProgramName is %q).\n"+
			"On Wayland the taskbar icon resolves app_id -> .desktop -> Icon=; a mismatched class "+
			"leaves the entry generic however correct the embedded icon is.", appID, appID)
	}
	if !strings.Contains(entrySrc, "Icon="+appID) {
		t.Errorf(".desktop does not declare Icon=%s, matching the icon file name `desktop-install` "+
			"writes into hicolor (%s.png)", appID, appID)
	}
}

// TestDesktopInstallInstallsAnIconAndMenuEntry — the DEV path must install what
// the package installs, or the icon works only for .deb users.
func TestDesktopInstallInstallsAnIconAndMenuEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	src := string(b)

	start := strings.Index(src, "desktop-install:")
	if start < 0 {
		t.Fatal("Makefile has no desktop-install target — a dev checkout then has no .desktop, so on " +
			"Wayland the taskbar cannot resolve the icon at all")
	}
	end := strings.Index(src[start:], "\ndesktop-test:")
	if end < 0 {
		t.Fatalf("cannot find the end of desktop-install")
	}
	body := src[start : start+end]

	for _, want := range []struct{ needle, why string }{
		// KEYED ON THE COMMAND, NOT ON THE PATH: the target's own comments and its
		// closing echo both mention these paths, so a path-only assertion still
		// passes after the copy is deleted — a guard that cannot fail is worse than
		// none, because it reports coverage it does not have.
		{"cp -f $(DESKTOP_PROJECT)/build/appicon.png", "it must COPY our appicon.png into the hicolor theme"},
		{"hicolor/512x512/apps/orchicon-desktop.png", "the icon must land under the name Icon= expects"},
		{"applications/orchicon-desktop.desktop", "the entry must land in the user's applications dir"},
		// The shipped entry says bare `orchicon-desktop`, which is not on PATH for a
		// dev build — so the target must REWRITE Exec, and this is the only line
		// that does it (the echoes mention the path but never the sed script).
		{"s|^Exec=", "it must rewrite Exec to this checkout's binary"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("desktop-install does not mention %q: %s", want.needle, want.why)
		}
	}
}
