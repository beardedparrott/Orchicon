package neverallow

// neverallow_test.go — THE DECLARATION, TESTED WHERE IT LIVES.

import (
	"os"
	"strings"
	"testing"
)

// THE PACKAGE IMPORTS NOTHING INTERNAL.
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

// A `*` in a shimmed name would be a symlink literally named `mkfs.*` (nothing would ever resolve
// through it), and a name missing from the case arm would be a shim that falls through.
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

// THE TWO HALVES ARE ONE CLASS.
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

// DenyRules HANDS OUT A COPY.
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
