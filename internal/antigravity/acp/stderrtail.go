package acp

// stderrtail.go — THE LAST STDERR LINES, CAPTURED CONTINUOUSLY.
//
// This is the same pattern as internal/claude/stderrtail.go, duplicated rather than imported:
// internal/claude pulls the scheduler in, which an ACP client has no business depending on, and
// exporting the ring would be a refactor of another package's internals.
//
// The reasons it exists at all:
//
//  1. a fatal diagnostic is the LAST thing a program prints, so a capture biased to the front of
//     the stream shows the banner and not the cause;
//  2. the stderr channel is buffered and its pump BLOCKS when full, so an undrained stream backs
//     up into the child's own write to fd 2 and hangs the handshake.
//
// So the drain runs for the whole session and keeps a bounded ring of the most recent lines.

import (
	"strings"
	"sync"
	"time"
)

// stderrRingMaxLines bounds the retained history: enough for a stack trace's tail, small enough
// to keep per live session.
const stderrRingMaxLines = 200

// stderrStopGrace bounds how long stop waits for the pump after the stream closed. A var so a
// test can shorten it.
var stderrStopGrace = 250 * time.Millisecond

// stderrRing is a fixed-size ring of the most recent stderr lines.
type stderrRing struct {
	mu    sync.Mutex
	lines []string
	done  chan struct{}
}

// startStderrDrain consumes ch until it closes, retaining the last stderrRingMaxLines lines.
func startStderrDrain(ch <-chan []byte) *stderrRing {
	r := &stderrRing{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for line := range ch {
			r.push(strings.TrimRight(string(line), "\r\n"))
		}
	}()
	return r
}

func (r *stderrRing) push(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == stderrRingMaxLines {
		copy(r.lines, r.lines[1:])
		r.lines[len(r.lines)-1] = line
		return
	}
	r.lines = append(r.lines, line)
}

// Tail returns the retained lines, oldest first. It never blocks.
func (r *stderrRing) Tail() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// stop waits briefly for the pump to finish after the stream closed. The bounded wait matters: a
// stuck pump must not hold a finishing execution open.
func (r *stderrRing) stop() {
	if r == nil {
		return
	}
	select {
	case <-r.done:
	case <-time.After(stderrStopGrace):
	}
}
