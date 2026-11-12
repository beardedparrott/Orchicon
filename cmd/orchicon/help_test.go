package main

// help_test.go — the help must document every command the binary dispatches.

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

// helpDocuments reports whether the help has a ROW for cmd — the command in the command column, not
// merely the word appearing somewhere in the text.
func helpDocuments(out, bin, cmd string) bool {
	// A documented command is one that appears in the COMMAND COLUMN: the first token of its own line.
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

// TestTheHelpNamesInstallAndRestart is the direct regression pin for the report.
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

// TestTheHelpSaysServeStatusIsTheSameSurface pins the claim the row now makes: `serve --status` is
// not a second, weaker status surface — it reports what `orchicon plane status` reports.
func TestTheHelpSaysServeStatusIsTheSameSurface(t *testing.T) {
	bin := filepath.Base(os.Args[0])
	out := captureStdout(t, func() { printHelp() })

	lines := strings.Split(out, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), bin+" serve --status") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("--help has no row for `serve --status`")
	}
	// The row and its continuation lines: everything until the next row (a line that begins with the
	// binary's own name) or a blank line.
	row := []string{lines[start]}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, bin+" ") {
			break
		}
		row = append(row, line)
	}
	joined := strings.Join(row, " ")

	if !strings.Contains(joined, "plane status") {
		t.Errorf("the `serve --status` row must say it reports the same thing as `plane status`, or an "+
			"operator will assume the old PID-only answer:\n%s", joined)
	}
	if strings.Contains(joined, "Report whether a detached server is running") {
		t.Errorf("the old PID-only wording is back; `serve --status` reports the instance, the serving "+
			"binary, drift and health through planecontrol.StatusOf:\n%s", joined)
	}
	if helpDocuments(out, bin, "serve --start") {
		t.Errorf("--help must NOT document `serve --start`: it does not exist, and `orchicon start` is the " +
			"verb. A row for it would invite exactly the command the CLI refuses.")
	}
	// …but the reader who expects it is told where to go, which is what AC1 asks for.
	if !strings.Contains(out, "serve --start") {
		t.Errorf("--help never mentions `serve --start`; a reader who expects that flag is left with no " +
			"pointer to `orchicon start`")
	}
}
