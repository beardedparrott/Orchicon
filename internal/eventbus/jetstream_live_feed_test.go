package eventbus

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestDirectPublishLiveFeedDeliversEveryChunk is the end-to-end guard for the
// user-visible surface of the de-outboxed per-token execution.text path: with
// the outbox row gone, the ONLY delivery path for live streaming text is the
// direct publish consumed by StreamExecutionEvents (internal/execution
// Service.StreamExecutionEvents -> Subscriber.Subscribe). This test drives the
// real production publisher (NewNATSPublisher) and the real production
// subscriber (NewNATSSubscriber) against a real nats-server with the production
// stream config, and asserts that every streamed chunk published with the
// TaskReconciler's new unique MsgID (`direct:<exec>:<type>:<seq>`) reaches the
// consumer on a distinct JetStream sequence — i.e. the GUI's live feed renders
// every chunk, exactly as it did when the outbox relay was the live path.
//
// It also pins the failure mode the fix removed: the old constant MsgID
// (`direct:<exec>:<type>`) is silently deduplicated by the stream's
// Duplicates: 5m window, so without the unique suffix live streaming would
// collapse to one chunk per execution per 5 minutes.
func TestDirectPublishLiveFeedDeliversEveryChunk(t *testing.T) {
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server not installed; skipping JetStream live-feed path test")
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
	waitForNATSReady(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pub, err := NewNATSPublisher(ctx, url)
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	sub, err := NewNATSSubscriber(ctx, url)
	if err != nil {
		t.Fatalf("new subscriber: %v", err)
	}

	// Subscribe exactly as StreamExecutionEvents does.
	ch, err := sub.Subscribe(ctx, "orchicon.events.execution.>", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const subject = "orchicon.events.execution.execution.text"
	chunks := []string{"the model ", "streams this ", "token by token"}
	for i, c := range chunks {
		msgID := fmt.Sprintf("direct:exec-live:execution.text:%d", i+1)
		if err := pub.Publish(ctx, subject, msgID, []byte(c)); err != nil {
			t.Fatalf("publish chunk %d: %v", i, err)
		}
	}

	got := make([]EventMsg, 0, len(chunks))
	deadline := time.After(10 * time.Second)
	for len(got) < len(chunks) {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("consumer channel closed after %d of %d chunks", len(got), len(chunks))
			}
			got = append(got, msg)
		case <-deadline:
			t.Fatalf("live feed delivered only %d of %d streamed chunks (the GUI would show a truncated reply): %v",
				len(got), len(chunks), got)
		}
	}

	// Every chunk must land on its own JetStream sequence — the frontend's
	// stream dedup key is <eventType>-<sequence>, so a shared sequence would
	// mean the GUI dropped all but one chunk.
	seqs := map[uint64]bool{}
	var joined string
	for i, m := range got {
		if m.Subject != subject {
			t.Errorf("chunk %d: unexpected subject %q", i, m.Subject)
		}
		if seqs[m.Seq] {
			t.Fatalf("chunk %d reused JetStream sequence %d; the frontend dedups on <type>-<sequence> and would drop it", i, m.Seq)
		}
		seqs[m.Seq] = true
		joined += string(m.Data)
	}
	if want := "the model streams this token by token"; joined != want {
		t.Fatalf("reassembled live text %q does not match the published stream %q", joined, want)
	}
}

func waitForNATSReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err := nats.Connect(url, nats.Name("orchicon-test"))
		if err == nil {
			nc.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server never became ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
