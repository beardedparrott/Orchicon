package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// dedupRecordingPublisher records the MsgID of every direct publish. No other
// eventbus.Publisher fake exists in the tree; this one is local to the
// direct-publish dedup regression guard.
type dedupRecordingPublisher struct {
	mu       sync.Mutex
	subjects []string
	msgIDs   []string
	payloads [][]byte
}

func (p *dedupRecordingPublisher) Publish(_ context.Context, subject, msgID string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subject)
	p.msgIDs = append(p.msgIDs, msgID)
	p.payloads = append(p.payloads, append([]byte(nil), data...))
	return nil
}

func (p *dedupRecordingPublisher) snapshot() (subjects, msgIDs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.subjects...), append([]string(nil), p.msgIDs...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestPublishExecEventMsgIDUnique is the regression guard for the blocking
// finding behind this work item: the direct publish used a CONSTANT MsgID
// ("direct:<exec>:<type>"), so JetStream's Duplicates: 5m window silently
// dropped every direct publish after the first per execution per 5 minutes —
// which made the outbox relay the real live-text path and made cutting the
// per-token outbox write unsafe. The MsgID must be unique per publish.
func TestPublishExecEventMsgIDUnique(t *testing.T) {
	pub := &dedupRecordingPublisher{}
	r := &TaskReconciler{log: discardLogger(), eventPub: pub}
	ctx := context.Background()
	exec := db.ExecutionRow{ID: "exec-dedup-1", TenantID: "tnt_dev", Status: "running"}

	r.publishExecEvent(ctx, "execution.text", exec, map[string]any{"text": "chunk-a"})
	r.publishExecEvent(ctx, "execution.text", exec, map[string]any{"text": "chunk-b"})
	r.publishExecEvent(ctx, "execution.tool_call", exec, map[string]any{"tool_name": "read"})

	subjects, msgIDs := pub.snapshot()
	if len(msgIDs) != 3 {
		t.Fatalf("expected 3 direct publishes, got %d", len(msgIDs))
	}
	for _, s := range subjects {
		if s != "orchicon.events.execution.execution.text" && s != "orchicon.events.execution.execution.tool_call" {
			t.Errorf("unexpected direct-publish subject %q", s)
		}
	}
	seen := map[string]bool{}
	for i, id := range msgIDs {
		if seen[id] {
			t.Fatalf("duplicate direct-publish MsgID %q at index %d — JetStream (Duplicates: 5m) silently drops all but the first, so live streaming would break", id, i)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "direct:exec-dedup-1:execution.") {
			t.Errorf("MsgID %q should keep the direct:<exec>:<type> prefix plus a unique suffix", id)
		}
	}
}
