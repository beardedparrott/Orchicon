package opencode

// TestFileEditHookSkipsNonCompletedStatus: only completed tool calls reach
// the file-edit hook. An error/failed/in-flight tool_use event carries no
// ground truth — failed edits must never create phantom ledger rows (AC 4).
// The hook previously fired for EVERY tool_use regardless of status; the
// tolerance of the parse funnel happened to drop most of them, but an error
// output that still embeds a file_edits payload (retry tail, partial write)
// would have ledgered a lie. The gate is explicit now.
import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func TestFileEditHookSkipsNonCompletedStatus(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	var calls int
	adapter.SetFileEditHook(func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string) {
		calls++
	})

	payload := "batch_write: applied 1 write(s): a.txt\n{\"summary\":\"s\",\"file_edits\":[{\"path\":\"a.txt\",\"kind\":\"create\",\"unified_diff\":\"--- /dev/null\\n+++ b/a.txt\\n@@ -0,0 +1,1 @@\\n+hi\\n\"}]}"
	mkEvt := func(status string) map[string]any {
		return map[string]any{
			"type": "tool_use",
			"part": map[string]any{
				"tool":   "batch_write",
				"callID": "call-" + status,
				"state": map[string]any{
					"status": status,
					"input":  map[string]any{},
					"output": payload,
				},
			},
		}
	}

	ctx := context.Background()
	row := db.ExecutionRow{ID: "exec-status", TenantID: "tnt-1"}
	for _, status := range []string{"error", "failed", "running", "pending"} {
		var output strings.Builder
		textSeq := 0
		adapter.parseEvent(ctx, row, manifest, mkEvt(status), noopCallbacks{}, nil, &output, nil, &textSeq, &execStreamState{}, nil)
	}
	if calls != 0 {
		t.Fatalf("non-completed statuses fired the hook %d times, want 0", calls)
	}

	var output strings.Builder
	textSeq := 0
	adapter.parseEvent(ctx, row, manifest, mkEvt("completed"), noopCallbacks{}, nil, &output, nil, &textSeq, &execStreamState{}, nil)
	if calls != 1 {
		t.Fatalf("completed status fired the hook %d times, want 1", calls)
	}
}
