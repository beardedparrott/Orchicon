package askorchicon

// handshake_retry_test.go — the plane must retry an un-accepted message itself.
//
// The operator: "this message 'turn failed: the opencode serve did not accept the message within 1m0s —
// please try again' Let's ensure we are doing our keep alive/retries as well. If not we should add them.
// Users should very rarely see these messages. Orchicon should be trying to handle these timeouts and
// other issues itself first. It's frustrating."
//
// They were right: a serve that has not accepted the message is usually still coming up (binding,
// replaying its session store) or busy, and BOTH resolve within one more window. The turn used to fail
// on the first timeout, making the operator do the retry work the loop could do.

import (
	"strings"
	"testing"
	"time"
)

// THERE IS A RETRY BUDGET, AND IT IS MORE THAN ONE ATTEMPT.
func TestHandshakeHasARetryBudget(t *testing.T) {
	t.Setenv("ORCHICON_ASK_HANDSHAKE_ATTEMPTS", "")
	if got := askHandshakeAttempts(); got <= 1 {
		t.Fatalf("askHandshakeAttempts() = %d. With no retry the FIRST un-accepted message ends the turn, "+
			"which is the 'did not accept the message within 1m0s — please try again' the operator "+
			"should almost never see.", got)
	}
}

// The budget is env-overridable, and a bad value does not win.
func TestHandshakeAttemptsOverride(t *testing.T) {
	t.Setenv("ORCHICON_ASK_HANDSHAKE_ATTEMPTS", "5")
	if got := askHandshakeAttempts(); got != 5 {
		t.Errorf("askHandshakeAttempts() = %d, want the override 5", got)
	}
	for _, bad := range []string{"0", "-2", "lots"} {
		t.Setenv("ORCHICON_ASK_HANDSHAKE_ATTEMPTS", bad)
		if got := askHandshakeAttempts(); got != defaultHandshakeAttempts {
			t.Errorf("%q gave %d; a non-positive or unparseable override must fall back to the default %d",
				bad, got, defaultHandshakeAttempts)
		}
	}
}

// THE RETRY MUST CANCEL THE IN-FLIGHT ATTEMPT BEFORE RE-DISPATCHING.
//
// Without it, attempt N can still be in flight when attempt N+1 succeeds, and the serve receives the
// SAME user message twice — a duplicate reply is worse than the timeout it avoids.
func TestHandshakeRetryCancelsBeforeRedispatch(t *testing.T) {
	src := readChatSource(t)
	cancel := indexOf(src, "cancelSend()\n\t\t\t\t\tsendCtx, cancelSend = context.WithCancel(subCtx)")
	if cancel < 0 {
		t.Fatal("the handshake retry does not cancel the previous attempt's context before re-dispatching, " +
			"so a slow attempt plus a retry can deliver the same message to the serve twice")
	}
	redispatch := indexOf(src[cancel:], "dispatchSend()")
	if redispatch < 0 {
		t.Error("the retry cancels but never re-dispatches")
	}
}

// AND THE FAILURE MUST NAME THE ATTEMPTS IT MADE, so an operator who does see it knows the plane tried.
func TestHandshakeFailureNamesTheAttempts(t *testing.T) {
	src := readChatSource(t)
	if !strings.Contains(src, "did not accept the message after %d attempt(s)") {
		t.Error("the failure does not report how many accept attempts were made, so it reads as if " +
			"Orchicon gave up immediately")
	}
}

// The handshake window itself is unchanged: it is the ACCEPT bound, not the turn bound.
func TestHandshakeWindowIsSixtySeconds(t *testing.T) {
	t.Setenv("ORCHICON_ASK_TIMEOUT", "")
	if got := askTimeout(); got != 60*time.Second {
		t.Errorf("askTimeout() = %s, want the 60s accept bound", got)
	}
}
