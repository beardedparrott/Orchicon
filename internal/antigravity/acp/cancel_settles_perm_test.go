package acp

// cancel_settles_perm_test.go — Cancel() must return with the outstanding permission ANSWERED.
//
// THE BUG THIS PINS, because it is invisible in a single run. Cancelling only WAKES the permission
// handler; the `{"outcome":"cancelled"}` answer is written afterwards, by the goroutine that owns
// the request. So a caller that cancels and then inspects the wire is racing that write, and the
// loser sees a request agy is still waiting on. Measured: TestReplay_CancelMidturn passed 12/12
// when the package ran alone and failed inside the full `go test ./internal/... ./cmd/...` gate,
// where scheduling is far less friendly — a one-in-a-gate flake that would have been dismissed as
// "load" if it had not been reproduced against the gate output.
//
// THE FIX IS A HANDSHAKE, NOT AN ORDERING. `inflightPerm.settled` is closed by serveRequest AFTER
// `c.respond` writes the answer, and cancelPerms waits on it (bounded by permSettleGrace). The wire
// ORDER is unchanged and still the recording's: session/cancel first, then the answer
// (cancel_midturn.jsonl).
//
// These tests drive a real session over the fake child (startSessionOn does the handshake), so they
// assert the transport invariant directly instead of leaning on where an answer falls in a fixture.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// parkedPermission raises one permission request on a live session and waits until the transport has
// parked it — i.e. until a Cancel would actually have something to settle.
func parkedPermission(t *testing.T, c *Client, fp *fakeProc, sid string) {
	t.Helper()
	fp.emit(permissionRequest(900, sid, "run_command"))
	waitFor(t, 5*time.Second, func() bool {
		c.conn.permMu.Lock()
		defer c.conn.permMu.Unlock()
		return len(c.conn.perms[sid]) > 0
	})
}

// TestCancelAnswersAParkedPermissionBeforeReturning is the invariant on its own: a permission
// handler that IGNORES its context is parked, the turn is cancelled, and the answer must already be
// written when Cancel returns.
//
// THE STUBBORN HANDLER IS THE POINT. A well-behaved handler returns on ctx.Done(), so its answer
// would land quickly whether or not the transport waits — the test would pass for the wrong reason
// and the race would survive. stubbornHandler cannot answer on its own, so only the transport's
// settle handshake can make the assertion hold the instant Cancel returns.
func TestCancelAnswersAParkedPermissionBeforeReturning(t *testing.T) {
	const sid = "sess-cancel-settles"
	c, fp, sess, _ := startSessionOn(t, &stubbornHandler{}, sid)

	parkedPermission(t, c, fp, sid)

	if err := sess.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// NO sleep and NO retry loop on purpose: a poll here would hide exactly the race this test
	// exists to catch. The assertion must hold the moment Cancel returns.
	if !answeredCancelled(t, fp) {
		t.Fatalf("Cancel returned with the outstanding permission UNANSWERED; writes: %s",
			string(joinDocs(fp.writtenDocs())))
	}
	// AND IT IS THE NEVER-ALLOW ANSWER: a torn-down turn must not grant the tool call.
	if answeredWith(t, fp, "allow") {
		t.Fatal("Cancel answered a permission request with allow")
	}
}

// TestCancelWithoutAPendingPermissionStillCancels is the negative control: the handshake must not
// turn an ordinary cancel into a wait. Nothing is parked, so cancelPerms has nothing to settle and
// Cancel returns immediately.
func TestCancelWithoutAPendingPermissionStillCancels(t *testing.T) {
	const sid = "sess-cancel-idle"
	_, fp, sess, _ := startSessionOn(t, &blockingHandler{gate: make(chan struct{})}, sid)

	done := make(chan error, 1)
	go func() { done <- sess.Cancel() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Cancel with nothing parked: %v", err)
		}
	case <-time.After(permSettleGrace + time.Second):
		t.Fatal("Cancel blocked on an empty permission set — the settle wait must be a no-op then")
	}
	if answeredCancelled(t, fp) {
		t.Fatal("Cancel answered a permission request that was never made")
	}
}

// stubbornHandler blocks and IGNORES its context, so it never answers on a cancel by itself. That is
// what forces the TRANSPORT to settle the request rather than relying on a well-behaved handler —
// and it is why an ordering-only fix cannot pass this test.
type stubbornHandler struct {
	mu      sync.Mutex
	entered chan struct{}
	once    sync.Once
}

func (h *stubbornHandler) RequestPermission(context.Context, PermissionRequest) (PermissionDecision, error) {
	h.mu.Lock()
	if h.entered == nil {
		h.entered = make(chan struct{})
	}
	ch := h.entered
	h.mu.Unlock()
	h.once.Do(func() { close(ch) })
	// Deliberately NOT select on ctx.Done(): the handler must not be the thing that answers.
	time.Sleep(30 * time.Second)
	return PermissionDecision{OptionID: "allow"}, nil
}
