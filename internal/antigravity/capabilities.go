package antigravity

import "encoding/json"

// BuildCapabilitiesJSON returns the runtime-adapter capabilities JSON advertised for the Antigravity (agy)
// in-process bridge (adapter kind "antigravity").
//
// WHY THIS EXISTS, and it is not decoration. Dispatch resolves an adapter in TWO places, and the
// bridge registration only satisfies the first:
//
//  1. the Dispatcher — `dispatcher.Register(adapter.KindAntigravity, agyBridge)`, which routes an
//     already-chosen execution to a bridge;
//  2. a READY `runtime_adapters` ROW of the matching kind — `selectAdapter` →
//     `db.ListReadyAdaptersByKind(tenant, kind)`, which is what decides a task may dispatch AT ALL.
//
// Registering only the bridge is the exact black hole that has now been hit THREE times (opencode,
// orchicon, then claude): every worker of that kind fails with `no ready adapters of kind "<k>"`.
//
// THE VALUES ARE EXACTLY WHAT IS IMPLEMENTED, AND NOTHING MORE. Advertising a capability the adapter does
// not have makes `selectAdapter` route work that then fails at runtime, which is worse than refusing it up
// front. Each entry below is backed by a method in this package that a test exercises:
//
//	execution:  cancellation        → Bridge.AbortExecution (`session/cancel`, then process close)
//	            resume              → Bridge.ContinueSession + SessionContinuer (`session/resume`)
//	            mid_run_injection   → Bridge.SendExecutionMessage — QUEUED until the turn's stopReason,
//	                                  because agy defines no way to inject into a running turn
//	            context_compaction  → Bridge.CompactExecution (agy's own `/compact` slash command)
//	telemetry:  tool_calls_streamed → mapper.go's tool_call/tool_call_update arms
//	            file_diffs          → mapper.go's diff content → OnWrittenFiles + the diff ledger
//	            usage               → mapper.go's prompt-result usage → the usage recorder
//
// TWO DELIBERATE OMISSIONS:
//
//   - `transcript_jsonl` is ABSENT. agy advertises no transcript file (docs §7 row 3: "no transcript file
//     is advertised"), so there is no path to expose the way internal/claude/transcript.go does for claude.
//     The bridge keeps its OWN durable transcript through the SessionStore (session parts), which is what
//     the recovery path reads.
//   - `reasoning` is ABSENT. agy 1.5.13 never streams reasoning text — only `usage.thoughtTokens` — so
//     advertising a reasoning channel would promise a surface the recording proves does not exist. The
//     mapper still handles `agent_thought_chunk` for a future agy.
func BuildCapabilitiesJSON() string {
	caps := map[string]any{
		// agy's own tool surface, informational for the control plane. The raw names are used here (not
		// the canonical ones the callbacks receive) because this list describes what the AGENT offers.
		"tools": []string{
			"run_command", "write_file", "edit_file", "read_file",
			"grep_search", "search_web", "read_url_content", "ask_question", "mcp",
		},
		// What the bridge knows how to deliver from the manifest and the tenant's MCP configuration.
		"context": []string{"system_prompt_append", "mcp_servers", "additional_directories"},
		"execution": []string{
			"cancellation", "resume", "mid_run_injection", "context_compaction",
		},
		"telemetry": []string{
			"tool_calls_streamed", "file_diffs", "usage",
		},
		// NB: these keys are the generic ones the control plane reads uniformly across kinds.
	}
	b, _ := json.Marshal(caps)
	return string(b)
}

// antigravityCapabilities is the decoded form of BuildCapabilitiesJSON, so a test can assert that an
// advertised capability corresponds to a real method rather than to a string someone typed.
func antigravityCapabilities() map[string][]string {
	var caps map[string][]string
	_ = json.Unmarshal([]byte(BuildCapabilitiesJSON()), &caps)
	return caps
}

// capabilitiesAdvertises reports whether the advertised set names one value under one key.
func capabilitiesAdvertises(key, value string) bool {
	for _, v := range antigravityCapabilities()[key] {
		if v == value {
			return true
		}
	}
	return false
}

// capabilityAdvertisedAnywhere reports whether any capability key names value.
func capabilityAdvertisedAnywhere(value string) bool {
	for _, vs := range antigravityCapabilities() {
		for _, v := range vs {
			if v == value {
				return true
			}
		}
	}
	return false
}
