package guard

// guard_protected_path_test.go — the paths a session may never be allowed to DESTROY.
//
// The operator, after a guard test deleted their home directory: "The fact that a permission accept
// and FULLSEND can do a damaging rm -rf on /home on other directories may be a bit concerning."
//
// THEY WERE RIGHT, and this is the measurement that proved it before the rule existed. Without
// FULLSEND the shim already refused every one of these (a target outside the sanctioned set is
// blocked); WITH FULLSEND it allowed all of them, because fullsend SKIPS the sanctioned-set tests by
// design and an ancestor of the project is in no set to begin with — so nothing refused it:
//
//     rm -rf /home                 ALLOWED under FULLSEND
//     rm -rf ~                      ALLOWED under FULLSEND
//     rm -rf <project's parent>     ALLOWED under FULLSEND
//
// The rule that closes it: a target that EQUALS or CONTAINS a path the session works in or depends on
// is refused, ABOVE the fullsend skip, in the same position as the deny list — because it is the same
// category of thing, a decision no permission request can turn into an approval.
//
// EVERY TARGET HERE IS TEMP SPACE, for the reason this whole package learned the hard way: these
// tests EXECUTE real binaries, so a case the shim allows does not fail an assertion, it RUNS.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedPathsSurviveFULLSEND(t *testing.T) {
	pol := writePolicyLists(t, nil, nil) // no deny entries: the rule alone has to hold
	root := t.TempDir()
	// A home-shaped tree, so the cases below are the real relationship ( /home/user/projects/<name> )
	// rather than an arbitrary nesting.
	home := filepath.Join(root, "home", "user")
	projects := filepath.Join(home, "projects")
	proj := filepath.Join(projects, "proj")
	sibling := filepath.Join(home, "other")

	mk := func() {
		for _, d := range []string{proj, projects, home, sibling} {
			_ = os.MkdirAll(d, 0o755)
		}
	}
	mk()
	// projectDir must be the PROJECT (so the shim's GUARD_PROJECT is the scope), and HOME must be the
	// home-shaped dir for the equality cases to be meaningful.
	t.Setenv("HOME", home)
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("NewExecutionGuard: %v", err)
	}
	defer g.Close()

	cases := []struct {
		name     string
		victim   string
		fullsend bool
		want     string
	}{
		// THE REPORTED HOLE: the ancestors, with FULLSEND ON.
		{"the project's parent   FULLSEND", projects, true, "blocked"},
		{"the home directory     FULLSEND", home, true, "blocked"},
		{"the whole tree         FULLSEND", root, true, "blocked"},

		// The same cases with fullsend off, where the outside-scope rule already covered them — kept
		// so a future change to that rule cannot silently reopen the hole.
		{"the project's parent   no fullsend", projects, false, "blocked"},
		{"the home directory     no fullsend", home, false, "blocked"},

		// THE FALSE POSITIVE THAT MUST NOT EXIST: acting ON the scope root is ordinary work, so the
		// project itself stays allowed. Without this the rule would refuse `chmod -R 755 <project>`
		// and `rm -rf <project>/dist` would be the only shape left.
		{"the project ITSELF     FULLSEND", proj, true, "allowed"},
		// A SIBLING that is neither an ancestor nor a protected root: FULLSEND is supposed to widen
		// what may be written, so this must stay allowed or the mode is useless.
		{"a sibling directory    FULLSEND", sibling, true, "allowed"},
	}

	for _, tc := range cases {
		mk() // a previous "allowed" case really deletes something
		exit, out := runGuardEnv(t, g, InteractiveEnviron(pol, proj, nil, nil, tc.fullsend), "rm", "-rf", tc.victim)
		got := "allowed"
		if exit != 0 {
			got = "blocked"
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s (out: %s)", tc.name, got, tc.want, out)
		}
		// A refusal must NAME what would have been destroyed and say it is not waivable: the
		// operator's next move is to decide whether the rule is right.
		if tc.want == "blocked" {
			if !strings.Contains(out, "NEVER ALLOWED") {
				t.Errorf("%s: the refusal must say it cannot be waived: %s", tc.name, out)
			}
			if !strings.Contains(out, tc.victim) {
				t.Errorf("%s: the refusal must name the target: %s", tc.name, out)
			}
		}
	}
}

// A DENY ENTRY IS THE OPERATOR'S OWN DECISION and is consulted first, so it stays the reason given —
// the protected rule must not pre-empt it with a different message.
func TestADenyEntryIsStillTheReasonGiven(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(home, "projects", "proj")
	_ = os.MkdirAll(proj, 0o755)
	t.Setenv("HOME", home)
	// The deny entry has to cover the REAL path, which is a temp dir — the pattern is what the
	// operator would write for this target, not a literal "/home".
	pol := writePolicyLists(t, []string{home + "/**"}, nil)
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	exit, out := runGuardEnv(t, g, InteractiveEnviron(pol, proj, nil, nil, true), "rm", "-rf", home)
	if exit == 0 {
		t.Fatalf("a denied target ran: %s", out)
	}
	if !strings.Contains(out, "denied by entry") {
		t.Errorf("the operator's own deny entry must remain the stated reason, got: %s", out)
	}
}
