package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/transcript"
)

// TestClaudeBudgetAndCompactPartsRenderInTheSharedTranscript is the surface
// guard for the DISPLAYED half of this change. The shared ladder's budget
// warning and the Claude-native compact directive are durable user_message
// parts; internal/transcript is the ONE per-part renderer the session pane,
// the follow-up seed and the recovery seed all share (it is deliberately a
// leaf package for exactly that reason). Both the warning and the directive
// must therefore be visible in the rendered transcript. A payload-shape drift
// that silently dropped them from every pane would still pass every
// callback-level assertion in this suite.
func TestClaudeBudgetAndCompactPartsRenderInTheSharedTranscript(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	_, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-surface",
		Goal:        "ship the surface-testable feature",
		Budgets:     []byte(costGatedBudgets),
	}, rec)

	fp.push(initLine)
	fp.push(cacheHeavyResult) // turn 1: warn tier → the tenant's own message
	fp.push(cacheHeavyResult) // turn 2: escalate tier → message + compact directive
	waitFor(t, func() bool { return len(budgetCompactedParts(rec)) == 1 }, "budget compaction")
	// Drain the queued warning/directive turns so the session terminates.
	for i := 0; i < 3; i++ {
		fp.push(freeResult)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session never reached a terminal turn")
	}

	// Rebuild the durable rows exactly as they persist, then render them
	// through the SHARED renderer the panes and the recovery seeds use.
	var rows []db.SessionPart
	for _, p := range rec.snap().parts {
		if p.kind != db.SessionPartUserMessage {
			continue
		}
		body, err := json.Marshal(p.payload)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, db.SessionPart{
			ExecutionID: "exec-surface",
			Seq:         int64(len(rows) + 1),
			Kind:        p.kind,
			Payload:     body,
		})
	}
	if len(rows) < 2 {
		t.Fatalf("durable user_message parts = %d, want the ladder warning AND the compact directive", len(rows))
	}
	out := transcript.RenderParts(rows, 64*1024, 8*1024, "\n…(truncated)")
	if !strings.Contains(out, "TENANT-WARN") {
		t.Fatalf("the ladder's own warning is not visible in the rendered transcript: %q", out)
	}
	if !strings.Contains(out, "CONTEXT COMPACTION") {
		t.Fatalf("the compact directive is not visible in the rendered transcript: %q", out)
	}
}
