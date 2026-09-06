package orchicon

// todowrite_native_test.go: the native todowrite fix — the composite prompt
// orders a `todowrite` call every turn, so the native engine must resolve it
// (advertised def + a succeeding handler), never `unknown tool "todowrite"`.
// Pins: the def exists in hostToolDefs, HostTools.Execute answers success,
// and the loop's native path intercepts the call (digest stashed, no error).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeTodowriteDefAdvertised(t *testing.T) {
	h := NewHostTools(t.TempDir(), "")
	var found *ToolDef
	for i, d := range h.Defs() {
		if d.Name == "todowrite" {
			found = &h.Defs()[i]
			break
		}
	}
	if found == nil {
		t.Fatal("hostToolDefs must contain todowrite (composite prompt orders it every turn)")
	}
	if found.Description == "" {
		t.Error("todowrite def must carry a description")
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal([]byte(found.ParamsJSON), &schema); err != nil {
		t.Fatalf("todowrite ParamsJSON invalid: %v", err)
	}
	// The arg shape must mirror todoDigestArgs (internal/orchicon/prompt.go)
	// and the post-hoc parser (internal/execution/todos.go): {todos:[...]}.
	if _, ok := schema.Properties["todos"]; !ok {
		t.Errorf("todowrite ParamsJSON must carry a todos array, got %s", found.ParamsJSON)
	}
}

func TestNativeTodowriteExecuteSucceeds(t *testing.T) {
	h := NewHostTools(t.TempDir(), "")
	out, err := h.Execute(context.Background(), "todowrite", `{"todos":[{"content":"do the thing","status":"in_progress","priority":"high"}]}`)
	if err != nil {
		t.Fatalf("todowrite must succeed on the native engine, got: %v", err)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Errorf("todowrite result = %q, want success envelope", out)
	}
}

func TestNativeTodowriteLoopPath(t *testing.T) {
	// The loop must intercept todowrite natively (stash + success), never
	// route it to the registry as `unknown tool "todowrite"`.
	if !nativeToolName("todowrite") {
		t.Fatal("nativeToolName must recognize todowrite (loop native path)")
	}
	s := promptSession(t, &mockProvider{}, promptManifest("todowrite native composite"))
	s.tools = NewHostTools(t.TempDir(), "")
	cb := &recordedCallback{}
	calls := []ToolCall{{Index: 0, ToolCallID: "t1", Name: "todowrite", ArgsJSON: `{"todos":[{"content":"do the thing","status":"in_progress","priority":"high"}]}`}}
	results := s.executeTools(context.Background(), cb, calls)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Err != "" {
		t.Fatalf("todowrite must not error on the native path, got %q", results[0].Err)
	}
	if got := s.TodosDigest(); !strings.Contains(got, "do the thing") {
		t.Errorf("todo digest = %q, want the stashed payload", got)
	}
}
