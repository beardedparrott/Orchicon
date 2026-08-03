package runtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// TestStreamLinesHandlesLargeLines verifies a single stream line larger than
// the old 64KB cap survives intact. opencode `--format json` emits an entire
// model response as ONE stdout line; a scanner cap smaller than the response
// drops the line AND permanently kills the stream (bufio.Scanner cannot
// recover from ErrTooLong), which made otherwise-successful executions come
// back with empty output.
func TestStreamLinesHandlesLargeLines(t *testing.T) {
	big := strings.Repeat("x", 70*1024) // > old 64KB cap
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	streamTo(json.NewEncoder(&buf), "stdout", strings.NewReader(big+"\n"), &wg)
	wg.Wait()

	var ev AgentEvent
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("decode streamed event: %v", err)
	}
	if len(ev.Data) != len(big) {
		t.Fatalf("large line truncated: got %d bytes, want %d", len(ev.Data), len(big))
	}
	if ev.Data != big {
		t.Fatalf("large line corrupted")
	}
}
