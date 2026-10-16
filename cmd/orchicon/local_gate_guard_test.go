package main

import (
	"os"
	"strings"
	"testing"
)

// local_gate_guard_test.go — the Makefile's local gate must not misreport its own failure.
//
// THE FAILURE THIS PINS. `PKGS` fed `vet` and `test` (and through them `make ci`, so every
// `make rebuild-dev` / `rebuild-prod` / `rebuild-test`). It was:
//
//	PKGS = $(shell $(GO) list ./... 2>/dev/null | grep -v '/tmp/')
//
// An agent had run with a RELATIVE GOMODCACHE — relative to the repo root — so a whole module
// cache landed INSIDE the repo at tmp/orchicon/gomodcache: ~1.2 GB of module sources with no
// go.mod. `go list ./...` then aborted the WHOLE pattern ("directory ... outside main module or
// its selected dependencies") and printed NOTHING on stdout. `2>/dev/null` discarded the one line
// that named the cause, so a FAILED `go list` and an EMPTY match collapsed into the same empty
// string — and the guard blamed the cause it knew about:
//
//	ERROR: no packages matched ./... (tmp/ excluded) — refusing to run the gate over nothing
//
// over 98 perfectly healthy packages, in a tree whose only sin was a stray scratch cache. A human
// reasonably reads that as "the merge or the module is broken", which is the opposite of the truth.
// The diagnosis cost a full manual investigation of a healthy checkout.
//
// WHY A TEST AND NOT JUST A BETTER COMMENT. The two facts that make the message actionable are
// MECHANICAL, and prose decays: the error must be CAPTURED (nothing on the shell may send it to
// /dev/null), and the guard must have an arm that reports it rather than the empty-match text.
// A later edit that reinstates `2>/dev/null`, drops the captured variable, or simplifies the guard
// back to the single message reverts the fix silently — nothing else in the repo reads the
// Makefile, so no other test would notice. This is the same shape as the repo's other config-pin
// tests (ci_release_workflows_test.go parses release.yml; install_ports_test.go pins the port table
// against scripts/container.sh): the agreement is asserted, not left to care.
//
// It deliberately does NOT assert the exact wording — copy edits are fine and desirable. It asserts
// the STRUCTURE: the capture exists and is separate from PKGS, the strict `go list` survives (an
// `-e`/tolerant listing would tolerate real module breakage, which is strictly worse than a clear
// error), and the guard has a distinct arm for the failed case.

func readRootFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// makefileLogicalLines joins backslash continuations so a recipe may be reformatted without the
// assertion depending on where the author wrapped it.
func makefileLogicalLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if n := len(out); n > 0 && strings.HasSuffix(out[n-1], "\\") {
			out[n-1] = strings.TrimSuffix(out[n-1], "\\") + " " + strings.TrimSpace(line)
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestMakefilePKGSReportsAFailedGoList(t *testing.T) {
	logical := makefileLogicalLines(readRootFile(t, "../../Makefile"))

	var pkgsLine, errLine string
	var guards []string
	for _, line := range logical {
		switch {
		case strings.HasPrefix(line, "PKGS"):
			pkgsLine = line
		case strings.HasPrefix(line, "GO_LIST_ERR"):
			errLine = line
		case strings.Contains(line, `test -n "$(PKGS)"`):
			guards = append(guards, line)
		}
	}

	// --- the capture exists, and captures the ERROR rather than discarding it -----------------
	if errLine == "" {
		t.Fatal("the Makefile no longer captures `go list`'s failure into GO_LIST_ERR.\n" +
			"Without it a failed `go list` is indistinguishable from an empty package match, and the " +
			"guard reports the wrong cause — the regression this test exists for.")
	}
	if !strings.Contains(errLine, "2>&1") {
		t.Errorf("GO_LIST_ERR does not merge stderr into stdout (%q).\n"+
			"`go list ./...` prints its failure reason on STDERR and nothing on stdout, so without "+
			"2>&1 the capture is empty exactly when it matters.", errLine)
	}
	if strings.Contains(errLine, "2>/dev/null") {
		t.Errorf("GO_LIST_ERR sends stderr to /dev/null (%q) — that is the original defect.", errLine)
	}
	if !strings.Contains(errLine, "> /dev/null") && !strings.Contains(errLine, ">/dev/null") {
		t.Errorf("GO_LIST_ERR does not redirect the error-capture's stdout away (%q).\n"+
			"It must end up with the ERROR only; leaking the package list into it would make the "+
			"failed-case and empty-case arms fire together.", errLine)
	}

	// --- PKGS is still strict, and still excludes the local scratch ---------------------------
	if pkgsLine == "" {
		t.Fatal("PKGS is gone from the Makefile — every local gate target walks the tree through it.")
	}
	if !strings.Contains(pkgsLine, "grep -v '/tmp/'") {
		t.Errorf("PKGS no longer excludes tmp/ (%q).\n"+
			"tmp/ is the gitignored scratch dir: it carries no go.mod, so `./...` walks straight in "+
			"and compiles whatever is lying there as part of this module.", pkgsLine)
	}
	if strings.Contains(pkgsLine, "-e ./...") || strings.Contains(pkgsLine, "-e ") {
		t.Errorf("PKGS uses a tolerant listing (%q).\n"+
			"`go list -e` would suppress the very error that must surface, and would let a genuinely "+
			"broken package through the gate. Strict `go list` is deliberate.", pkgsLine)
	}

	// --- the guard distinguishes the two cases ------------------------------------------------
	if len(guards) < 2 {
		t.Fatalf("expected a PKGS guard on both gate targets (test and vet), found %d", len(guards))
	}
	for _, g := range guards {
		if !strings.Contains(g, "GO_LIST_ERR") {
			t.Errorf("a PKGS guard does not consult GO_LIST_ERR:\n  %s\n"+
				"Both arms are required: one reports `go list`'s own error, the other keeps the "+
				"empty-match refusal for genuine cases.", g)
		}
		if !strings.Contains(g, "no packages matched ./... (tmp/ excluded)") {
			t.Errorf("a PKGS guard dropped the empty-match branch:\n  %s\n"+
				"That message is still correct when `go list` SUCCEEDS and matches nothing — it is "+
				"the failed case that must not be reported with it.", g)
		}
	}
}

// TestCrossCompileVetsTheFilteredPackageList pins the SECOND half of the same defect, and the two
// halves are separate facts that failed separately.
//
// THE FAILURE THIS PINS. The Makefile states its scratch exclusion as "ONE variable used by every
// target that walks the tree, so a target added later cannot forget it" — but `cross-compile` WAS
// such a target and had forgotten it: it vetted a literal `./...` of its own.
//
//	tmp/cleanup-backup-.../gotmp-preserved/restore.go: undefined: PolicyEvaluator
//	make: *** [Makefile:748: cross-compile] Error 1
//
// `tmp/` is the gitignored scratch dir and carries no go.mod, so `./...` walks into it and compiles
// whatever is lying there as part of this module. The file was a preserved snapshot of a moved
// type, so it could never compile — and it was PRESERVED ON PURPOSE, which is the point: scratch a
// human deliberately keeps is still scratch, and the gate must not be hostage to it.
//
// THE ASYMMETRY IS THE INVARIANT WORTH PINNING. `make vet` and `make test` consult PKGS, so they
// saw nothing wrong and reported GREEN; `cross-compile` did not, so it failed. A tree could be
// declared healthy by every gate except the one that runs last in `rebuild-dev` — which is exactly
// how a correct-looking fix came back broken. So this test asserts BOTH: that cross-compile vets
// the filtered list, and that NO target anywhere vets a bare `./...` — because the next target that
// walks the tree is the one that will forget, and prose in a comment cannot stop it.
func TestCrossCompileVetsTheFilteredPackageList(t *testing.T) {
	logical := makefileLogicalLines(readRootFile(t, "../../Makefile"))

	// --- no target may vet a bare `./...` — the general form of the defect --------------------
	for _, line := range logical {
		if strings.Contains(line, "$(GO) vet ./...") {
			t.Errorf("a target vets a literal `./...`:\n  %s\n"+
				"`./...` walks the gitignored scratch dir tmp/, which has no go.mod, so any stray file "+
				"left there is compiled as part of this module and can fail the gate on a healthy tree. "+
				"Vet $(PKGS), which is the same module-wide listing with tmp/ removed.", line)
		}
	}

	// Scope the rest to the cross-compile recipe: from its target line to the next line that starts
	// in column 0 (the next target, comment or variable).
	start := -1
	for i, line := range logical {
		if strings.HasPrefix(line, "cross-compile:") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("cross-compile is gone from the Makefile — it is the gate that compiles the shipped " +
			"binaries for every release platform.")
	}
	var recipe []string
	for _, line := range logical[start+1:] {
		if line == "" || (line[0] != '\t' && line[0] != ' ') {
			break
		}
		recipe = append(recipe, line)
	}
	if len(recipe) == 0 {
		t.Fatal("the cross-compile recipe is empty — refusing to assert over nothing.")
	}
	joined := strings.Join(recipe, "\n")

	// --- it vets, and it vets the FILTERED list -----------------------------------------------
	if !strings.Contains(joined, "$(GO) vet $(PKGS)") {
		t.Errorf("cross-compile does not vet the filtered package list:\n  %s\n"+
			"Without this it vets whatever `./...` resolves to, scratch included — the failure above.", joined)
	}

	// --- and the scope it defends is NOT narrowed ---------------------------------------------
	//	The comment in the Makefile argues at length for a module-wide vet rather than just the two
	//	command packages. `$(PKGS)` preserves that: it IS a module-wide `go list ./...`, strict,
	//	with the go.mod-less scratch removed. Assert the pieces that make that true.
	if !strings.Contains(joined, "$(GO) build -o \"$$tmp/\" ./cmd/orchicon ./cmd/orch") {
		t.Errorf("cross-compile no longer builds BOTH shipped binaries:\n  %s", joined)
	}
	if !strings.Contains(joined, `GOOS="$$os"`) || !strings.Contains(joined, `GOARCH="$$arch"`) {
		t.Errorf("cross-compile no longer drives the vet with the loop's per-platform GOOS/GOARCH:\n"+
			"  %s\n"+
			"Vetting on the host only would miss the platform-specific breaks this gate exists for.", joined)
	}

	// --- an empty list must refuse, not silently pass -----------------------------------------
	//	`go vet` with NO arguments vets the package in the current directory, so an empty PKGS would
	//	make the gate report success while checking one package instead of the module.
	if !strings.Contains(joined, `test -n "$(PKGS)"`) {
		t.Errorf("cross-compile lost its empty-PKGS guard:\n  %s\n"+
			"An empty list reaches `go vet` as no arguments at all, which vets the CURRENT DIRECTORY "+
			"and reports success — a gate that passes while checking almost nothing.", joined)
	}
}
