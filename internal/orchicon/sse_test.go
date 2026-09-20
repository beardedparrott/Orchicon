package orchicon

import (
	"strings"
	"testing"
)

// --- Shared SSE reader --------------------------------------------------------

func TestSSEReaderEventTaggedFrames(t *testing.T) {
	r := newSSEReader(strings.NewReader(
		"event: message_start\ndata: {\"a\":1}\n\n" +
			": keep-alive comment\n\n" +
			"event: content_block_delta\ndata: {\"b\":2}\n\n"))
	f, ok, err := r.Next()
	if err != nil || !ok || f.Event != "message_start" || f.Data != `{"a":1}` {
		t.Fatalf("frame1 = %#v ok=%v err=%v", f, ok, err)
	}
	f, ok, err = r.Next()
	if err != nil || !ok || f.Event != "content_block_delta" || f.Data != `{"b":2}` {
		t.Fatalf("frame2 = %#v ok=%v err=%v", f, ok, err)
	}
	if _, ok, _ = r.Next(); ok {
		t.Fatal("EOF expected")
	}
}

func TestSSEReaderDataOnlyFrames(t *testing.T) {
	r := newSSEReader(strings.NewReader(sse(`{"x":1}`, `[DONE]`)))
	f, ok, _ := r.Next()
	if !ok || f.Event != "" || f.Data != `{"x":1}` {
		t.Fatalf("frame1 = %#v", f)
	}
	f, ok, _ = r.Next()
	if !ok || !f.Done() {
		t.Fatalf("[DONE] sentinel: %#v", f)
	}
}

func TestSSEReaderMultiDataLinesAndUnterminatedTail(t *testing.T) {
	// Two data: lines in one frame concatenate with \n.
	r := newSSEReader(strings.NewReader("data: line1\ndata: line2\n\n"))
	f, ok, _ := r.Next()
	if !ok || f.Data != "line1\nline2" {
		t.Fatalf("multi-data = %#v", f)
	}
	// Unterminated final frame still flushes at EOF.
	r2 := newSSEReader(strings.NewReader("data: tail"))
	f2, ok2, _ := r2.Next()
	if !ok2 || f2.Data != "tail" {
		t.Fatalf("tail = %#v ok=%v", f2, ok2)
	}
}

func TestSSEDecodedHelpers(t *testing.T) {
	if !(SSEDecoded{Data: "[DONE]"}).Done() {
		t.Fatal("Done")
	}
	if (SSEDecoded{}).Done() {
		t.Fatal("empty is not done")
	}
	if !(SSEDecoded{}).Empty() {
		t.Fatal("Empty")
	}
}
