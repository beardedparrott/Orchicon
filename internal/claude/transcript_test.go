package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTranscript writes a plausible transcript for sid under a project dir, so a
// test can drive the disk-dependent flag choice without the real CLI.
func fakeTranscript(t *testing.T, home, projectDir, sid string) {
	t.Helper()
	if sid == "" {
		return
	}
	dir := filepath.Join(home, ".claude", "projects", projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func TestClaudeSessionHasTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if claudeSessionHasTranscript("nope") {
		t.Fatal("reported a transcript that does not exist")
	}
	fakeTranscript(t, home, "-tmp-ask-conv", "sid-1")
	if !claudeSessionHasTranscript("sid-1") {
		t.Fatal("did not find a transcript that exists")
	}
	// ANY project directory counts: claude resumes by id across cwds (verified
	// against the real CLI), and the host's cwd differs from the container's.
	fakeTranscript(t, home, "-some-other-project", "sid-2")
	if !claudeSessionHasTranscript("sid-2") {
		t.Fatal("did not find a transcript in another project directory — resume is not cwd-scoped")
	}
	if claudeSessionHasTranscript("") {
		t.Fatal("an empty id must report no transcript")
	}
}

func argvFlagValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// THE BUG, pinned. Reopening a conversation must RESUME, not re-create: passing
// --session-id for an id that already exists makes the CLI exit 1 with an empty
// stdout, which the operator saw as the message being accepted and then nothing.
func TestAskArgvResumesAnExistingSessionInsteadOfRecreatingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	b := New(quietLogger())
	s := newAskSession(b, "conv-reopen", filepath.Join(t.TempDir(), "ask"))
	s.sid = "existing-sid"

	// (a) the transcript EXISTS → --resume
	fakeTranscript(t, home, "-tmp-ask-conv-reopen", "existing-sid")
	argv := s.argv()
	if got := argvFlagValue(t, argv, "--resume"); got != "existing-sid" {
		t.Fatalf("--resume = %q, want existing-sid (the reopened conversation)", got)
	}
	if got := argvFlagValue(t, argv, "--session-id"); got != "" {
		t.Fatalf("--session-id = %q on a session that exists; the CLI refuses this with an EMPTY stdout", got)
	}

	// (b) the transcript is GONE (a rebuild wiping ~/.claude, a wiped /tmp) → the
	// id is re-pinned as a NEW session, not resumed. --resume here would fail with
	// "No conversation found".
	s2 := newAskSession(b, "conv-fresh", filepath.Join(t.TempDir(), "ask"))
	s2.sid = "vanished-sid"
	argv2 := s2.argv()
	if got := argvFlagValue(t, argv2, "--session-id"); got != "vanished-sid" {
		t.Fatalf("--session-id = %q, want vanished-sid re-pinned as a new session", got)
	}
	if got := argvFlagValue(t, argv2, "--resume"); got != "" {
		t.Fatalf("--resume = %q for a session with no transcript; the CLI fails with 'No conversation found'", got)
	}
}

// A RESPAWN is not a new session. A child that died mid-conversation must resume
// the transcript it left behind — the same rule, reached from the other direction.
func TestAskArgvResumesAfterAChildDies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	b := New(quietLogger())
	s := newAskSession(b, "conv-respawn2", filepath.Join(t.TempDir(), "ask"))
	s.sid = "respawn-sid"

	// First spawn: no transcript yet → create.
	if got := argvFlagValue(t, s.argv(), "--session-id"); got != "respawn-sid" {
		t.Fatalf("first spawn flag = %q, want --session-id", got)
	}
	// The child ran and left a transcript; the child then died and we respawn.
	fakeTranscript(t, home, "-tmp-ask-conv-respawn2", "respawn-sid")
	if got := argvFlagValue(t, s.argv(), "--resume"); got != "respawn-sid" {
		t.Fatalf("respawn flag = %q, want --resume — a dead child is not a new conversation", got)
	}
}

// THE WORKER PATH DELIBERATELY DIFFERS FROM ASK, and this pins why.
//
// I first applied the Ask rule here too — drop `--resume` when no transcript
// exists — and three existing tests failed ("must re-attach the SAME session
// identity"). They were right:
//
//   - the CONTRACT is that a worker continuation re-attaches the same session, so
//     a silent fresh start would drop the very history it exists to carry;
//   - a LOST SESSION IS THE RECOVERY SYSTEM'S problem (capture → summarize →
//     preserve → resume), and working around it here would bypass that and
//     quietly hand a worker a context-free run;
//   - the check could not be trusted anyway: a worker's child may run INSIDE A
//     RUNTIME CONTAINER, where the adapter's host-side view of ~/.claude and the
//     child's view can diverge. The Ask path is host-only, which is exactly why
//     the check is sound there and not here.
//
// So: ALWAYS resume, with or without a local transcript.
func TestWorkerAlwaysResumesTheRecordedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	for _, tc := range []struct {
		name string
		sid  string
	}{
		{"transcript present", "present-sid"},
		{"transcript ABSENT (a wiped ~/.claude, or a container whose view differs)", "absent-sid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "transcript present" {
				fakeTranscript(t, home, "-worktree", tc.sid)
			}
			s := &session{b: New(quietLogger()), resumeID: tc.sid}
			argv := s.argv()
			if got := argvFlagValue(t, argv, "--resume"); got != tc.sid {
				t.Fatalf("--resume = %q, want %q — a continuation must re-attach its identity", got, tc.sid)
			}
			// The worker must NOT pin an id: it never has, and doing so would make
			// the CLI refuse a live id ("already in use") the way the Ask path did.
			if got := argvFlagValue(t, argv, "--session-id"); got != "" {
				t.Fatalf("the worker pinned --session-id = %q", got)
			}
			// The rest of the launch shape survives.
			joined := strings.Join(argv, " ")
			for _, want := range []string{"-p", "--permission-mode", "--mcp-config"} {
				if !strings.Contains(joined, want) {
					t.Errorf("the worker argv lost %q", want)
				}
			}
		})
	}
}

// The failure the operator would otherwise have seen, spelled out: an empty
// stdout is what made this silent. Pinning it here so the flag choice is
// understood as load-bearing rather than cosmetic.
func TestAskPermissionsAndMCPRobustToFlagChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)
	fakeTranscript(t, home, "-x", "sid-x")

	b := New(quietLogger())
	s := newAskSession(b, "conv-x", filepath.Join(t.TempDir(), "ask"))
	s.sid = "sid-x"
	argv := s.argv()
	// The permission and MCP halves must be present whichever flag was chosen.
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--permission-prompts") {
		t.Error("the Ask argv lost --permission-prompts")
	}
	if !strings.Contains(joined, "--mcp-config") {
		t.Error("the Ask argv lost --mcp-config")
	}
	// And the flag must actually be a resume.
	var doc map[string]any
	if err := json.Unmarshal([]byte("{}"), &doc); err != nil {
		t.Fatal(err)
	}
	if got := argvFlagValue(t, argv, "--resume"); got != "sid-x" {
		t.Fatalf("--resume = %q, want sid-x", got)
	}
}
