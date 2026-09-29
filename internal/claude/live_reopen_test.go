package claude

// A LIVE, env-gated check of the reopen fix — the bug the operator reported.
//
// Gated because it costs real money (two tiny prompts). Run with:
//
//	ORCHICON_TEST_LIVE_CLAUDE=1 go test -run TestLiveAskReopenResumes -v ./internal/claude/
//
// The unit tests prove the FLAG CHOICE; only this proves the flag works against
// the real CLI, which is what the operator needs to be true.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// liveTestModel is the model a live test uses.
//
// IT DEFAULTS TO THE CHEAPEST AVAILABLE, and that is a correction: these tests
// exercise FLAG BEHAVIOUR (does an id resume, does a transcript exist), which no
// model capability contributes to. I originally hard-coded a frontier model, and
// several of my probes omitted --model entirely and silently ran on the account
// default (Opus) — including one that loaded 87 MCP tool definitions per call.
// That combination exhausted the operator's five-hour window.
//
// The live model IS overridable, because a smoke test against a specific model is
// occasionally what you want — but it is never the DEFAULT, so a careless run
// cannot be expensive.
func liveTestModel() string {
	if m := strings.TrimSpace(os.Getenv("ORCHICON_TEST_LIVE_CLAUDE_MODEL")); m != "" {
		return m
	}
	// Haiku: the cheapest tier in the managed catalog, and enough for a
	// one-word round trip.
	return "claude-haiku-4-5-20251001"
}

func TestLiveAskReopenResumes(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("set ORCHICON_TEST_LIVE_CLAUDE=1 (costs real money)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	askDir := filepath.Join(t.TempDir(), "ask")
	if err := os.MkdirAll(askDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	b := New(quietLogger())
	s := newAskSession(b, "live-reopen-"+uuid.NewString(), askDir)
	s.sid = uuid.NewString()
	s.model = liveTestModel()

	// TURN 1 — a fresh session. Our argv must CREATE it.
	flag1 := ""
	a1 := s.argv()
	for i, a := range a1 {
		if (a == "--session-id" || a == "--resume") && i+1 < len(a1) {
			flag1 = a
		}
	}
	if flag1 != "--session-id" {
		t.Fatalf("turn 1 flag = %q, want --session-id (no transcript yet)", flag1)
	}
	out1 := runLive(t, a1, askDir, "Remember the codeword PLATYPUS. Reply OK.")
	if !strings.Contains(out1, `"is_error":false`) {
		t.Fatalf("turn 1 failed: %s", truncateForLog(out1))
	}

	// Our own check must now see what the CLI wrote — the link between the
	// adapter's flag choice and the CLI's on-disk state.
	if !claudeSessionHasTranscript(s.sid) {
		t.Fatalf("the adapter cannot see the transcript the CLI just wrote under %s (projects dir %s)",
			home+"/.claude/projects", ClaudeProjectsDir())
	}

	// TURN 2 — the reopen. Our argv must now RESUME.
	a2 := s.argv()
	flag2 := ""
	for i, a := range a2 {
		if (a == "--session-id" || a == "--resume") && i+1 < len(a2) {
			flag2 = a
		}
	}
	if flag2 != "--resume" {
		t.Fatalf("turn 2 flag = %q, want --resume — this is the operator's bug", flag2)
	}
	out2 := runLive(t, a2, askDir, "What was the codeword? Reply with just the word.")
	if !strings.Contains(out2, `"is_error":false`) {
		t.Fatalf("turn 2 failed: %s", truncateForLog(out2))
	}
	if !strings.Contains(strings.ToUpper(out2), "PLATYPUS") {
		t.Fatalf("the resumed session did not carry its history (no PLATYPUS): %s", truncateForLog(out2))
	}
}

// The worker's equivalent: a continuation against a REAL prior transcript resumes
// and carries history; against a missing one it does not even try.
func TestLiveWorkerResume(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("set ORCHICON_TEST_LIVE_CLAUDE=1 (costs real money)")
	}
	dir := t.TempDir()
	sid := uuid.NewString()
	b := New(quietLogger())

	// Establish a transcript the way a first execution would. A worker does not
	// pin --session-id (the CLI assigns ids there), so the setup pins one
	// explicitly to have a known id to continue from.
	argv := []string{ClaudeBinaryPath(), "-p", "--input-format", "stream-json",
		"--output-format", "stream-json", "--verbose", "--session-id", sid, "--model", liveTestModel()}
	out1 := runLive(t, argv, dir, "Remember the codeword WALRUS. Reply OK.")
	if !strings.Contains(out1, `"is_error":false`) {
		t.Fatalf("worker turn 1 failed: %s", truncateForLog(out1))
	}
	if !claudeSessionHasTranscript(sid) {
		t.Fatal("the adapter cannot see the worker transcript")
	}

	// Now a CONTINUATION through the worker's path.
	cont := &session{b: b, resumeID: sid, model: liveTestModel()}
	out2 := runLive(t, cont.argv(), dir, "What was the codeword? Reply with just the word.")
	if !strings.Contains(out2, `"is_error":false`) {
		t.Fatalf("worker continuation failed: %s", truncateForLog(out2))
	}
	if !strings.Contains(strings.ToUpper(out2), "WALRUS") {
		t.Fatalf("the worker continuation did not carry history: %s", truncateForLog(out2))
	}

	// A continuation ALWAYS carries --resume, even with no local transcript: that
	// is the worker contract (see TestWorkerAlwaysResumesTheRecordedSession). The
	// CLI reports "No conversation found" and the execution fails VISIBLY, which
	// is the recovery system's cue — not something the adapter papers over.
	gone := &session{b: b, resumeID: uuid.NewString(), model: liveTestModel()}
	joined := strings.Join(gone.argv(), " ")
	if !strings.Contains(joined, "--resume") {
		t.Fatal("the worker argv dropped --resume; a continuation must re-attach its identity")
	}
}

func runLive(t *testing.T, argv []string, dir, prompt string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":` +
			strconv.Quote(prompt) + `}]}}` + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	body := string(out)
	// A RATE LIMIT IS NOT A TEST FAILURE. The CLI reports one as a normal-looking
	// result with is_error=true and a "session limit" message, plus a
	// rate_limit_event whose utilization is over 1 — and it exits non-zero. Failing
	// here would send the next person hunting a code bug that is not there (it did
	// exactly that to me, after my own probing exhausted the five-hour window).
	if isRateLimited(body) {
		t.Skipf("the account is rate-limited (a probe consumed the window); re-run after the reset — this is NOT a code failure.\n%s", truncateForLog(body))
	}
	if err != nil {
		t.Fatalf("live run failed: %v\nstderr: %s\nstdout: %s", err, stderr.String(), truncateForLog(body))
	}
	return body
}

// isRateLimited recognises the CLI's limit signal in a stream, so a live test can
// skip instead of failing.
func isRateLimited(stream string) bool {
	for _, line := range strings.Split(stream, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["type"] == "rate_limit_event" {
			if info, ok := m["rate_limit_info"].(map[string]any); ok {
				if s, _ := info["status"].(string); s == "rejected" {
					return true
				}
			}
		}
	}
	low := strings.ToLower(stream)
	return strings.Contains(low, "session limit") || strings.Contains(low, "hit your limit")
}

func truncateForLog(s string) string {
	if len(s) > 600 {
		return s[:600]
	}
	return s
}

// summarizeResult pulls the result field for a readable failure message.
func summarizeResult(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["type"] == "result" {
			b, _ := json.Marshal(m)
			return string(b)
		}
	}
	return "(no result line)"
}

