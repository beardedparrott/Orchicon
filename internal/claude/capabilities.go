package claude

import "encoding/json"

// BuildCapabilitiesJSON returns the runtime-adapter capabilities JSON advertised
// for the Claude Code in-process bridge (adapter kind "claude").
//
// WHY THIS EXISTS, and it is not decoration. Dispatch resolves an adapter in TWO
// places, and the bridge registration only satisfies the first:
//
//  1. the Dispatcher — `dispatcher.Register(adapter.KindClaude, claudeBridge)`,
//     which routes an already-chosen execution to a bridge;
//  2. a READY `runtime_adapters` ROW of the matching kind —
//     `selectAdapter` → `db.ListReadyAdaptersByKind(tenant, kind)`, which is what
//     decides a task may dispatch AT ALL.
//
// The feature registered the bridge and never a row, so every claude worker failed
// with `no ready adapters of kind "claude"` — the exact black hole
// internal/server/server.go's own comment warns about, which had already been hit
// twice (opencode, then orchicon).
//
// This mirrors orchicon.BuildCapabilitiesJSON's shape, and the generic keys are the
// ones the control plane treats uniformly across kinds. The values are the ones
// this adapter ACTUALLY has, taken from its declared capability set rather than
// copied from a sibling:
//
//	execution  cancellation  ← Aborter (SIGINT before SIGTERM)
//	           resume        ← SessionContinuer (--resume <session-id>)
//	           mid_run_injection ← MessageInjector (writes a turn to the live stdin)
//	           context_compaction ← ContextCompacter (a Claude-native compact turn)
//	telemetry  transcript_jsonl ← the CLI's own ~/.claude/projects JSONL transcript
//	           tool_calls_streamed, file_diffs ← the parse.go callback mapper
//
// It is rebuilt each call so a future dynamic tool/MCP list can change it without a
// code-path change here — the same reason the native builder does.
func BuildCapabilitiesJSON() string {
	caps := map[string]any{
		// The CLI's tool surface.
		"tools":   []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "TodoWrite", "TaskCreate", "TaskUpdate"},
		"context": []string{"file_index"},
		"execution": []string{
			"cancellation", "resume", "mid_run_injection", "context_compaction",
		},
		"telemetry": []string{"tool_calls_streamed", "file_diffs", "transcript_jsonl"},
		// NB: these keys are the generic ones the control plane reads uniformly across kinds.
	}
	b, _ := json.Marshal(caps)
	return string(b)
}
