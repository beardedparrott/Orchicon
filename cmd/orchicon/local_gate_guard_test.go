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
