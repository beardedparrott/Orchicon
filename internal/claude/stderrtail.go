package claude

// stderrtail.go — THE LAST STDERR LINES, CAPTURED CONTINUOUSLY.
//
// REPORTED: six executions, 0 tokens each, ~280ms apart, every one reported as
//
//	claude session exited without a terminal result message (exit code 1)
//
// with NOTHING else. The CLI had written to stderr at exit — most likely that its credentials were missing —
// and the adapter discarded it for two compounding reasons:
//
//  1. the drain ran ON DEMAND, once, when the failure was already being reported. By then the pump goroutine
//     owned the stream, and the 50ms window could close before the last lines arrived;
//  2. when it did read, it kept the FIRST 512 bytes. A fatal diagnostic is the LAST thing a program prints,
//     so the capture was systematically biased towards the banner and away from the cause.
//
// There is a third, quieter problem: the stderr channel is buffered at 64 lines and its pump BLOCKS when full.
// An undrained stream therefore backs up into the child's own write to fd 2.
//
// So the drain runs for the whole session and keeps a bounded ring of the most recent lines.

import (
	"strings"
	"sync"
	"time"
)

// timeAfterStderrDrain bounds how long stop waits for the pump. A var so a test can shorten it.
var timeAfterStderrDrain = func() <-chan time.Time { return time.After(250 * time.Millisecond) }

// stderrRingMaxLines bounds the retained history. Enough for a stack trace's tail, small enough to keep in
// memory per live execution.
const stderrRingMaxLines = 200

// stderrRing is a fixed-size ring of the most recent stderr lines.
type stderrRing struct {
	mu     sync.Mutex
	lines  []string
	closed bool
	done   chan struct{}
}

// startStderrDrain consumes ch until it closes, retaining the last stderrRingMaxLines lines.
//
// It must be called with the proc's channel and must be stopped (stop) before the proc is closed, or the
// goroutine outlives the session.
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

// last returns the retained lines, oldest first. It never blocks: the ring is already populated, which is the
// whole point of draining continuously.
func (r *stderrRing) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// stop waits briefly for the pump to finish after the stream closed, then returns. The bounded wait matters:
// a stuck pump must not hold a finishing execution open.
func (r *stderrRing) stop() {
	select {
	case <-r.done:
	case <-timeAfterStderrDrain():
	}
}
