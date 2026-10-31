package claude

// stderrtail_test.go — THE DIAGNOSTIC IS THE LAST LINE, NOT THE FIRST.
//
// Reported as six executions of "claude session exited without a terminal result message (exit code 1)" with
// no further detail, while the CLI had written the actual cause to stderr at exit. The old capture kept the
// FIRST 512 bytes and read them on demand, so it systematically missed the fatal line.

import (
	"strings"
	"testing"
)

// TestTheTailKeepsTheLastLines — the bias that lost the diagnostic.
func TestTheTailKeepsTheLastLines(t *testing.T) {
	ch := make(chan []byte, 8)
	r := startStderrDrain(ch)
	// More lines than the ring holds, so eviction is exercised.
	for i := 0; i < stderrRingMaxLines+50; i++ {
		ch <- []byte("filler line")
	}
	ch <- []byte("Error: no credentials found — run `claude login`")
	close(ch)
	r.stop()

	got := r.last()
	if len(got) != stderrRingMaxLines {
		t.Fatalf("ring retained %d lines, want %d", len(got), stderrRingMaxLines)
	}
	if !strings.Contains(got[len(got)-1], "no credentials found") {
		t.Errorf("the LAST retained line is %q — the fatal diagnostic must be the one that survives",
			got[len(got)-1])
	}
	for _, line := range got {
		if strings.Contains(line, "filler") {
			// Expected in the ring; the assertion above is what matters.
			break
		}
	}
}

// TestTheDrainDoesNotBlockTheWriter — the child's fd 2 must never back up on an undrained channel.
//
// The pump channel is buffered at 64; without a drain the 65th write blocks the pump, which blocks the child.
func TestTheDrainDoesNotBlockTheWriter(t *testing.T) {
	ch := make(chan []byte, 4)
	r := startStderrDrain(ch)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			ch <- []byte("line")
		}
		close(ch)
		close(done)
	}()
	<-done
	r.stop()
	if len(r.last()) == 0 {
		t.Fatal("the drain retained nothing")
	}
}

// TestNoStderrIsReportedAsSuch — silence must not be ambiguous.
func TestNoStderrIsReportedAsSuch(t *testing.T) {
	ch := make(chan []byte)
	r := startStderrDrain(ch)
	close(ch)
	r.stop()
	s := &session{stderrRing: r}
	got := s.stderrTail()
	if got == "" {
		t.Fatal("an empty stderr produced no message at all, so the operator cannot tell whether the CLI said " +
			"nothing or the adapter failed to capture it")
	}
	if !strings.Contains(got, "no stderr") {
		t.Errorf("an empty capture must say so plainly; got: %q", got)
	}
}

// TestStderrTailQuotesTheLines — the normal case.
func TestStderrTailQuotesTheLines(t *testing.T) {
	ch := make(chan []byte, 2)
	r := startStderrDrain(ch)
	ch <- []byte("Error: invalid API key")
	close(ch)
	r.stop()
	s := &session{stderrRing: r}
	if got := s.stderrTail(); !strings.Contains(got, "invalid API key") {
		t.Errorf("the tail dropped the message; got: %q", got)
	}
}
