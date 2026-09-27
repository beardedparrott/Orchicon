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

func TestCommandPathsDoesNotInventPaths(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "an awk regex is not a path",
			cmd:  `cd /p/proj && awk 'NR>=1400 && NR<=1505 && /^func /{print NR": "$0}' internal/x.go`,
			want: []string{"/p/proj"},
		},
		{
			name: "an awk regex with no metacharacter but one segment is not a path",
			cmd:  `cd /p/proj && awk 'NR>=1618 && /return turnAttemptResult/{print}' internal/x.go`,
			want: []string{"/p/proj"},
		},
		{
			name: "a slash-delimited grep pattern is not a path",
			cmd:  `cd /p/proj && grep -n '/func /' internal/x.go`,
			want: []string{"/p/proj"},
		},
		{
			name: "a bracketed sed program is not a path",
			cmd:  `cd /p/proj && sed -e '/^[a-z]/,/end/p' internal/x.go`,
			want: []string{"/p/proj"},
		},
		{
			name: "a redirect to the null device is not a path",
			cmd:  `cd /p/proj && go test ./... 2>/dev/null`,
			want: []string{"/p/proj"},
		},
		{
			name: "a URL is not a path",
			cmd:  `cd /p/proj && curl https://host/x && echo ok`,
			want: []string{"/p/proj"},
		},
		{
			name: "the case this extraction exists for",
			cmd:  `cp ~/a /etc/b`,
			want: []string{"/home/beardedparrott/a", "/etc/b"},
		},
		{
			name: "a flag-carried path",
			cmd:  `tool --out=/etc/passwd`,
			want: []string{"/etc/passwd"},
		},
		{
			name: "a redirect target OUTSIDE the project is a path",
			cmd:  `cd /p/proj && go build ./... > /tmp/out.log 2>&1`,
			want: []string{"/p/proj", "/tmp/out.log"},
		},
		{
			name: "a quoted path with a space survives",
			cmd:  `cd /p/proj && cp a "/tmp/my file.log"`,
			want: []string{"/p/proj", "/tmp/my file.log"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandPaths(tc.cmd)
			if len(got) != len(tc.want) {
				t.Fatalf("commandPaths = %v, want %v\n  cmd: %s", got, tc.want, tc.cmd)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("commandPaths = %v, want %v\n  cmd: %s", got, tc.want, tc.cmd)
				}
			}
		})
	}
}

// TestCommandPathsIgnoresHeredocBodies — a heredoc body is a FILE'S CONTENT, not arguments.
//
// Judging it meant that writing a test file which mentioned /tmp raised a consent ask about
// /tmp: the content was read as though the command were touching those paths.

func TestCommandPathsIgnoresHeredocBodies(t *testing.T) {
	cmd := "cd /p/proj && cat > internal/zz_test.go <<'GOEOF'\n" +
		"package x\n\n" +
		"var a = \"/tmp/dev\"\n" +
		"var b = `cp ~/a /etc/b`\n" +
		"// see /etc/passwd for details\n" +
		"GOEOF\n" +
		"echo done"
	got := commandPaths(cmd)
	if len(got) != 1 || got[0] != "/p/proj" {
		t.Fatalf("commandPaths = %v, want just the cwd — a heredoc body is file CONTENT, not an argument", got)
	}

	// BUT THE HEREDOC'S REDIRECT TARGET IS STILL JUDGED: writing the file is the action.
	cmd2 := "cat > /tmp/escape.txt <<'EOF'\nhello\nEOF\necho done"
	got2 := commandPaths(cmd2)
	if len(got2) != 1 || got2[0] != "/tmp/escape.txt" {
		t.Fatalf("commandPaths = %v, want the heredoc's redirect target", got2)
	}
}

// TestShellMetacharAndPathShape pins the two predicates the extraction leans on, at their
// boundaries, so a loosening edit is a failing test rather than a silent return of the
// treadmill.

func TestShellMetacharAndPathShape(t *testing.T) {
	for _, tok := range []string{"/^func", "/a*b", "/a?b", "/a[b]", "/a{b}", "/a(b)", "/a|b", `/a\b`, "/a$b", "/a+b", "/a!b", "/a,b"} {
		if !hasShellMetachar(tok) {
			t.Errorf("hasShellMetachar(%q) = false — a pattern must not be read as a path", tok)
		}
	}
	for _, tok := range []string{"/etc/b", "/tmp/out.log", "/a-b_c.d", "/a b/c"} {
		if hasShellMetachar(tok) {
			t.Errorf("hasShellMetachar(%q) = true — an ordinary path", tok)
		}
	}
	// A QUOTED token must look like a path: two segments, no trailing slash.
	for _, tok := range []string{"/return", "/func", "/func /", "/", "return"} {
		if looksLikePath(tok) {
			t.Errorf("looksLikePath(%q) = true — a quoted span that is not a path", tok)
		}
	}
	for _, tok := range []string{"/etc/b", "~/a", "/tmp/orchicon-develop", "/a/b/c"} {
		if !looksLikePath(tok) {
			t.Errorf("looksLikePath(%q) = false — an ordinary quoted path", tok)
		}
	}
}

