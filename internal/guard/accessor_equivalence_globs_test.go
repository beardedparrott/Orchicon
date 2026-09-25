package guard

// accessor_equivalence_globs_test.go — the second half of AC-6: the shim's
// hand-written bash matcher must reach permpolicy.Decide's verdict for the
// pattern SHAPES the accessor accepts, not just for plain literals.
//
// Each case here was a REAL divergence found by sweeping the shim against
// permpolicy.Decide. The first two are FAIL-OPEN — the shim ran a command the
// operator's deny list refused, in the one component whose job is the
// subprocess hole consent cannot see:
//
//   * a deny entry using {a,b} alternation was never matched (bash 'case' has
//     no brace expansion), so the denied target was deleted;
//   * a deny entry with two '/**/' runs was only tried with its runs collapsed
//     front-to-back, so 'a/**/b/**/c' never matched 'a/x/b/c'.
//
// The rest are drift (the shim refusing a path the accessor proceeds on), which
// is the failure mode the work item names: a command that prompts and is then
// refused anyway.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

func TestShimMatchesDecideOnPatternShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mkfile := func(rel string) string {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Fail-open sweep: the guard's PROJECT_DIR IS the home dir, so a target the
	// deny list does NOT cover is allowed to run — a deny entry the shim misses
	// deletes the file.
	t.Run("deny entries cover what Decide refuses", func(t *testing.T) {
		cases := []struct{ name, entry, target string }{
			{"brace alternation", filepath.Join(home, "a", "{x,y}.txt"), mkfile("a/x.txt")},
			{"brace alternation sibling", filepath.Join(home, "a", "{x,y}.txt"), mkfile("a/y.txt")},
			{"two double-star runs", filepath.Join(home, "**", "a", "**", "x.txt"), mkfile("q/a/x.txt")},
			{"two double-star runs, first matches", filepath.Join(home, "**", "a", "**", "x.txt"), mkfile("x/a/q/x.txt")},
			{"character class", filepath.Join(home, "a", "[xy].txt"), mkfile("a/x.txt")},
			{"question mark", filepath.Join(home, "a", "?.txt"), mkfile("a/x.txt")},
			{"star matching zero characters", filepath.Join(home, "secret") + "*", mkfile("secret")},
			{"double-star file glob", filepath.Join(home, "**", "*.pem"), mkfile("deep/k.pem")},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				policy := writePolicyLists(t, []string{tc.entry}, nil)
				want, err := permpolicy.NewStore(policy).Decide(tc.target, permpolicy.Inputs{ProjectDefault: true})
				if err != nil {
					t.Fatalf("Decide: %v", err)
				}
				if want.Verdict != permpolicy.VerdictDeny {
					t.Fatalf("test bug: Decide must deny %s (entry %q): %s", tc.target, tc.entry, want.Verdict)
				}
				g, err := NewExecutionGuardWithPolicy(home, policy)
				if err != nil {
					t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
				}
				defer g.Close()

				exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, home, nil, nil), "rm", "-f", tc.target)
				if exit == 0 {
					t.Fatalf("FAIL-OPEN: the shim ran 'rm -f %s' — permpolicy.Decide denies it by entry %q", tc.target, want.Entry)
				}
				if !strings.Contains(out, "denied by entry") || !strings.Contains(out, want.Entry) {
					t.Fatalf("the refusal must name the deny entry %q: %s", want.Entry, out)
				}
				if _, err := os.Stat(tc.target); err != nil {
					t.Fatalf("a path the deny list covers was deleted: %v", err)
				}
			})
		}
	})

	// Drift sweep: no project, so the accept entry is the ONE thing that can
	// let the command run.
	t.Run("accept entries cover what Decide proceeds on", func(t *testing.T) {
		cases := []struct{ name, entry, target string }{
			{"brace alternation", filepath.Join(home, "a", "{x,y}.txt"), mkfile("a/x.txt")},
			{"two double-star runs", filepath.Join(home, "**", "a", "**", "x.txt"), mkfile("q/a/x.txt")},
			{"character class", filepath.Join(home, "a", "[xy].txt"), mkfile("a/x.txt")},
			{"question mark", filepath.Join(home, "a", "?.txt"), mkfile("a/x.txt")},
			{"double-star covers the directory itself", home + "/**", mkfile("nested/deep.txt")},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				policy := writePolicyLists(t, nil, []string{tc.entry})
				want, err := permpolicy.NewStore(policy).Decide(tc.target, permpolicy.Inputs{})
				if err != nil {
					t.Fatalf("Decide: %v", err)
				}
				if want.Verdict != permpolicy.VerdictAccept {
					t.Fatalf("test bug: Decide must accept %s (entry %q): %s", tc.target, tc.entry, want.Verdict)
				}
				g, err := NewExecutionGuardWithPolicy("", policy)
				if err != nil {
					t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
				}
				defer g.Close()

				victim := tc.target
				if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, "", nil, nil), "rm", "-f", victim)
				if exit != 0 {
					t.Fatalf("DRIFT: Decide accepts %s by entry %q but the shim refused: %s", victim, want.Entry, out)
				}
				if _, err := os.Stat(victim); !os.IsNotExist(err) {
					t.Fatalf("the accepted target was not deleted")
				}
			})
		}
	})

	// A deny entry the operator spelled with a home reference is recognised even
	// when the TARGET carries the reference too (the accessor matches either
	// spelling on either side), and the refusal names the operator's own text.
	t.Run("tilde-written target is denied by name", func(t *testing.T) {
		key := filepath.Join(home, ".ssh", "id_rsa")
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		policy := writePolicyLists(t, []string{"~/.ssh/**"}, nil)
		want, err := permpolicy.NewStore(policy).Decide("~/.ssh/id_rsa", permpolicy.Inputs{})
		if err != nil || want.Verdict != permpolicy.VerdictDeny {
			t.Fatalf("Decide: %v %s", err, want.Verdict)
		}
		g, err := NewExecutionGuardWithPolicy(home, policy)
		if err != nil {
			t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
		}
		defer g.Close()

		exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, home, nil, nil), "rm", "-f", "~/.ssh/id_rsa")
		if exit == 0 {
			t.Fatalf("FAIL-OPEN: the shim ran rm on a denied path")
		}
		if !strings.Contains(out, "denied by entry") || !strings.Contains(out, "~/.ssh/**") {
			t.Fatalf("the refusal must name the entry as the operator wrote it: %s", out)
		}
	})

	// An entry the translation cannot vouch for (a nested brace group) must
	// REFUSE in the interactive profile rather than be silently dropped — a
	// dropped entry is a policy the operator believes is in force.
	t.Run("untranslatable entry fails closed", func(t *testing.T) {
		policy := writePolicyLists(t, []string{filepath.Join(home, "a", "{x,{y,z}}.txt")}, nil)
		target := mkfile("a/x.txt")
		g, err := NewExecutionGuardWithPolicy(home, policy)
		if err != nil {
			t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
		}
		defer g.Close()

		exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, home, nil, nil), "rm", "-f", target)
		if exit == 0 || !strings.Contains(out, "fail-closed") {
			t.Fatalf("the shim must fail closed on an entry it cannot evaluate: exit=%d %s", exit, out)
		}
	})
}
