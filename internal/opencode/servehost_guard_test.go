package opencode

import (
	"strings"
	"testing"
)

// servehost_guard_test.go — an unavailable OS guard must not disable Ask chat.
//
// REPORTED: "We got opencode and its models discoverable now, but when I try to submit a chat it says
// 'create conversation session: host opencode serve unavailable — Ask chat transport is disabled: host
// serve guard: guard: symlink sudo: … A required privilege is not held by the client.' This seems
// wrong. Why is there a sudo in windows?"
//
// It is wrong, and there is no sudo — that is a SYMLINK named `sudo`, the execution guard's
// interception mechanism, which cannot exist on an unelevated Windows token. The guard itself being
// unavailable is a legitimate platform limitation (internal/guard/platform_windows.go). THE DEFECT WAS
// THE SEVERITY: startOnce returned that error, so a defense-in-depth backstop being absent disabled the
// entire Ask chat transport. internal/runtime's supervisor and internal/askorchicon's bash path both
// warn and continue; the host serve was the one caller that disagreed.
//
// ASSERTED ON THE SOURCE, deliberately. Driving startOnce for real needs an opencode binary and a
// spawnable serve; the regression is a CONTROL-FLOW property — does this function return when the
// guard cannot be built — and control flow is exactly what a source assertion pins without a fixture
// that would test the fixture. Same approach as servehost_credentials_test.go.
func TestStartOnceDoesNotFailWhenTheGuardIsUnavailable(t *testing.T) {
	src := readServehostSource(t)

	i := strings.Index(src, "func (h *HostServe) startOnce(")
	if i < 0 {
		t.Fatal("startOnce is gone")
	}
	body := src[i:]
	// startOnce ends at the next top-level func; slicing at the first "\n}\n" would cut at the end of
	// an `if` block and silently narrow the window this test inspects.
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}

	// THE GUARD CONSTRUCTION, and what follows it.
	g := strings.Index(body, "guard.NewExecutionGuard(")
	if g < 0 {
		t.Fatal("startOnce no longer builds an execution guard — if the guard moved, this guard test " +
			"must move with it")
	}
	// The window between the call and the next statement is where the fatal `return` used to be.
	window := body[g:]
	if len(window) > 700 {
		window = window[:700]
	}
	for _, line := range strings.Split(window, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "return ") && strings.Contains(trimmed, "err") &&
			!strings.Contains(trimmed, "//") {
			// A `return` on the guard's error is the reported bug: it disables the serve, and with it
			// every Ask turn, over a backstop that is optional by design.
			if strings.Contains(trimmed, "guard") || strings.Contains(trimmed, "host serve") {
				t.Errorf("startOnce RETURNS on the guard failure (%q).\n"+
					"  An execution guard that cannot be built must be a WARNING and the serve must "+
					"start: returning here turns 'no OS-level backstop on this platform' into 'Ask "+
					"chat is disabled', which is the reported bug. opencode's own permission rules "+
					"still apply, so the serve is safe to run unguarded.", trimmed)
			}
		}
	}

	// AND THE DISABLED-PLATFORM CASE IS REPORTED, not passed over in silence. A run with no OS guard
	// and no line saying so is how an operator concludes the guard is protecting them.
	if !strings.Contains(window, "Enabled()") && !strings.Contains(window, "DisabledReason()") {
		t.Error("startOnce never inspects Enabled()/DisabledReason(), so a platform with no guard " +
			"starts the serve silently unguarded — the operator has no way to know the backstop is " +
			"absent")
	}
}