// The rate-limit detector, pinned: this is what keeps a live test from reporting
// an account limit as a code regression.
func TestIsRateLimited(t *testing.T) {
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":1.01}}}}`
	if !isRateLimited(rejected) {
		t.Error("a rejected rate_limit_event was not recognised")
	}
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}`
	if isRateLimited(allowed) {
		t.Error("an ALLOWED rate_limit_event was read as a limit")
	}
	msg := `{"type":"result","is_error":true,"result":"You've hit your session limit · resets 2:50pm"}`
	if !isRateLimited(msg) {
		t.Error("the human-readable limit message was not recognised")
	}
	if isRateLimited(`{"type":"result","is_error":false,"result":"PONG"}`) {
		t.Error("a normal result was read as a limit")
	}
}

// A live test must not default to an expensive model, and must not leave the
// choice implicit. Pinned because the cost of getting this wrong is the
// operator's quota, not a failing assertion.
func TestLiveTestModelIsCheapByDefault(t *testing.T) {
	t.Setenv("ORCHICON_TEST_LIVE_CLAUDE_MODEL", "")
	got := liveTestModel()
	if !strings.Contains(got, "haiku") {
		t.Fatalf("liveTestModel() = %q, want the cheapest tier by default — a live test exercises flag behaviour, not model capability", got)
	}
	// Overridable, deliberately.
	t.Setenv("ORCHICON_TEST_LIVE_CLAUDE_MODEL", "claude-opus-5-5")
	if got := liveTestModel(); got != "claude-opus-5-5" {
		t.Fatalf("override ignored: got %q", got)
	}
}
