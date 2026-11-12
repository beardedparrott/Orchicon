package orchicon

// ask_tool_deadline_test.go — a hung native tool must not silently consume the whole turn.
//
// The failure this pins: a tool that never returns emits no provider round and no bus event, so the
// turn is indistinguishable from a model that has gone quiet, and the operator is told the PROVIDER
// produced no output. The bound converts that into a tool error the model can read and route around.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// hangingAskTools never returns from a tool call until its context is done, which is exactly what a
// wedged subprocess, a blocked engine call, or a dead MCP server does.
type hangingAskTools struct {
	returned chan struct{}
}

func (h *hangingAskTools) AskToolDefs(context.Context) []ToolDef { return nil }

func (h *hangingAskTools) ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error) {
	<-ctx.Done() // hold until the deadline, like a wedged call
	close(h.returned)
	return "", ctx.Err()
}

// THE PROPERTY: the turn gets a TOOL error naming the tool, promptly, instead of blocking until some
// outer timeout blames the model.
func TestAHungToolReturnsAToolErrorInsteadOfBlockingTheTurn(t *testing.T) {
	t.Setenv("ORCHICON_ASK_TOOL_DEADLINE", "50ms")
	h := &hangingAskTools{returned: make(chan struct{})}

	start := time.Now()
	out, err := runAskTool(context.Background(), h, "unity", `{}`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a tool that never returns produced no error (out=%q) — the turn would block on it", out)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runAskTool took %s to give up; the deadline did not bound it", elapsed)
	}
	msg := err.Error()
	// IT MUST BLAME THE TOOL. The old path produced "The model produced no output", which sent the
	// operator to check provider credentials for a fault that had nothing to do with them.
	if !strings.Contains(msg, "unity") {
		t.Errorf("the error does not name the tool that hung: %q", msg)
	}
	if !strings.Contains(msg, "TOOL FAULT") {
		t.Errorf("the error does not separate a tool fault from a model fault: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "model produced no output") {
		t.Errorf("the error blames the model for a tool fault: %q", msg)
	}
}

// AND A HEALTHY CALL IS UNTOUCHED — the bound must not turn a slow-but-working tool into a failure.
func TestAFastToolIsReturnedUnchanged(t *testing.T) {
	t.Setenv("ORCHICON_ASK_TOOL_DEADLINE", "5s")
	out, err := runAskTool(context.Background(), fastAskTools{}, "read", `{}`)
	if err != nil {
		t.Fatalf("a fast tool returned an error: %v", err)
	}
	if out != "the tool's own output" {
		t.Fatalf("the tool's output was altered: %q", out)
	}
}

type fastAskTools struct{}

func (fastAskTools) AskToolDefs(context.Context) []ToolDef { return nil }
func (fastAskTools) ExecuteAskTool(context.Context, string, string) (string, error) {
	return "the tool's own output", nil
}
