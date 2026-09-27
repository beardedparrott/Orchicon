package askorchicon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandPathsFindsLiteralPaths: the operator chose "also judge the paths a
// command mentions", and the case that prompted it is a literal path in the text.
func TestCommandPathsFindsLiteralPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"a read outside", "ls /tmp/x", []string{"/tmp/x"}},
		{"a copy from home to etc", "cp ~/secret /etc/x", []string{filepath.Join(home, "secret"), "/etc/x"}},
		{"a flag value", "git --file=/etc/passwd status", []string{"/etc/passwd"}},
		{"a redirect target", "echo hi > /etc/motd", []string{"/etc/motd"}},
		{"grouping and separators", "(cd /opt && ls /var/log)", []string{"/opt", "/var/log"}},
		{"quoted", `cat "/etc/hosts"`, []string{"/etc/hosts"}},
		// A path inside a substitution is STILL a literal path the command reads —
		// finding it is the point, not a false positive. (An earlier version of this
		// test asserted the opposite and was wrong: the inner path is right there in
		// the text.)
		{"inside a command substitution", "cat $(cat /tmp/cfg)", []string{"/tmp/cfg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandPaths(tc.cmd)
			if len(got) != len(tc.want) {
				t.Fatalf("commandPaths(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("commandPaths(%q)[%d] = %q, want %q", tc.cmd, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCommandPathsIgnoresNonPaths is the CONTROL, and it matters more than the
// positive cases: a false POSITIVE would put a card in front of the operator for an
// ordinary command, and enough of those make the gate something to click through.
func TestCommandPathsIgnoresNonPaths(t *testing.T) {
	for _, cmd := range []string{
		"",
		"   ",
		"go test ./...",                   // relative
		"ls -la",                          // no path at all
		"grep -rn foo ./internal",         // relative
		"curl https://example.com/x",      // a URL, not a path
		"curl --url=//host/x",             // protocol-relative: still not a filesystem path
		"rm -rf /",                        // the root alone is covered by the cwd entry
		"echo $HOME/x",                    // a variable: invisible to a static scan
		"cat $CFG",                        // ditto
		"ls \"$DIR\"",                     // ditto, quoted
		"for f in *; do test -f $f; done", // nothing absolute at all
	} {
		if got := commandPaths(cmd); len(got) != 0 {
			t.Errorf("commandPaths(%q) = %v, want none", cmd, got)
		}
	}
}

// TestDecisionTargetsJudgesAPathTheCommandNames is the operator's decision end to
// end: a command run in a granted directory that touches a path OUTSIDE it must be
// judged on that path too, or one cwd grant becomes blanket shell access.
func TestDecisionTargetsJudgesAPathTheCommandNames(t *testing.T) {
	targets := decisionTargets(
		askAction{Tool: "bash", Command: "cp ./x /etc/y"},
		"/p/proj",
	)
	// The cwd first (it is the grant key), then the path the command names.
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want the cwd and the named path", targets)
	}
	if targets[0].abstarget != "/p/proj" || targets[0].key != "/p/proj" {
		t.Errorf("first target = %+v, want the cwd with itself as the key", targets[0])
	}
	if targets[1].abstarget != "/etc/y" || targets[1].key != "/etc" {
		t.Errorf("second target = %+v, want /etc/y keyed on its directory", targets[1])
	}
}

// TestDecisionTargetsDoesNotDuplicateTheCwd: a command naming its own cwd must not
// produce the same target twice — the loop judges every target, so a duplicate would
// double-count a verdict (and a card would name it twice).
func TestDecisionTargetsDoesNotDuplicateTheCwd(t *testing.T) {
	targets := decisionTargets(
		askAction{Tool: "bash", Command: "ls /p/proj"},
		"/p/proj",
	)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want just the cwd", targets)
	}
}

// TestDecideAsksForACommandTouchingOutsideEvenWhenTheCwdIsGranted is the security
// property the operator asked for, asserted through the real decision path: with a
// session grant on the project directory, an ordinary command proceeds — and the
// same command naming a path OUTSIDE the project ASKS.
func TestDecideAsksForACommandTouchingOutsideEvenWhenTheCwdIsGranted(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	svc.grants.Grant("conv-1", "/p/proj") // the cwd grant the operator would give

	// Ordinary command in the granted cwd: silent.
	resp, ask, refusal := ct.decide(context.Background(), "ses_1",
		mcpAskEvent("per_1", "orchicon_bash", map[string]any{"command": "go test ./..."}))
	if resp != "once" || ask != nil || refusal != "" {
		t.Fatalf("granted cwd, ordinary command: resp=%q ask=%v refusal=%q", resp, ask, refusal)
	}

	// The SAME cwd, but the command names a path outside every grant: it must ASK.
	resp, ask, refusal = ct.decide(context.Background(), "ses_1",
		mcpAskEvent("per_2", "orchicon_bash", map[string]any{"command": "cp ./x /etc/y"}))
	if resp != "" || ask == nil || refusal != "" {
		t.Fatalf("a granted cwd must NOT cover a path the command names outside it: resp=%q ask=%v refusal=%q", resp, ask, refusal)
	}
	if !strings.Contains(ask.Summary, "/etc/y") {
		t.Errorf("the card must name the outside path it is asking about, got %q", ask.Summary)
	}
}
