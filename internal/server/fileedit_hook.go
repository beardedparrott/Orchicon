package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// newFileEditHook builds the adapter's file-edit ledger hook: the execution population's ingestion
// point for the diff pipeline (file_edit_ledger).
func newFileEditHook(feSvc *fileedit.Service, log *slog.Logger, ownerKind string) opencode.FileEditHookFunc {
	// Per-execution built-in tool observers: on the first opencode built-in write/edit for a run, an
	// Observer is created rooted at the run's exec dir (worktree or project dir).
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
		switch toolName {
		case "write", "edit":
			// Engine single-op wrapper output carries the exact in-engine diffs; genuine built-in output never
			// contains "file_edits", so the branch is unambiguous.
			if parsed, recorded := feSvc.RecordEngineOutput(ctx, tenantID, ownerKind, execID, toolName, output); parsed > 0 {
				log.Debug("file edit hook: engine payload claimed write/edit",
					"execution", execID, "tool", toolName, "parsed", parsed, "recorded", recorded)
				return
			}
			// opencode built-in write/edit (the runtime's native file tools): take a plane-side after-snapshot.
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
			// Adapter fallback for paths no known mutating tool covered: the input carries the file_diff
			// event's path.
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
