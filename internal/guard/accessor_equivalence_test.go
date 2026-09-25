package guard

// accessor_equivalence_test.go — AC-6: "the guard and the consent layer read the
// SAME policy accessor", asserted by TEST rather than by inspection.
//
// The shim keeps a hand-written bash reader of the policy file (the merged,
// reviewed parser the shipped-format test already pins). The property that makes
// that safe is that the two readers AGREE: for the same policy and the same
// target, the shim's verdict is permpolicy.Store.Decide's verdict — including
// naming the same deny entry. This test fails the moment either side starts
// reading a different file or applying a different order.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

func TestShimVerdictEqualsPermpolicyDecide(t *testing.T) {
	// HOME lives outside the project/scratch set, so a matched entry is the ONE
	// thing that can refuse the target: the interactive shim refuses any
	// uncovered absolute target anyway, and the entry must be what did it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshKey := filepath.Join(home, ".ssh", "id_rsa")
	if err := os.MkdirAll(filepath.Dir(sshKey), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshKey, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(other, "file.txt")
	if err := os.WriteFile(otherFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		policy string
		target string
	}{
		{"deny entry (tilde)", writePolicyLists(t, []string{"~/.ssh/**"}, nil), sshKey},
		{"deny entry (glob)", writePolicyLists(t, []string{other + "/**"}, nil), otherFile},
		{"accept entry", writePolicyLists(t, nil, []string{other + "/**"}), otherFile},
		{"nothing covers it", writePolicyLists(t, nil, nil), otherFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := permpolicy.NewStore(tc.policy)
			want, err := store.Decide(tc.target, permpolicy.Inputs{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			g, err := NewExecutionGuardWithPolicy("", tc.policy)
			if err != nil {
				t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
			}
			defer g.Close()

			exit, out := runGuardEnv(t, g, InteractiveEnviron(tc.policy, "", nil, nil), "rm", "-f", tc.target)

			// The shim refuses exactly when Decide says "do not proceed"
			// (deny / ask / none); it runs the command when Decide proceeds
			// (grant / accept / project).
			wantRefuse := !want.Verdict.Proceed()
			if gotRefuse := exit != 0; gotRefuse != wantRefuse {
				t.Fatalf("verdict mismatch: shim refuses=%v (exit %d: %s); Decide=%s (proceed=%v)",
					gotRefuse, exit, out, want.Verdict, want.Verdict.Proceed())
			}
			if want.Verdict == permpolicy.VerdictDeny {
				// BOTH must name the SAME entry: a refusal that does not is
				// unfixable by the operator.
				if !strings.Contains(out, "denied by entry") || !strings.Contains(out, want.Entry) {
					t.Fatalf("the shim refusal must name the deny entry %q that Decide named: %s", want.Entry, out)
				}
				ref := store.Refusal(tc.target, want.Entry)
				if ref == nil || !strings.Contains(ref.Error(), want.Entry) || !strings.Contains(ref.Error(), tc.target) {
					t.Fatalf("the consent layer's refusal must name the same entry and target: %v", ref)
				}
			}
		})
	}

	// Malformed: BOTH fail closed. The shim refuses via failed_closed; Decide
	// returns an error (which the consent core turns into a rejection).
	t.Run("malformed fails closed on both sides", func(t *testing.T) {
		bad := writePolicyFileRaw(t, "deny: 3\n")
		if _, err := permpolicy.NewStore(bad).Decide(sshKey, permpolicy.Inputs{}); err == nil {
			t.Fatalf("Decide must reject a malformed policy")
		}
		g, err := NewExecutionGuardWithPolicy("", bad)
		if err != nil {
			t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
		}
		defer g.Close()
		exit, out := runGuardEnv(t, g, InteractiveEnviron(bad, "", nil, nil), "rm", "-f", sshKey)
		if exit == 0 || !strings.Contains(out, "fail-closed") {
			t.Fatalf("the shim must fail closed on a malformed policy: exit=%d %s", exit, out)
		}
	})
}

// TestShimMatchesDecideWhereAFirstMatchReadWouldDiverge pins the shim's reader
// against the accessor on the cases a naive first-match-in-file-order read gets
// WRONG — the shim allowing a path permpolicy.Decide DENIES is a fail-open in
// the one component whose whole job is the subprocess hole consent cannot see.
//
// It also pins the glob semantics: bash's own '*' crosses '/' while
// doublestar's does not, so a shim that globbed naively would both deny paths
// the accessor allows (drift) and accept paths it denies.
func TestShimMatchesDecideWhereAFirstMatchReadWouldDiverge(t *testing.T) {
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

	grant := filepath.Join(home, "grant")
	denyTarget := mkfile("grant/sub/file.txt") // two levels below the grant
	oneBelow := mkfile("grant/plain.txt")      // one level below the grant
	zeroSeg := mkfile("a/x")                   // for a '**' matching ZERO segments

	cases := []struct {
		name   string
		body   string
		grant  string
		target string
	}{
		{
			// An accept section written ABOVE the deny section. permpolicy.Decide
			// reads its deny half first, so this is a DENY; a first-match read
			// hits the accept entry first and would run the command.
			name:   "accept section above deny",
			body:   "accept:\n  - " + grant + "/**\ndeny:\n  - " + denyTarget + "\n",
			grant:  grant,
			target: denyTarget,
		},
		{
			// '**' matches ZERO path segments: /a/**/x covers /a/x.
			name:   "double star matches zero segments",
			body:   "deny:\n  - " + filepath.Join(home, "a", "**", "x") + "\naccept:\n  - " + filepath.Join(home, "a", "**") + "\n",
			grant:  filepath.Join(home, "a"),
			target: zeroSeg,
		},
		{
			// '*' does not cross '/': /grant/* covers the files directly in
			// /grant, NOT /grant/sub/file.txt — which the session grant covers,
			// so the accessor proceeds and the shim must not deny it either.
			name:   "single star does not cross a slash",
			body:   "deny:\n  - " + grant + "/*\naccept: []\n",
			grant:  grant,
			target: denyTarget,
		},
		{
			name:   "plain deny inside a granted directory",
			body:   "deny:\n  - " + oneBelow + "\naccept: []\n",
			grant:  grant,
			target: oneBelow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := writePolicyFileRaw(t, tc.body)
			want, err := permpolicy.NewStore(policy).Decide(tc.target, permpolicy.Inputs{SessionGranted: true})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			g, err := NewExecutionGuardWithPolicy("", policy)
			if err != nil {
				t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
			}
			defer g.Close()

			exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, "", []string{tc.grant}, nil), "rm", "-f", tc.target)
			wantRefuse := !want.Verdict.Proceed()
			if gotRefuse := exit != 0; gotRefuse != wantRefuse {
				t.Fatalf("verdict mismatch: shim refuses=%v (exit %d: %s); Decide=%s (proceed=%v)",
					gotRefuse, exit, out, want.Verdict, want.Verdict.Proceed())
			}
			if wantRefuse {
				if !strings.Contains(out, "denied by entry") || !strings.Contains(out, want.Entry) {
					t.Fatalf("the shim refusal must name the deny entry %q Decide named: %s", want.Entry, out)
				}
				if _, err := os.Stat(tc.target); err != nil {
					t.Fatalf("a path Decide DENIES was deleted: %v", err)
				}
				return
			}
			if _, err := os.Stat(tc.target); !os.IsNotExist(err) {
				t.Fatalf("a path Decide proceeds on was not deleted (the shim refused it): %s", out)
			}
		})
	}
}
