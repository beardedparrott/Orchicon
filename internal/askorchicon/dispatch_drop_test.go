package askorchicon

// dispatch_drop_test.go — A DROPPED LIVE EVENT SAYS SO.
//
// The operator: "Live streaming is STILL not happening in a conversation." A live pane that sits on
// "thinking…" while the turn runs has two possible causes, and they were INDISTINGUISHABLE from
// outside: the turn produced nothing, or the turn produced plenty and every event was thrown away.
//
// The dispatch channel is what feeds the client's HTTP response stream AND the conversation's
// broadcast hub, so an event that does not fit is lost for the originating client and for every
// watcher. Dropping is deliberate — the alternative is parking the collector, and the reply still
// lands durably for the poll to resolve — but it used to happen with NO log, NO counter and NO way
// to tell the two causes apart. These tests pin the signal.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func newDropLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// A FULL BUFFER IS REPORTED, WITH THE COUNTER AND THE DRAIN'S STATE.
func TestADroppedEventIsReported(t *testing.T) {
	log, buf := newDropLogger()
	st := &dispatchStreamState{}
	st.drainActive.Store(true)

	ch := make(chan *apiv1.ChatStreamResponse, 1)
	ch <- &apiv1.ChatStreamResponse{} // fill it
	sendOrDrop(ch, &apiv1.ChatStreamResponse{}, st, log, "conv-1")

	if got := st.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1 — a full buffer must be counted", got)
	}
	out := buf.String()
	if !strings.Contains(out, "DROPPED live events") {
		t.Errorf("no drop was logged; a silent drop is exactly what made this un-diagnosable: %q", out)
	}
	for _, want := range []string{"conv-1", "drain_active=true", "dropped_total=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the drop log does not carry %q: %q", want, out)
		}
	}
}

// THE REASON IS CARRIED, because the two causes need different fixes: a slow reader (the drain is
// still running) is a back-pressure problem, while an ENDED drain (a dropped socket while the turn
// runs on) means every remaining event has nowhere to go.
func TestTheDropDistinguishesASlowReaderFromADeadDrain(t *testing.T) {
	for _, drainActive := range []bool{true, false} {
		log, buf := newDropLogger()
		st := &dispatchStreamState{}
		st.drainActive.Store(drainActive)

		ch := make(chan *apiv1.ChatStreamResponse, 1)
		ch <- &apiv1.ChatStreamResponse{}
		sendOrDrop(ch, &apiv1.ChatStreamResponse{}, st, log, "conv-1")

		want := "drain_active=false"
		if drainActive {
			want = "drain_active=true"
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("drainActive=%v: the log does not say %q: %q", drainActive, want, buf.String())
		}
	}
}

// THE LOG CANNOT BE A FIREHOSE. Every dropped token logging its own line would bury the plane's log
// and make the report useless — so the first drop is reported and then every maxDropReports, while
// the COUNTER keeps counting every one.
func TestDropsAreCountedInFullButLoggedSparingly(t *testing.T) {
	log, buf := newDropLogger()
	st := &dispatchStreamState{}
	st.drainActive.Store(true)
	ch := make(chan *apiv1.ChatStreamResponse, 1)
	ch <- &apiv1.ChatStreamResponse{} // stays full for the whole test

	const attempts = 500
	for i := 0; i < attempts; i++ {
		sendOrDrop(ch, &apiv1.ChatStreamResponse{}, st, log, "conv-1")
	}
	if got := st.dropped.Load(); got != attempts {
		t.Errorf("dropped = %d, want %d — the counter must record every drop, not every REPORTED one",
			got, attempts)
	}
	if lines := strings.Count(buf.String(), "DROPPED live events"); lines >= attempts {
		t.Errorf("%d drops produced %d log lines — one per drop would flood the log", attempts, lines)
	}
	if lines := strings.Count(buf.String(), "DROPPED live events"); lines == 0 {
		t.Error("500 drops produced no log line at all")
	}
}

// AND A DELIVERED EVENT IS NOT A DROP — the counter must not move when the buffer has room, or the
// signal would fire on a healthy turn.
func TestADeliveredEventIsNotCounted(t *testing.T) {
	log, buf := newDropLogger()
	st := &dispatchStreamState{}
	st.drainActive.Store(true)

	ch := make(chan *apiv1.ChatStreamResponse, 4)
	for i := 0; i < 4; i++ {
		sendOrDrop(ch, &apiv1.ChatStreamResponse{}, st, log, "conv-1")
	}
	if got := st.dropped.Load(); got != 0 {
		t.Errorf("dropped = %d after four events into a four-slot buffer, want 0", got)
	}
	if got := len(ch); got != 4 {
		t.Errorf("channel holds %d, want 4 — events were lost with room to spare", got)
	}
	if buf.Len() != 0 {
		t.Errorf("a healthy turn logged a drop: %q", buf.String())
	}
}
