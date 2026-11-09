package askorchicon

// handshake_retry_test.go — the plane must retry an un-accepted message itself.

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