// TestASessionGrantSilencesTheOperatorsRealCommands is the reported bug as a test.
//
// Every command here is one from the session whose ledger showed ask→grant→ask repeated.
// With the project directory granted, EVERY ONE must be silent, and before the fix every
// one of them asked.

func TestASessionGrantSilencesTheOperatorsRealCommands(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	const proj = "/home/beardedparrott/projects/Orchicon"
	ct := newTestConsentTurn(svc, proj, true, nil)
	svc.grants.Grant("conv-1", proj)

	for i, cmd := range []string{
		`cd /home/beardedparrott/projects/Orchicon && awk 'NR>=1400 && NR<=1505 && /^func /{print NR": "$0}' internal/askorchicon/chat.go && echo hi`,
		`cd /home/beardedparrott/projects/Orchicon && awk 'NR>=1618 && NR<=2230 && /return turnAttemptResult/{print NR": "$0}' internal/x.go`,
		`cd /home/beardedparrott/projects/Orchicon && sed -n '3480,3530p' internal/tui/app.go && echo "=== who calls this? ===" && awk 'N' internal/y.go`,
		`cd /home/beardedparrott/projects/Orchicon && grep -n '/func /' internal/x.go`,
		`cd /home/beardedparrott/projects/Orchicon && go test ./... 2>&1 | tail -5`,
		`cd /home/beardedparrott/projects/Orchicon && git status --short | head -5`,
	} {
		_, ask, _ := ct.decide(context.Background(), "ses_1", bashAskEvent("per_"+string(rune('a'+i)), cmd))
		if ask != nil {
			t.Errorf("ASKED AGAIN (card named %q) with the directory granted:\n  %s", ask.Directory, cmd)
		}
	}
}

// TestTheCardNamesTheDirectoryThatWouldSilenceIt is the OTHER half, and the one that makes
// "never ask again" true rather than a promise the card cannot keep.
//
// When a command's cwd is granted but a path it NAMES is not, the ask must offer the grant
// for THAT directory — not for the one the operator already gave. Otherwise the operator
// clears the card by granting the named directory and is asked again by the next identical
// command, with no way to see why.

func TestTheCardNamesTheDirectoryThatWouldSilenceIt(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	svc.grants.Grant("conv-1", "/p/proj")

	cmd := `cd /p/proj && git worktree remove --force /tmp/dev 2>/dev/null`

	_, ask, _ := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", cmd))
	if ask == nil {
		t.Fatal("expected an ask: /tmp is outside the granted project")
	}
	if ask.Directory != "/tmp" || ask.Key != "/tmp" {
		t.Fatalf("card names dir=%q key=%q, want /tmp — the grant it offers must be the one that works",
			ask.Directory, ask.Key)
	}
	if !strings.Contains(ask.Summary, "/tmp/dev") {
		t.Errorf("the card's summary must still name the ACTION (%q)", ask.Summary)
	}

	// Taking the offer silences the command — the property the operator was missing.
	svc.grants.Grant("conv-1", ask.Key)
	if _, again, _ := ct.decide(context.Background(), "ses_2", bashAskEvent("per_2", cmd)); again != nil {
		t.Fatalf("asked AGAIN (dir=%q) after granting the directory the card named", again.Directory)
	}
}

// A plain in-project ask is unchanged: nothing blocks, so the card names the scope dir, and
// one grant covers the whole directory — not just this command.

func TestAnOrdinaryInProjectAskStillNamesTheScopeDirectory(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	_, ask, _ := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", `cd /p/proj && go test ./...`))
	if ask == nil {
		t.Fatal("expected an ask with nothing granted")
	}
	if ask.Directory != "/p/proj" {
		t.Fatalf("card names %q, want the scope directory", ask.Directory)
	}
	svc.grants.Grant("conv-1", ask.Key)
	// The grant covers the DIRECTORY, so a different command in it is silent too.
	if _, again, _ := ct.decide(context.Background(), "ses_2", bashAskEvent("per_2", `cd /p/proj && go build ./...`)); again != nil {
		t.Fatal("a grant for the directory must cover another command in it")
	}
}

// A heredoc write that MENTIONS paths in its content must not ask — the file's content is
// not an argument. This is the shape of nearly every test file written in this repo.

