package askorchicon

// chatlimit_test.go — REACTIVE CONTEXT-LIMIT RECOVERY, driven through the real collector loop.
//
// The operator's report, from a Windows box running the DeepSeek free model:
//
//	turn failed: Your request resulted in more tokens than this model's maximum context length allows.
//	Please reduce the length of the messages or run compaction.
//
// They were told to run a command the plane can run for itself. What must hold now:
//
//  1. the conversation COMPACTS and the SAME message is re-dispatched, so the operator gets an answer;
//  2. the recovered failure is NOT shown — no red bubble over a turn that worked (the durable system-role
//     notice in the transcript is the one visible trace the collapse leaves);
//  3. a failure that does NOT recover is reported VERBATIM, because its text names the real limit;
//  4. the retry happens ONCE. A loop against a hard limit is how a turn burns its whole reply window.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// limitFakeClient is a fake chat client that ALSO implements scheduler.ChatCompactor, recording each
// compaction so the test can prove it ran (and ran once).
type limitFakeClient struct {
	*fakeSessionClient
	compactions []scheduler.CompactConversationOpts
	compactErr  error
	// compacted is what the adapter reports. false models "nothing to collapse", which must NOT be
	// treated as recovery — retrying an unchanged history would only fail the same way.
	compacted bool
}

func (c *limitFakeClient) CompactConversationSession(_ context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	c.compactions = append(c.compactions, opts)
	if c.compactErr != nil {
		return scheduler.ChatCompaction{}, c.compactErr
	}
	return scheduler.ChatCompaction{Compacted: c.compacted, Detail: "compacted 40 messages into a summary"}, nil
}

// deepSeekLimitError is the exact phrasing from the operator's report.
const deepSeekLimitError = "Your request resulted in more tokens than this model's maximum context length allows. " +
	"Please reduce the length of the messages or run compaction."

// TestContextLimitRecoversByCompactingAndResending is the whole feature: the turn fails on the limit, the
// conversation compacts, and the same user message is sent again and answered.
func TestContextLimitRecoversByCompactingAndResending(t *testing.T) {
	client := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compacted: true}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4.1-flash-free", userMsg: "keep going",
	}

	go func() {
		// First send: the model reports the context-limit overflow.
		waitForSend(t, client.fakeSessionClient, 1)
		client.sub.feed(busSessionError("ses_live", deepSeekLimitError))
		// Then the retry must happen and be answered.
		waitForSend(t, client.fakeSessionClient, 2)
		client.sub.feed(busText("ses_live", "carrying on"))
		client.sub.feed(busIdle("ses_live"))
	}()

	reply, _, sid, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("the turn must recover, not fail: %v", err)
	}
	if reply != "carrying on" {
		t.Errorf("reply = %q, want the retry's answer %q", reply, "carrying on")
	}
	if sid != "ses_live" {
		t.Errorf("session = %q, want ses_live (compaction is in place on the same session)", sid)
	}
	if len(client.compactions) != 1 {
		t.Fatalf("compactions = %d, want exactly 1", len(client.compactions))
	}
	if got := client.compactions[0].Reason; got != contextLimitReason {
		t.Errorf("compaction reason = %q, want %q — the notice text and the audit row key off it", got, contextLimitReason)
	}
	// THE SAME MESSAGE IS RESENT, not a resume of a half-consumed turn: the conversation the operator
	// asked a question of is the conversation that answers.
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.sendCalls) < 2 {
		t.Fatalf("send calls = %d, want the message re-dispatched", len(client.sendCalls))
	}
	for i, call := range client.sendCalls[:2] {
		if call.text != "keep going" {
			t.Errorf("send %d text = %q, want the operator's own message %q", i, call.text, "keep going")
		}
	}
}

// TestContextLimitFailureIsReportedWhenCompactionCannotRecover pins the other half: a non-recovering failure
// reaches the operator VERBATIM. Swallowing it would leave a dead turn with no explanation at all, and the
// original text is what tells them the real limit.
func TestContextLimitFailureIsReportedWhenCompactionCannotRecover(t *testing.T) {
	cases := []struct {
		name string
		// client returns the chat client AND, for the compacting cases, the handle the assertions need.
		// It is not typed to *limitFakeClient because the "cannot compact" arm must be a client that
		// genuinely lacks scheduler.ChatCompactor — a limitFakeClient always has it, so using one there
		// would silently test nothing.
		client func() (scheduler.ChatTurnClient, *fakeSessionClient, func() int)
	}{
		{"compaction errors", func() (scheduler.ChatTurnClient, *fakeSessionClient, func() int) {
			c := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compactErr: errors.New("serve unavailable")}
			return c, c.fakeSessionClient, func() int { return len(c.compactions) }
		}},
		{"nothing to collapse", func() (scheduler.ChatTurnClient, *fakeSessionClient, func() int) {
			c := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compacted: false}
			return c, c.fakeSessionClient, func() int { return len(c.compactions) }
		}},
		{"adapter cannot compact", func() (scheduler.ChatTurnClient, *fakeSessionClient, func() int) {
			// The plain fake implements ChatTurnClient and NOTHING else: no ChatCompactor. This is the
			// arm that must say "the adapter does not support compaction" internally and still hand the
			// operator the PROVIDER'S text, which names the real limit.
			f := &fakeSessionClient{}
			return f, f, func() int { return 0 }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, fake, compactions := c.client()
			opts := turnCollectOpts{
				client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
				modelRef: "opencode/deepseek-v4.1-flash-free", userMsg: "keep going",
			}
			go func() {
				waitForSend(t, fake, 1)
				fake.sub.feed(busSessionError("ses_live", deepSeekLimitError))
			}()
			_, _, _, err := collectTurn(t, client, opts)
			if err == nil {
				t.Fatal("a non-recovering context-limit failure must be reported, not swallowed")
			}
			if !strings.Contains(err.Error(), "maximum context length") {
				t.Errorf("error = %q, want the provider's own text (it names the real limit)", err)
			}
			if n := compactions(); n > 1 {
				t.Errorf("compactions = %d, want at most 1 (a loop against a hard limit burns the reply window)", n)
			}
		})
	}
}

