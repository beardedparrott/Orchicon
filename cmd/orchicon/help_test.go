package main

// help_test.go — the help must document every command the binary dispatches.
//
// THE BUG THIS PINS: `orchicon install` was absent from `--help` entirely. It is the single most
// important entry point — the one-line installers call it, and it is what an operator is told to
// run — and it was missing because the list was maintained BY HAND as a Printf with one positional
// argument per line (fifteen of them). A hand-maintained list drifts silently: nothing errors, the
// command simply is not mentioned, and the operator concludes it does not exist.
//
// So the expectation is DERIVED from the dispatch rather than restated here. A new subcommand that
// is not documented fails this test, which is the only way to keep a hand-written help honest.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dispatchedSubcommands reads the `case "…"` labels out of main.go's subcommand switch.
func dispatchedSubcommands(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Scope to the dispatch switch, so `case` labels elsewhere in the file are not picked up.
	start := strings.Index(string(src), "switch os.Args[1] {")
	if start < 0 {
		t.Fatal("main.go no longer has the subcommand switch — update this test's anchor")
	}
	end := strings.Index(string(src)[start:], "\n\t\tdefault:")
	if end < 0 {
		t.Fatal("the subcommand switch no longer has a default arm — update this test's anchor")
	}
	block := string(src)[start : start+end]

	var out []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([^"]+)"`).FindAllStringSubmatch(block, -1) {
		name := strings.TrimLeft(m[1], "-") // --version and version are the same command
		if name == "" || name == "help" || seen[name] {
			continue // `-h`/`--help` IS the help; naming it inside itself reads absurdly
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) < 10 {
		t.Fatalf("only %d subcommands found (%v) — the extraction broke, and a broken extraction "+
			"would pass this test vacuously", len(out), out)
	}
	return out
}

// helpDocuments reports whether the help has a ROW for cmd — the command in the command column,
// not merely the word appearing somewhere in the text.
//
// THE DISTINCTION IS THE WHOLE TEST, and it was measured: the first version of this used
// strings.Contains and PASSED with the `install` row renamed to `instal`, because the prose still
// said "`install` alone will not refresh it". A documentation test that a prose mention satisfies is
// not a documentation test.
func helpDocuments(out, bin, cmd string) bool {
	// A documented command is one that appears in the COMMAND COLUMN: the first token of its own
	// line. That is what distinguishes a row from a prose mention — the first version of this used
	// strings.Contains and PASSED with the `install` row renamed, because the prose still said
	// "`install` alone will not refresh it".
	//
	// Anchoring on the line start (rather than requiring the command to be the whole first token)
	// is deliberate: `db` is documented by its subcommand rows (`db backup` …), which is how a
	// command group is meant to read. No prose line begins with the binary's own name, so this
	// cannot be satisfied by a sentence.
	prefix := bin + " " + cmd
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

func TestTheHelpDocumentsEveryDispatchedCommand(t *testing.T) {
	cmds := dispatchedSubcommands(t)
	bin := filepath.Base(os.Args[0])

	// Capture printHelp's output the way an operator sees it.
	out := captureStdout(t, func() { printHelp() })

	for _, cmd := range cmds {
		if !helpDocuments(out, bin, cmd) {
			t.Errorf("`%s` is dispatched by the binary but has no ROW in --help.\n"+
				"  A command an operator cannot discover is one they will conclude does not exist — "+
				"which is exactly how `install` came to be missing from this list.\n"+
				"  Add it in printHelp (cmd/orchicon/main.go).", cmd)
		}
	}
}

// TestTheHelpNamesInstallAndRestart is the direct regression pin for the report. It is deliberately
// redundant with the loop above, because these two rows are the ones the operator asked for by name
// and a derived test that silently stopped extracting would leave them uncovered.
func TestTheHelpNamesInstallAndRestart(t *testing.T) {
	bin := filepath.Base(os.Args[0])
	out := captureStdout(t, func() { printHelp() })

	for _, want := range []string{"install", "restart"} {
		if !helpDocuments(out, bin, want) {
			t.Errorf("--help has no row for %q", want)
		}
	}
	// And the help must not present install as the way to refresh an environment, which is the
	// conflation that made the naming confusing in the first place.
	if !strings.Contains(out, "does NOT restart a plane") {
		t.Error("--help must say that install does not restart a healthy plane, so an operator " +
			"who needs an environment refresh is pointed at `restart` instead")
	}
}
