package orchicon

import "encoding/json"

// BuildCapabilitiesJSON returns the runtime-adapter capabilities JSON
// advertised for the native in-process session engine (adapter kind
// "orchicon"). It mirrors the shape opencode.BuildCapabilitiesJSON
// produces so the control plane can treat both adapter kinds uniformly:
// the server seeds + heartbeats an in-process `adp_orchicon_dev`
// runtime_adapters row (kind "orchicon") with this payload, which is what
// lets the TaskReconciler's selectAdapter find a ready adapter for a
// native worker (model_ref orchicon/<provider>/<model>).
//
// The native engine's capabilities are static (the tool suite and loop
// behavior ship in-process), so the result does not vary per heartbeat —
// but it is rebuilt each call so a future dynamic tool/MCP list can
// change it without a code path change here.
func BuildCapabilitiesJSON() string {
	caps := map[string]any{
		"tools":     []string{"read", "write", "edit", "glob", "grep", "bash", "todowrite"},
		"context":   []string{"file_index"},
		"execution": []string{"cancellation", "resume", "mid_run_injection", "sequence_continuation"},
		"telemetry": []string{"tool_calls_streamed", "file_diffs", "transcript_jsonl"},
	}
	b, _ := json.Marshal(caps)
	return string(b)
}
