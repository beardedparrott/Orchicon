package neverallow

// neverallow_test.go — THE DECLARATION, TESTED WHERE IT LIVES.
//
// The behavioural proof that the guard and the opencode config both READ this
// declaration (rather than restating it) is in share_test.go. These cover the
// declaration itself: that it stays dependency-free, and that its two halves —
// the binary class and the command class — cannot drift apart.

import (
	"os"
	"strings"
	"testing"
)

// THE PACKAGE IMPORTS NOTHING INTERNAL. That is the whole reason the class was
// moved out of the two consumers: a declaration with a dependency edge is one
// the next consumer has to import a whole package to read.
func TestImportsNothingInternal(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(src), "beardedparrott/orchicon") {
			t.Errorf("%s imports an internal package — the class must stay dependency-free so guard and opencode can both read it", name)
		}
	}
}

// EVERY SHIMMED NAME IS A REAL BASENAME AND APPEARS IN THE GUARD'S CASE ARM.
// A `*` in a shimmed name would be a symlink literally named `mkfs.*` (nothing
// would ever resolve through it), and a name missing from the case arm would
// be a shim that falls through to the "unexpected invocation" branch.
func TestShimmedNamesAreRealAndCoveredByTheCasePattern(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range strings.Split(CasePattern(), "|") {
		if seen[n] {
			t.Errorf("CasePattern lists %q twice", n)
		}
		seen[n] = true
	}

	shimmed := Shimmed()
	if len(shimmed) == 0 {
		t.Fatal("Shimmed() is empty — the execution guard would shim nothing")
	}
	for _, n := range shimmed {
		if strings.Contains(n, "*") {
			t.Errorf("Shimmed() returned the pattern %q — a shim is a symlink named after a real binary", n)
		}
		if !seen[n] {
			t.Errorf("the shimmed binary %q is missing from CasePattern()", n)
		}
	}
}

// THE TWO HALVES ARE ONE CLASS. A member whose binary is refused but whose
// command string is not (or the reverse) is a hole in the class — the layer
// that happens to see the other spelling lets it through.
func TestEveryBinaryHasACommandPattern(t *testing.T) {
	for _, b := range Binaries {
		if strings.Contains(b.Name, "*") {
			continue // a family pattern, matched by the patterns below it
		}
		covered := false
		for _, p := range CommandPatterns {
			if patternCoversName(p, b.Name) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("binary %q has no command pattern — the binary class and the command class have drifted", b.Name)
		}
	}
}

// patternCoversName reports whether a bash deny pattern can match the command
// that invokes name.
func patternCoversName(pattern, name string) bool {
	if base := strings.TrimSuffix(pattern, " *"); base == name {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(name, prefix) || strings.HasPrefix(prefix, name)
	}
	return false
}

// DenyRules HANDS OUT A COPY. The worker profile APPENDS its own project
// boundary rules to it; if that append aliased the declaration's backing array
// a worker profile could silently rewrite the shared class.
func TestDenyRulesIsACopy(t *testing.T) {
	if len(CommandPatterns) == 0 {
		t.Fatal("CommandPatterns is empty")
	}
	got := DenyRules()
	got[0] = "orchicon-mutated-sentinel"
	if CommandPatterns[0] == "orchicon-mutated-sentinel" {
		t.Fatal("DenyRules() returned the declaration itself — a consumer's append would corrupt the shared class")
	}
}
