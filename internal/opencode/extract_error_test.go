// Package opencode: JSON error-event parsing tests.
//
// opencode's --format json stream emits structured error events like
// {"type":"error","error":{"name":"UnknownError","data":{"message":"..."}}}.
// The adapter must extract the human-readable message from
// error.data.message (NOT from a top-level message field, which doesn't
// exist on the wire) so the failure reason is preserved in
// worker_executions.error_message for the operator. These tests pin the
// parser so a refactor of opencode's JSON shape trips us early instead
// of silently dropping the reason in production.
package opencode

import "testing"

func TestExtractErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			name: "typical opencode JSON error event (provider 500)",
			in: map[string]any{
				"type": "error",
				"error": map[string]any{
					"name": "UnknownError",
					"data": map[string]any{
						"message": `{"code":500,"message":"Internal Server Error","metadata":{"error_type":"server"}}`,
					},
				},
			},
			want: `{"code":500,"message":"Internal Server Error","metadata":{"error_type":"server"}}`,
		},
		{
			name: "rate-limited plain message",
			in: map[string]any{
				"type": "error",
				"error": map[string]any{
					"name": "UnknownError",
					"data": map[string]any{
						"message": "AI_APICallError: Rate limit exceeded. Please try again later.",
					},
				},
			},
			want: "AI_APICallError: Rate limit exceeded. Please try again later.",
		},
		{
			name: "missing error object falls back to top-level message",
			in: map[string]any{
				"type":    "error",
				"message": "something else",
			},
			want: "something else",
		},
		{
			name: "empty input returns empty",
			in:   map[string]any{},
			want: "",
		},
		{
			name: "name only (no data)",
			in: map[string]any{
				"error": map[string]any{"name": "SomeName"},
			},
			want: "SomeName",
		},
		{
			name: "plain non-JSON message is returned verbatim",
			in: map[string]any{
				"error": map[string]any{
					"data": map[string]any{"message": "not json"},
				},
			},
			want: "not json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractErrorMessage(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
