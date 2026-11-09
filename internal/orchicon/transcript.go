package orchicon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Transcript event kinds (one JSON object per line of the JSONL file).
const (
	// TransSession is the header event: identity block + model binding.
	TransSession = "session"
	// TransUserMessage is a user message (goal at session start, or a
	// mid-run injected human message).
	TransUserMessage = "user_message"
	// TransText is a chunk of assistant text.
	TransText = "text"
	// TransReasoning is a chunk of assistant reasoning.
	TransReasoning = "reasoning"
	// TransToolCall is a complete tool invocation.
	TransToolCall = "tool_call"
	// TransToolResult is the result of one tool invocation.
	TransToolResult = "tool_result"
	// TransWrittenFiles carries the deduped set of written file paths.
	TransWrittenFiles = "written_files"
	// TransCompaction records that the platform COLLAPSED the session's context mid-run: which trigger
	// fired, how many messages went in and came out, and how many tool rounds were evicted.
	TransCompaction = "compaction"
	// TransError is a surfaced error (stream error, tool failure, panic).
	TransError = "error"
	// TransState is a lifecycle marker: started / cancelled / failed / done.
	TransState = "state"
	// TransFinish is the terminal model-finish marker.
	TransFinish = "finish"
)

// transEntry is one JSONL line.
type transEntry struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	TS   string          `json:"ts"`
	Data json.RawMessage `json:"data,omitempty"`
}

// transToolCallData is the durable payload of a TransToolCall event: the assistant turn's
// accumulated text plus its complete tool calls.
type transToolCallData struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"tool_calls"`
}

// JSONLTranscript is the crash-safe append-only session transcript at
// <project_dir>/.orchicon/sessions/<session_id>.jsonl.
type JSONLTranscript struct {
	path string

	mu   sync.Mutex
	f    *os.File
	seq  int64
	done bool // Close already called (or fatal write error)
	// observer, when set, is invoked after every durable append (while mu is held) with the entry's
	// seq/type/marshaled payload.
	observer func(seq int64, typ string, data []byte)
}

// SetObserver installs the transcript observer. Must be called before the
// first Append (the bridge wires it between transcript open and Run).
func (t *JSONLTranscript) SetObserver(fn func(seq int64, typ string, data []byte)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observer = fn
}

// openTranscript opens (creating if needed) the JSONL at path in append mode.
func openTranscript(path string) (*JSONLTranscript, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("transcript: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("transcript: open: %w", err)
	}
	t := &JSONLTranscript{path: path, f: f}
	// If the file exists from a prior (crashed/cancelled) run, advance seq
	// past the last durable line so resume appends continue the sequence.
	t.seq = t.lastSeq()
	return t, nil
}

// lastSeq scans the file for the highest durable seq.
func (t *JSONLTranscript) lastSeq() int64 {
	var max int64
	if _, err := t.f.Seek(0, 0); err != nil {
		return 0
	}
	sc := bufio.NewScanner(t.f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e transEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn final line — tolerated
		}
		if e.Seq > max {
			max = e.Seq
		}
	}
	// Leave the file positioned for appends (O_APPEND ignores offset).
	if _, err := t.f.Seek(0, 2); err != nil {
		return max
	}
	return max
}

// Append writes one event and fsyncs it before returning.
func (t *JSONLTranscript) Append(typ string, data any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return fmt.Errorf("transcript: append after close")
	}
	t.seq++
	payload := json.RawMessage("null")
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			payload = b
		}
	}
	e := transEntry{Seq: t.seq, Type: typ, TS: time.Now().UTC().Format(time.RFC3339Nano), Data: payload}
	b, err := json.Marshal(e)
	if err != nil {
		t.done = true
		_ = t.f.Close()
		return fmt.Errorf("transcript: marshal: %w", err)
	}
	if _, err := t.f.Write(append(b, '\n')); err != nil {
		t.done = true
		_ = t.f.Close()
		return fmt.Errorf("transcript: write: %w", err)
	}
	if err := t.f.Sync(); err != nil {
		t.done = true
		_ = t.f.Close()
		return fmt.Errorf("transcript: fsync: %w", err)
	}
	if t.observer != nil {
		t.observer(e.Seq, e.Type, payload)
	}
	return nil
}

// Close flushes and closes the file. Safe to call twice; after Close,
// appends are no-ops.
func (t *JSONLTranscript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	t.done = true
	return t.f.Close()
}

// Path returns the transcript file path.
func (t *JSONLTranscript) Path() string { return t.path }

// Seq returns the current sequence counter (last durable seq + 1).
func (t *JSONLTranscript) Seq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seq
}

// replayEvent is one decoded transcript line (for Load/replay).
type replayEvent struct {
	Seq  int64
	Type string
	TS   string
	Data json.RawMessage
}

// Load reads a transcript file and returns its ordered events, tolerating a torn final line.
func Load(path string) ([]replayEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("transcript: load: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var out []replayEvent
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e transEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn final line — tolerated
		}
		out = append(out, replayEvent{Seq: e.Seq, Type: e.Type, TS: e.TS, Data: e.Data})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("transcript: scan: %w", err)
	}
	return out, nil
}

// SeedFrom re-appends the durable lines of a prior session's transcript into this one, preserving
// their original seq values (the new session's own header is written first.
func (t *JSONLTranscript) SeedFrom(path string) error {
	evs, err := Load(path)
	if err != nil {
		return fmt.Errorf("transcript: seed from %s: %w", path, err)
	}
	if len(evs) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return fmt.Errorf("transcript: seed after close")
	}
	// Keep the FIRST event's seq as the anchor (the new header was already
	// appended with seq 1); prior events continue after it.
	if len(evs) == 0 {
		return nil
	}
	anchor := evs[0].Seq
	for _, e := range evs {
		seq := e.Seq - anchor + 1
		if seq <= t.seq {
			continue // already seeded
		}
		payload := json.RawMessage("null")
		if len(e.Data) > 0 {
			payload = e.Data
		}
		entry := transEntry{Seq: seq, Type: e.Type, TS: e.TS, Data: payload}
		b, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("transcript: seed marshal: %w", err)
		}
		if _, err := t.f.Write(append(b, '\n')); err != nil {
			t.done = true
			_ = t.f.Close()
			return fmt.Errorf("transcript: seed write: %w", err)
		}
		if err := t.f.Sync(); err != nil {
			t.done = true
			_ = t.f.Close()
			return fmt.Errorf("transcript: seed fsync: %w", err)
		}
		t.seq = seq
	}
	return nil
}
