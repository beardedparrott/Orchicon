package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// newFileEditHook builds the adapter's file-edit ledger hook: the execution
// population's ingestion point for the diff pipeline (file_edit_ledger).
// Extracted as a named constructor (not an inline closure) so regression
// tests can invoke the exact production wiring with a fake store.
//
// Ingestion rule per tool name:
//   - "write" / "edit": since the composite-tools rollout these names denote
//     BOTH the worktree engine's single-op wrappers (whose ground truth rides
//     the structured `file_edits` output) AND the opencode built-in file tools
//     (observed plane-side via an after-snapshot). The engine payload is tried
//     FIRST — it is exact — and the observer fallback runs only when the
//     output carries no file_edits payload (genuine built-in usage). Routing
//     write/edit exclusively down the observer path silently dropped every
//     successful single-op engine edit (the live-run gap).
//   - "batch_write": engine tool, structured output only.
//   - "file_diff": adapter fallback for paths no known mutating tool covered.
//   - virtual tools (write_artifact, todowrite*): no file touched, ignored.
//
// Failed tool calls never reach Record: an error-state output carries no
// file_edits payload (parsed 0 → observer fallback → empty Entry → dropped),
// and the adapter gates non-completed statuses before invoking the hook.
func newFileEditHook(feSvc *fileedit.Service, log *slog.Logger) opencode.FileEditHookFunc {
	// Per-execution built-in tool observers: on the first opencode built-in
	// write/edit for a run, an Observer is created rooted at the run's exec
	// dir (worktree or project dir); it caches last-seen content per path
	// (git-HEAD-seeded) so each ObserveAfter diffs real file state on both
	// sides. Entries live for the process lifetime — bounded by one entry per
	// execution that used built-in write tools.
	var (
		mu  sync.Mutex
		obs = map[string]*fileedit.Observer{}
	)
	observer := func(execID, execDir string) *fileedit.Observer {
		if execDir == "" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		o, ok := obs[execID]
		if !ok {
			o = fileedit.NewObserver(execDir)
			obs[execID] = o
		}
		return o
	}
	// inputStr reads a string field off the tool_use input map.
	inputStr := func(m map[string]any, key string) string {
		if s, ok := m[key].(string); ok {
			return s
		}
		return ""
	}
	return func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string) {
		const ownerKind = db.FileEditOwnerExecution
		switch toolName {
		case "write", "edit":
			// Engine single-op wrapper output carries the exact in-engine
			// diffs; genuine built-in output never contains "file_edits",
			// so the branch is unambiguous. parsed > 0 claims the edit
			// even when the funnel records 0 (an engine-observed no-op
			// must NOT fall through to the HEAD-seeded observer, which
			// would hallucinate a row for it).
			if parsed, recorded := feSvc.RecordEngineOutput(ctx, tenantID, ownerKind, execID, toolName, output); parsed > 0 {
				log.Debug("file edit hook: engine payload claimed write/edit",
					"execution", execID, "tool", toolName, "parsed", parsed, "recorded", recorded)
				return
			}
			// opencode built-in write/edit (the runtime's native file tools):
			// take a plane-side after-snapshot. "before" is the observer's
			// last-seen content (git-HEAD-seeded on first sight), "after" is
			// the fresh read — real file state on both sides.
			if o := observer(execID, execDir); o != nil {
				p := inputStr(input, "filePath")
				if p == "" {
					p = inputStr(input, "path")
				}
				tool := fileedit.ToolOpenCodeWrite
				if toolName == "edit" {
					tool = fileedit.ToolOpenCodeEdit
				}
				if e := o.ObserveAfter(p, tool); e.Path != "" {
					feSvc.Record(ctx, tenantID, ownerKind, execID, []fileedit.Entry{e})
				} else {
					log.Debug("file edit hook: observer saw no change",
						"execution", execID, "tool", toolName, "path", p)
				}
			}
		case "batch_write":
			// Worktree engine tool: the engine already computed the
			// ground-truth diffs in its structured output.
			if parsed, recorded := feSvc.RecordEngineOutput(ctx, tenantID, ownerKind, execID, toolName, output); parsed > 0 {
				log.Debug("file edit hook: engine payload claimed batch_write",
					"execution", execID, "tool", toolName, "parsed", parsed, "recorded", recorded)
			}
		case "file_diff":
			// Adapter fallback for paths no known mutating tool covered:
			// the input carries the file_diff event's path. Real file
			// state both sides (observer cache vs fresh read); no-op reads
			// drop out inside ObserveAfter.
			if o := observer(execID, execDir); o != nil {
				p := inputStr(input, "path")
				if e := o.ObserveAfter(p, "file_diff"); e.Path != "" {
					feSvc.Record(ctx, tenantID, ownerKind, execID, []fileedit.Entry{e})
				}
			}
		case "write_artifact", "todowrite", "todowrite_more":
			// Virtual tools: no file touched.
		default:
			return
		}
	}
}
