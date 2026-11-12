package chat

// poll_auth_stop_test.go — a dead credential must END the poll, not feed it.

import (
	"errors"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
)

func TestAuthFailureStopsTheTurnPoll(t *testing.T) {
	c := NewController(nil)
	c.state["conv-1"] = &convState{streaming: true}
	c.cmds = make(chan tea.Cmd, 8)

	c.markAuthFailure("conv-1", connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired token")))

	done := make(chan struct{})
	go func() {
		c.runTurnPoll("conv-1") // returns on the first tick when credentials are dead
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll kept running with a rejected credential — that is the loop that exhausted the " +
			"host's ephemeral ports")
	}
	// AND IT DID NOT ENQUEUE A SINGLE POLL on the way out.
	if n := len(c.cmds); n != 0 {
		t.Errorf("the poll pushed %d command(s) despite dead credentials", n)
	}
}

// TestTransientFailuresKeepPolling is the other half: stopping on EVERY error would strand a live turn on a
// blip (a plane restart, a dropped socket), which is a worse failure than the one being fixed.
func TestTransientFailuresKeepPolling(t *testing.T) {
	c := NewController(nil)
	c.state["conv-1"] = &convState{streaming: true}
	c.cmds = make(chan tea.Cmd, 8)

	c.markAuthFailure("conv-1", connect.NewError(connect.CodeUnavailable, errors.New("plane restarting")))

	c.mu.Lock()
	dead := c.state["conv-1"].authDead
	c.mu.Unlock()
	if dead {
		t.Fatal("a transient error parked the poll; only a 401 is permanent")
	}
}

// TestASuccessfulReadResumesPolling: the flag must be CLEARED by a working credential, or a refreshed token
// would leave the transcript frozen for the rest of the process's life.
func TestASuccessfulReadResumesPolling(t *testing.T) {
	c := NewController(nil)
	c.state["conv-1"] = &convState{streaming: true, authDead: true}

	c.markAuthOK("conv-1")
	c.mu.Lock()
	dead := c.state["conv-1"].authDead
	c.mu.Unlock()
	if dead {
		t.Error("a successful read did not clear authDead, so polling would never resume after a re-auth")
	}
}