func TestAHeredocWriteIsJudgedByItsTargetNotItsContent(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	svc.grants.Grant("conv-1", "/p/proj")

	cmd := "cd /p/proj && cat > internal/zz_test.go <<'GOEOF'\n" +
		"package x\n\n" +
		"var a = \"/tmp/dev\"\n" +
		"var b = `cp ~/a /etc/b`\n" +
		"GOEOF\n" +
		"echo done"
	if _, ask, _ := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", cmd)); ask != nil {
		t.Fatalf("a heredoc's CONTENT raised an ask (card named %q) — content is not an argument", ask.Directory)
	}
}

// TestCommandPathsSkipsNodesThatAreNotTheOperatorsBusiness — the second round of the same
// bug, found in the LIVE plane's log rather than by reasoning.
//
// After the regex fix, two spurious asks remained, both from ordinary work:
//
//	ask native-ask-2  directory=/dev            <- curl -o /dev/null
//	ask native-ask-5  directory=/proc/1372221   <- strings /proc/<pid>/exe
//
// /dev/null is ubiquitous (`2>/dev/null`, `> /dev/null`, `curl -o /dev/null`) and reading a
// process's executable is introspection. Neither is a file the operator is being asked to
// protect, so both were pure friction: the operator granted the project directory and was
// then asked about /dev by a command that only discarded its stderr.
func TestCommandPathsSkipsNodesThatAreNotTheOperatorsBusiness(t *testing.T) {
	for _, cmd := range []string{
		`cd /p/proj && curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz`,
		`cd /p/proj && go test ./... > /dev/null 2>&1`,
		`cd /p/proj && strings /proc/1372221/exe | grep -q strip`,
		`cd /p/proj && cat /proc/self/status | head -3`,
		`cd /p/proj && readlink /proc/1234/cwd`,
		`cd /p/proj && echo hi > /dev/stdout`,
	} {
		got := commandPaths(cmd)
		if len(got) != 1 || got[0] != "/p/proj" {
			t.Errorf("commandPaths = %v, want only the cwd\n  cmd: %s", got, cmd)
		}
	}
}

// TestCommandPathsStillJudgesDANGEROUSNodes is the other side, and the reason the skip lists
// are EXPLICIT rather than prefixes. A blanket "skip /dev" would wave through the
// catastrophic writes this gate exists to stop, and a blanket /proc skip would wave through
// written kernel configuration and a direct write into another process's memory.
//
// Every case here must remain a consent target.
func TestCommandPathsStillJudgesDANGEROUSNodes(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{`rm -rf /dev/sda`, "/dev/sda"},
		{`dd if=/dev/zero of=/dev/nvme0n1`, "/dev/nvme0n1"},
		{`echo x > /proc/sys/kernel/panic`, "/proc/sys/kernel/panic"},
		{`echo x > /proc/1234/mem`, "/proc/1234/mem"},
		// A redirect THROUGH an fd writes into whatever the descriptor points at, and
		// commandPaths cannot tell a read from a write — so it assumes the dangerous case.
		{`echo x > /proc/1234/fd/5`, "/proc/1234/fd/5"},
		// The `..` escape is unchanged: an explicit traversal is always judged.
		{`ls /p/proj/../sibling`, "/p/sibling"},
	}
	for _, tc := range cases {
		got := commandPaths(tc.cmd)
		found := false
		for _, g := range got {
			if g == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("commandPaths(%q) = %v — %q must remain a consent target", tc.cmd, got, tc.want)
		}
	}
}

// TestTheSkipPredicatesAtTheirBoundaries pins the two predicates directly, so a loosening
// edit fails a test rather than silently opening /dev or /proc writing.
func TestTheSkipPredicatesAtTheirBoundaries(t *testing.T) {
	for _, tok := range []string{"/dev/null", "/dev/stdout", "/dev/stderr", "/dev/zero", "/dev/tty"} {
		if !harmlessDevicePath(tok) {
			t.Errorf("harmlessDevicePath(%q) = false — an ordinary sink", tok)
		}
	}
	for _, tok := range []string{"/dev/sda", "/dev/nvme0n1", "/dev/mapper/vg-root", "/dev", "/dev/disk/by-id/x"} {
		if harmlessDevicePath(tok) {
			t.Errorf("harmlessDevicePath(%q) = true — a DEVICE with state must be judged", tok)
		}
	}
	for _, tok := range []string{"/proc/self/status", "/proc/thread-self/cmdline", "/proc/1234/exe", "/proc/1234/maps", "/proc/1/environ"} {
		if !procIntrospectionPath(tok) {
			t.Errorf("procIntrospectionPath(%q) = false — read-only process metadata", tok)
		}
	}
	for _, tok := range []string{
		"/proc/sys/kernel/panic", "/proc/1234/mem", "/proc/1234/fd/5",
		"/proc", "/proc/", "/proc/notanumber/exe", "/proc/1234/", "/sys/kernel/x",
	} {
		if procIntrospectionPath(tok) {
			t.Errorf("procIntrospectionPath(%q) = true — writable or not introspection", tok)
		}
	}
}
