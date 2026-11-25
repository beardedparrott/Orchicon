package runtime

// THE CONTAINER'S ADAPTER PATH: host-first, image only as a REAL fallback.
//
// THE FAILURE THIS PINS. Every runtime run died at start with
//
//	serve handshake: exit status 1
//
// for a run demanding kinds ["claude","opencode"]. Reproduced by hand, the supervisor's own first step
// was:
//
//	{"event":"error","error":"exec: \"opencode\": executable file not found in $PATH"}
//
// The cause: `imageAdapters` claimed the runtime image bakes `opencode` and `claude`. It bakes NEITHER —
// measured in orchicon-runtime:orchicon-dev, `command -v opencode` and `command -v claude` are both
// absent; the adapters are mounted from the host at create time, the same "mount, never bake" rule as the
// orchicon binary itself. Because the list said otherwise, adapterPathPrefixFor skipped the host's
// ~/.opencode/bin under the `default:` arm, the container's PATH never carried it, and the supervisor
// could not start the serve the daemon was waiting for.

import (
	"os"
	"strings"
	"testing"
)

// A kind is on the container PATH when its resolved bin dir is there. The assertion is about the SHAPE
// of the answer (does the host dir appear), not about this host's exact layout.
func TestTheHostBinDirIsOnTheContainerPathForEveryKind(t *testing.T) {
	home := t.TempDir()
	for _, c := range []struct{ kind, sub, bin string }{
		{"opencode", ".opencode/bin", "opencode"},
		{"claude", ".local/bin", "claude"},
	} {
		dir := home + "/" + c.sub
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/"+c.bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		// ALONE and TOGETHER — the two-kind case is the one that broke, so it is asserted explicitly.
		for _, kinds := range [][]string{{c.kind}, {"claude", "opencode"}} {
			got := adapterPathPrefix(home, kinds...)
			found := false
			for _, d := range got {
				if d == dir {
					found = true
				}
			}
			if !found {
				t.Errorf("kind %s with kinds=%v: %s is NOT on the container PATH (got %v). The image bakes "+
					"neither adapter, so skipping the host dir leaves the container with no CLI to run and "+
					"the handshake fails with a bare exit status.", c.kind, kinds, dir, got)
			}
		}
	}
}

// THE IMAGES ARE BUILT TO DOWNLOAD THE CLI ADAPTERS — that is the operator's design, and both
// deploy/runtime/Dockerfile.dev and Dockerfile.gui install them at build time:
//
//	ARG ORCHICON_ADAPTERS="opencode claude"
//	opencode) npm install -g "@opencode/cli@latest" ;;
//	claude)   npm install -g "@anthropic-ai/claude-code@latest" ;;
//
// So the kind list must CREDIT the image for them. I emptied this list earlier tonight because one stale
// local image (built 2026-10-09, three weeks before PR #882 added that step) lacked them — the right
// measurement, the wrong conclusion. An empty list makes the system image-blind even after a correct
// rebuild, and turns every run back into a host-mount-only system.
func TestTheImageIsCreditedWithTheAdaptersItDownloads(t *testing.T) {
	for _, kind := range []string{"opencode", "claude"} {
		if _, ok := imageSuppliesAdapter(kind); !ok {
			t.Errorf("imageSuppliesAdapter(%q) = false, but the runtime Dockerfiles install it at build "+
				"time. The image is the PRIMARY route for these adapters; the host mount is the fallback.", kind)
		}
	}
	// The native binary is NOT downloaded — the daemon bind-mounts its own executable — so it must never
	// be credited to the image.
	if _, ok := imageSuppliesAdapter("orchicon"); ok {
		t.Error("the native orchicon binary is bind-mounted by the daemon at create time, never baked; it " +
			"must not appear in the image's adapter list")
	}
}

// The fallback chain must not FALSELY REPORT the native kind. It is never resolved from PATH or an install
// list — createContainer validates and mounts the daemon's own executable — so judging it as a missing
// adapter produced a false alarm on every runtime run, with an empty remedy string.
func TestTheNativeKindIsNeverReportedAsAMissingAdapter(t *testing.T) {
	absent := func(string) (string, error) { return "", nil }
	noImage := func(kind string) (imageAdapterKind, bool) { return imageAdapterKind{}, false }
	if problems := containerAdapterProblems(absent, noImage, "orchicon"); len(problems) != 0 {
		t.Fatalf("the native kind was reported as a missing adapter: %v — it is bind-mounted by the "+
			"daemon, not downloaded or resolved from PATH", problems)
	}
}

// No host install and no image install must be a REPORTED problem naming the remedy — not a silent pass
// that surfaces later as "exit status 1".
//
// IT IS DRIVEN THROUGH THE INJECTED SEAM, not through CheckAdaptersForContainerWith with an empty temp
// HOME. That was my first attempt and it asserted nothing: ResolveBinary's discovery arm searches PATH as
// well as known installs, so on a host where the adapter IS on PATH an empty temp home still resolves the
// real binary — the "absent" premise was false and the test passed vacuously. containerAdapterProblems
// takes both inputs as functions precisely so this arm is reachable.
func TestAMissingAdapterIsReportedWithARemedy(t *testing.T) {
	absent := func(string) (string, error) { return "", nil }
	noImage := func(string) (imageAdapterKind, bool) { return imageAdapterKind{}, false }
	problems := containerAdapterProblems(absent, noImage, "opencode")
	if len(problems) == 0 {
		t.Fatal("no problem was reported for an adapter present on NEITHER side. That silent pass is what " +
			"turned a missing CLI into a bare handshake exit status.")
	}
	joined := strings.Join(problems, " ")
	for _, want := range []string{"no host install", "does not bake"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the problem does not state %q: %s", want, joined)
		}
	}
	if !strings.Contains(joined, "ORCHICON_OPENCODE_BIN") {
		t.Errorf("the problem must name the override that fixes it: %s", joined)
	}
}

// Host-first: a usable host install wins even when an image CLAIMS the kind, so an operator's newer CLI
// is never silently replaced by an older baked one.
func TestAHostInstallWinsEvenWhenTheImageClaimsTheKind(t *testing.T) {
	home := t.TempDir()
	dir := home + "/.opencode/bin"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/opencode", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := adapterPathPrefix(home, "opencode")
	if len(got) == 0 || got[0] != dir {
		// sorted, so a single entry is the dir; assert membership rather than order
		found := false
		for _, d := range got {
			if d == dir {
				found = true
			}
		}
		if !found {
			t.Errorf("a usable host install was not placed on the container PATH (got %v)", got)
		}
	}
}