// TestContextLimitRetriesOnlyOnce pins the budget directly: the SECOND limit failure on the same turn must
// reach the operator rather than compacting again.
func TestContextLimitRetriesOnlyOnce(t *testing.T) {
	client := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compacted: true}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4.1-flash-free", userMsg: "keep going",
	}
	go func() {
		waitForSend(t, client.fakeSessionClient, 1)
		client.sub.feed(busSessionError("ses_live", deepSeekLimitError))
		// The retry hits the SAME limit. There is no third attempt to wait for.
		waitForSend(t, client.fakeSessionClient, 2)
		client.sub.feed(busSessionError("ses_live", deepSeekLimitError))
	}()
	_, _, _, err := collectTurn(t, client, opts)
	if err == nil {
		t.Fatal("a second context-limit failure must be reported")
	}
	if len(client.compactions) != 1 {
		t.Errorf("compactions = %d, want exactly 1 — re-compacting an already-compacted conversation costs "+
			"detail and proves nothing", len(client.compactions))
	}
}

// TestNonLimitFailuresAreNotCompacted is the false-positive guard. Compaction is LOSSY, so an unrelated
// failure (auth, rate limit, a dead serve) must never collapse the operator's history looking for a remedy.
func TestNonLimitFailuresAreNotCompacted(t *testing.T) {
	for _, msg := range []string{
		"provider status 401 unauthorized",
		"rate limit exceeded — try again later",
		"connection refused",
		"the serve restarted",
	} {
		t.Run(msg, func(t *testing.T) {
			client := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compacted: true}
			opts := turnCollectOpts{
				client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
				modelRef: "opencode/deepseek-v4.1-flash-free", userMsg: "hello",
			}
			go func() {
				waitForSend(t, client.fakeSessionClient, 1)
				client.sub.feed(busSessionError("ses_live", msg))
			}()
			_, _, _, err := collectTurn(t, client, opts)
			if err == nil {
				t.Fatalf("error must surface for %q", msg)
			}
			if len(client.compactions) != 0 {
				t.Errorf("compactions = %d, want 0 — an unrelated failure must not collapse the history", len(client.compactions))
			}
		})
	}
}

// TestContextLimitClassifierUsesTheSharedList pins the seam: the Ask service must classify by the ONE list
// internal/orchicon maintains, or the 413 and provider-specific phrasings drift apart.
func TestContextLimitClassifierUsesTheSharedList(t *testing.T) {
	for _, c := range []string{
		deepSeekLimitError,
		"orchicon bridge: start Ask turn: provider status 400 ... maximum context length is 1048576 tokens",
		"provider status 413 413 Request Entity Too Large: <html>...413 Payload Too Large...",
		"anthropic: prompt is too long: 210000 tokens > 200000 maximum",
	} {
		if !isContextLimitFailure(errors.New(c)) {
			t.Errorf("must classify as a context-limit failure: %s", c)
		}
	}
	for _, c := range []string{"", "provider status 401 unauthorized", "rate limit exceeded"} {
		if isContextLimitFailure(errors.New(c)) {
			t.Errorf("must NOT classify as a context-limit failure: %s", c)
		}
	}
	if isContextLimitFailure(nil) {
		t.Error("nil must not be a context-limit failure")
	}
}

// TestCollectTurnBudgetIsNotSpentByTheRecovery is a light guard that the turn still terminates promptly when
// recovery succeeds — i.e. the retry does not restart the reply window.
func TestCollectTurnBudgetIsNotSpentByTheRecovery(t *testing.T) {
	client := &limitFakeClient{fakeSessionClient: &fakeSessionClient{}, compacted: true}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4.1-flash-free", userMsg: "keep going",
	}
	go func() {
		waitForSend(t, client.fakeSessionClient, 1)
		client.sub.feed(busSessionError("ses_live", deepSeekLimitError))
		waitForSend(t, client.fakeSessionClient, 2)
		time.Sleep(10 * time.Millisecond)
		client.sub.feed(busText("ses_live", "ok"))
		client.sub.feed(busIdle("ses_live"))
	}()
	started := time.Now()
	reply, _, _, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if reply != "ok" {
		t.Errorf("reply = %q, want %q", reply, "ok")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("recovery took %s — it must not restart the reply window", elapsed)
	}
}
