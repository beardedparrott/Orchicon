package opencode

// TestFileEditHookSkipsNonCompletedStatus: only completed tool calls reach the file-edit hook.
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

// TestFileEditHookFiresBeforeWriteArtifactBreak: the `write` artifact branch (adapter.go) breaks
// out of the tool_use switch for every write carrying a `content` input.
func TestFileEditHookFiresBeforeWriteArtifactBreak(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	var calls []string
	adapter.SetFileEditHook(func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string) {
		calls = append(calls, toolName)
	})

	enginePayload := "write: applied 1 write(s): notes/a.txt\n{\"summary\":\"s\",\"file_edits\":[{\"path\":\"notes/a.txt\",\"kind\":\"create\",\"unified_diff\":\"--- /dev/null\\n+++ b/notes/a.txt\\n@@ -0,0 +1,1 @@\\n+hi\\n\"}]}"
	mkWriteEvt := func(callID string, input map[string]any, output string) map[string]any {
		return map[string]any{
			"type": "tool_use",
			"part": map[string]any{
				"tool":   "write",
				"callID": callID,
				"state": map[string]any{
					"status": "completed",
					"input":  input,
					"output": output,
				},
			},
		}
	}

	ctx := context.Background()
	row := db.ExecutionRow{ID: "exec-write", TenantID: "tnt-1"}
	var output strings.Builder
	textSeq := 0
	// Engine single-op write wrapper shape ({filePath, content} + payload).
	adapter.parseEvent(ctx, row, manifest, mkWriteEvt("call-eng", map[string]any{"filePath": "notes/a.txt", "content": "hi"}, enginePayload), noopCallbacks{}, nil, &output, nil, &textSeq, &execStreamState{}, nil)
	// Genuine built-in write shape ({path, content}, no payload).
	adapter.parseEvent(ctx, row, manifest, mkWriteEvt("call-bin", map[string]any{"path": "note.md", "content": "hello"}, "wrote note.md"), noopCallbacks{}, nil, &output, nil, &textSeq, &execStreamState{}, nil)
	if len(calls) != 2 {
		t.Fatalf("write events fired the hook %d times, want 2 (engine + built-in)", len(calls))
	}
}
