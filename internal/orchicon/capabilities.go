package orchicon

import "encoding/json"

// BuildCapabilitiesJSON returns the runtime-adapter capabilities JSON advertised for the native
// in-process session engine (adapter kind "orchicon").
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
