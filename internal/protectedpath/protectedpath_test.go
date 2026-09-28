package protectedpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homedir() (string, error) { return os.UserHomeDir() }

// The rule at its boundaries. The doc comment's ordering claim — a target that EQUALS or CONTAINS a
// root is refused — is what this pins, in both directions, because the cheap mistake here is a prefix
// test that makes /home2 look like it contains /home.
func TestMachineRootsRefuseEqualityAndContainment(t *testing.T) {
	home := "/home/ops"
	roots := Roots(home)

	for _, target := range []string{
		"/",     // the filesystem root, by equality
		"/home", // == the home dir, the reported case
		home,    // `rm -rf ~`
		home + "/.local/share/orchicon",
		home + "/.orchicon",
	} {
		if got := DestroyedBy(target, roots, nil); got == "" {
			t.Errorf("DestroyedBy(%q) = \"\" — a machine root must be refused on EQUALITY too", target)
		}
	}

	// CONTAINMENT: an ancestor of the home directory.
	if got := DestroyedBy("/home", Roots("/home/ops/deeper"), nil); got == "" {
		t.Error("an ancestor of the home directory must be refused")
	}

	// Inside the home directory is ordinary work, not a machine root.
	if got := DestroyedBy(home+"/projects/Orchicon/file.txt", roots, nil); got != "" {
		t.Errorf("a target INSIDE home was refused by %q — that is ordinary work", got)
	}
}

// A PREFIX IS NOT CONTAINMENT. /home2 must not be treated as containing /home.
func TestDestroyedByIsNotFooledByAPrefix(t *testing.T) {
	roots := Roots("/home/ops")
	for _, sibling := range []string{"/home/ops2", "/home/ops-other", "/home/operations", "/home"} {
		// /home is a legitimate ancestor of /home/ops, so it is expected to be refused; the three
		// siblings are not.
		if sibling == "/home" {
			continue
		}
		if got := DestroyedBy(sibling, roots, nil); got != "" {
			t.Errorf("DestroyedBy(%q) = %q — a sibling sharing a prefix must not be refused", sibling, got)
		}
	}
}

// THE WORK SCOPE IS DIFFERENT: equality is allowed, containment is not.
//
// This is the split that keeps the rule from breaking ordinary work — `chmod -R 755 <project>` and
// `rm -rf <project>/dist` both name the scope root — while still refusing the directory that CONTAINS
// the project, which is the catastrophe (it takes the project with it).
func TestScopeRootsRefuseContainmentButNotEquality(t *testing.T) {
	proj := "/home/ops/projects/Orchicon"
	scope := ScopeRoots(proj, []string{"/tmp/work"})

	// EQUALITY: allowed. Acting ON the scope root is ordinary work.
	if got := DestroyedBy(proj, nil, scope); got != "" {
		t.Errorf("DestroyedBy(%q) = %q — the scope root itself must NOT be refused; chmod -R 755 "+
			"<project> and rm -rf <project>/dist are ordinary operations", proj, got)
	}
	if got := DestroyedBy("/tmp/work", nil, scope); got != "" {
		t.Errorf("a granted dir's own root must not be refused, got %q", got)
	}

	// CONTAINMENT: refused, every level up.
	for _, ancestor := range []string{
		"/home/ops/projects",
		"/home/ops",
		"/home",
		"/",
	} {
		if got := DestroyedBy(ancestor, nil, scope); got == "" {
			t.Errorf("DestroyedBy(%q) = \"\" — an ancestor of the project must be refused", ancestor)
		}
	}

	// INSIDE the project: ordinary work.
	if got := DestroyedBy(proj+"/dist", nil, scope); got != "" {
		t.Errorf("a target INSIDE the project was refused by %q", got)
	}

	// A GRANT MUST NOT DISSOLVE THE RULE: a granted directory's ancestors are refused just as the
	// project's are, or granting /tmp would license `rm -rf /tmp/..`.
	if got := DestroyedBy("/tmp", nil, scope); got == "" {
		t.Error("an ancestor of a GRANTED directory must be refused — a grant widens what may be " +
			"written, not what may be destroyed")
	}
}

// An EMPTY value must never become a root: filepath.Clean("") is ".", and every relative target is
// "inside" it — which would refuse all relative work in an unassigned conversation.
func TestEmptyValuesNeverBecomeRoots(t *testing.T) {
	for _, r := range append(Roots(""), ScopeRoots("", []string{"", "  "})...) {
		if r == "" || r == "." {
			t.Fatalf("an empty value became a root: %q", r)
		}
	}
	if got := DestroyedBy("some/relative/file.txt", Roots(""), ScopeRoots("", nil)); got != "" {
		t.Errorf("a relative target was refused by %q", got)
	}
	// The relative spellings are skipped outright rather than resolved against the cwd.
	for _, t2 := range []string{".", "..", "", "   "} {
		if got := DestroyedBy(t2, Roots("/home/ops"), ScopeRoots("/home/ops/p", nil)); got != "" {
			t.Errorf("DestroyedBy(%q) = %q — a relative/short form must not resolve into a root", t2, got)
		}
	}
}

// Roots derives from the machine so `rm -rf ~` is refused with no configuration at all.
func TestRootsDerivesTheMachineRoots(t *testing.T) {
	roots := Roots("")
	joined := strings.Join(roots, " ")
	if !strings.Contains(joined, string(filepath.Separator)) {
		t.Errorf("the filesystem root is missing from %v", roots)
	}
	if h, err := homedir(); err == nil && h != "" && !strings.Contains(joined, h) {
		t.Errorf("the home directory (%q) is missing from %v", h, roots)
	}
}

// The refusal names both paths and says it cannot be waived — the operator's next move is to decide
// whether the rule is right, and a message that says only "blocked" cannot be checked.
func TestRefusalNamesBothPathsAndTheNonWaivability(t *testing.T) {
	got := Refusal("/home", "/home")
	for _, want := range []string{"/home", "FULLSEND", "never allowed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal must contain %q: %q", want, got)
		}
	}
}
