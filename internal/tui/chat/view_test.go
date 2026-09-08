package chat

import (
	"strings"
	"testing"
)

func TestRenderItemsShapes(t *testing.T) {
	items := []ChatItem{
		{Kind: KindUser, Text: "hello there friend", Key: "u1"},
		{Kind: KindText, Text: "hi! doing the thing", Key: "t1"},
		{Kind: KindReasoning, Text: "pondering", Key: "r1"},
		{Kind: KindTool, Tool: &ParsedTool{ID: "1", ToolName: "bash", Input: "ls", Output: "files"}, Key: "t2"},
		{Kind: KindArtifact, Name: "main.go", Type: "file", Content: "package main", Key: "a1"},
		{Kind: KindError, Text: "boom", Key: "e1"},
		{Kind: KindSession, SessionID: "ses_1", ServeURL: "http://x", Key: "s1"},
	}
	out := RenderItems(items, 80)
	for _, want := range []string{"you", "hello there friend", "orch", "thinking", "⚙ bash", "⬒ main.go", "error", "boom", "session ses_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderItemsWrapClampsWidth(t *testing.T) {
	long := strings.Repeat("word ", 60)
	out := RenderItems([]ChatItem{{Kind: KindText, Text: long}}, 40)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len([]rune(line)) > 45 { // label prefix + slack for ANSI-free check
			t.Fatalf("line too wide (%d): %q", len([]rune(line)), line)
		}
	}
}

func TestRenderEmptyItems(t *testing.T) {
	if got := RenderItems(nil, 80); got != "" {
		t.Fatalf("got %q", got)
	}
}
