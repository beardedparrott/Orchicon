package eventbus

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestJetStreamConstantMsgIDIsDeduplicated is the regression guard for the
// blocking finding that made the per-token execution.text outbox write load
// bearing: the direct publish used a CONSTANT JetStream MsgID
// ("direct:<exec>:<event-type>"), and the stream is created with
// Duplicates: 5m — so JetStream silently stored only the FIRST direct publish
// per execution per 5 minutes. Every later text chunk reached the GUI only via
// the outbox relay. Once the per-token outbox write is removed, a unique
// per-publish MsgID is what keeps live streaming working.
//
// The test boots a real nats-server (skipped when the binary is absent) with the
// exact stream config used in production (see NewNATSPublisher) and asserts:
//   - the same MsgID published twice is deduplicated (Duplicate=true, one stored)
//   - unique MsgIDs (the fix) are both stored, with distinct sequences.
func TestJetStreamConstantMsgIDIsDeduplicated(t *testing.T) {
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server not installed; skipping JetStream MsgID dedup behaviour test")
	}

	port := freePort(t)
	cmd := exec.Command(bin, "-js", "-p", strconv.Itoa(port), "-a", "127.0.0.1", "-sd", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	var nc *nats.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err = nats.Connect(url, nats.Name("orchicon-test"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server never became ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(nc.Close)

	ctx := context.Background()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// Identical to NewNATSPublisher's stream config.
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       "ORCHICON_EVENTS",
		Subjects:   []string{"orchicon.events.>"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		Discard:    jetstream.DiscardOld,
		MaxAge:     72 * time.Hour,
		Duplicates: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	const subject = "orchicon.events.execution.execution.text"

	// The OLD behaviour: one constant MsgID per (execution, event type).
	first, err := js.Publish(ctx, subject, []byte(`{"chunk":0}`), jetstream.WithMsgID("direct:exec-1:execution.text"))
	if err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first publish must not be reported as a duplicate")
	}
	second, err := js.Publish(ctx, subject, []byte(`{"chunk":1}`), jetstream.WithMsgID("direct:exec-1:execution.text"))
	if err != nil {
		t.Fatalf("publish second: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("a repeated constant MsgID must be deduplicated by JetStream (Duplicates: 5m); live text streaming would silently stall otherwise")
	}

	// The FIX: a unique MsgID per publish (direct:<exec>:<type>:<seq>) stores
	// every chunk.
	third, err := js.Publish(ctx, subject, []byte(`{"chunk":2}`), jetstream.WithMsgID("direct:exec-1:execution.text:1"))
	if err != nil {
		t.Fatalf("publish third: %v", err)
	}
	fourth, err := js.Publish(ctx, subject, []byte(`{"chunk":3}`), jetstream.WithMsgID("direct:exec-1:execution.text:2"))
	if err != nil {
		t.Fatalf("publish fourth: %v", err)
	}
	if third.Duplicate || fourth.Duplicate {
		t.Fatalf("unique per-publish MsgIDs must never be deduplicated (third=%v fourth=%v)", third.Duplicate, fourth.Duplicate)
	}
	if third.Sequence == fourth.Sequence {
		t.Fatalf("unique MsgIDs must land on distinct stream sequences, both were %d", third.Sequence)
	}
}

// freePort reserves an ephemeral localhost TCP port and releases it for the
// child process to bind.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
