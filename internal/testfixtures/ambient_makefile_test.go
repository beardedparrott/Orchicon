package testfixtures

// ambient_makefile_test.go — THE GATE MUST UNSET THE SAME LIST THE PACKAGES DO.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makefileAmbientUnsetList extracts the ORCHICON_* names from the Makefile `test` target's unset loop.
func makefileAmbientUnsetList(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	src := string(body)

	// The loop, joined across its line continuations: `for v in A B \` … `; do unset "$$v"; done;`
	start := strings.Index(src, "for v in ")
	if start < 0 {
		t.Fatalf("the Makefile no longer has a `for v in` unset loop — either the isolation was removed or it " +
			"was rewritten in a form this gate cannot see, and this gate must be updated with it")
	}
	rest := src[start:]
	end := strings.Index(rest, `done`)
	if end < 0 {
		t.Fatalf("the unset loop has no `done`")
	}
	loop := strings.ReplaceAll(rest[:end], "\\\n", " ")

	var names []string
	for _, f := range strings.Fields(loop) {
		if strings.HasPrefix(f, "ORCHICON_") {
			names = append(names, strings.TrimRight(f, ";"))
		}
	}
	if len(names) == 0 {
		t.Fatalf("the unset loop names no ORCHICON_ variables: %q", loop)
	}
	return names
}

// TestTheMakefileUnsetsEveryAmbientVariable is the drift guard.
func TestTheMakefileUnsetsEveryAmbientVariable(t *testing.T) {
	got := makefileAmbientUnsetList(t)
	inMakefile := make(map[string]bool, len(got))
	for _, n := range got {
		inMakefile[n] = true
	}
	want := make(map[string]bool, len(AmbientConfigEnv))
	for _, n := range AmbientConfigEnv {
		want[n] = true
		if !inMakefile[n] {
			t.Errorf("AmbientConfigEnv names %s and the Makefile's `test` target does not unset it.\n\n"+
				"THAT IS THE OPERATOR-ONLY FAILURE: their shell exports it (scripts/lib/instances.sh), CI does "+
				"not, so `make test` runs that package against the LIVE plane's state while CI runs it against "+
				"nothing. Add it to the Makefile's unset loop.", n)
		}
	}
	for n := range inMakefile {
		if !want[n] {
			t.Errorf("the Makefile unsets %s and AmbientConfigEnv does not name it. Either it belongs in "+
				"AmbientConfigEnv (so the per-package isolates unset it too) or it should not be unset at all", n)
		}
	}
}

// TestTheModelRegistryPackageIsIsolated pins the ONE omission this file was written for. WHY ONLY
// THIS PACKAGE, RATHER THAN A LIST OF EVERY ONE.
func TestTheModelRegistryPackageIsIsolated(t *testing.T) {
	path := filepath.Join("..", "orchicon", "isolate_test.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("internal/orchicon needs an isolate_test.go: SourcingService.registryCacheDir falls back to "+
			"ORCHICON_DATA_DIR, which an operator's shell exports pointing at a live plane — so this package's "+
			"tests read the LIVE plane's model-registry cache locally and an isolated path in CI. Pattern:\n\n"+
			"\tfunc init() { testfixtures.UnsetAmbientConfigEnv() }\n\n(%v)", err)
	}
	if !strings.Contains(string(body), "UnsetAmbientConfigEnv") {
		t.Fatal("internal/orchicon/isolate_test.go does not call UnsetAmbientConfigEnv — a file that exists but " +
			"does nothing is worse than none, because it satisfies this check")
	}
}

// TestTheMakefileUnsetLoopIsParsedNotGuessed guards the gate itself: if the Makefile's shape changes so the
// extraction above stops seeing the variables, this gate would pass vacuously.
func TestTheMakefileUnsetLoopIsParsedNotGuessed(t *testing.T) {
	names := makefileAmbientUnsetList(t)
	if len(names) < len(AmbientConfigEnv) {
		t.Fatalf("the extraction found only %d of %d variables: %v", len(names), len(AmbientConfigEnv), names)
	}
	// Every extracted name must look like an environment variable, not a fragment of shell syntax.
	ok := regexp.MustCompile(`^ORCHICON_[A-Z0-9_]+$`)
	for _, n := range names {
		if !ok.MatchString(n) {
			t.Fatalf("extracted a non-variable token %q — the parser is picking up shell syntax, so its "+
				"comparison proves nothing", n)
		}
	}
}
