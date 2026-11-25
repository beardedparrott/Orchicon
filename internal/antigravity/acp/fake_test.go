package acp

// fake_test.go — THE REPLAY HARNESS. No live agy is ever spawned by this package's tests.
//
// fakeProc is a Proc backed by channels. A fixture is a list of {direction, raw} entries; a
// replay driver walks it in order and:
//
//   - on an s2c entry, emits the recorded bytes on the process's stdout;
//   - on a c2s entry, WAITS for the client to write the corresponding document, then checks the
//     method and id, and (for an agent answer) that the result matches the recording.
//
// The lockstep is what makes a fixture a real replay rather than a bag of lines: the client cannot
// skip a step, and cannot take a step the recording does not contain. Steps the TEST triggers (a
// cancel mid-turn, a permission decision) simply block the driver until the test causes that
// write.
//
// The driver runs on its own goroutine and reports failures as an error value, so a broken replay
// fails the test instead of calling Fatalf off the test goroutine.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureDir is where child 1's recordings live.
const fixtureDir = "../testdata/acp"

// offwireFixture is a probe transcript with no c2s/s2c lines; it is not replayable.
const offwireFixture = "offwire-probes.jsonl"

// writeTimeout bounds how long a replay step waits for the client's next document.
const writeTimeout = 10 * time.Second

// fixtureEntry is one recorded line.
type fixtureEntry struct {
	dir string // "c2s" | "s2c"
	raw string
}

// loadFixture reads one recording. meta and header lines are skipped: they document facts, they
// are not protocol traffic.
func loadFixture(t *testing.T, name string) []fixtureEntry {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []fixtureEntry
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Dir  string          `json:"dir"`
			Line json.RawMessage `json:"line"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%s line %d: %v", name, i+1, err)
		}
		switch rec.Dir {
		case "c2s", "s2c":
			// The recorded line is a JSON *string*; keep its bytes verbatim so the replay is
			// exact, rather than re-encoding it.
			var s string
			if err := json.Unmarshal(rec.Line, &s); err != nil {
				t.Fatalf("%s line %d: %s line is not a string: %v", name, i+1, rec.Dir, err)
			}
			out = append(out, fixtureEntry{dir: rec.Dir, raw: s})
		case "meta", "":
			// recorder note or header
		default:
			t.Fatalf("%s line %d: unknown dir %q", name, i+1, rec.Dir)
		}
	}
	return out
}

// fixtureC2SCount counts the documents the client is expected to write.
func fixtureC2SCount(entries []fixtureEntry) int {
	n := 0
	for _, e := range entries {
		if e.dir == "c2s" {
			n++
		}
	}
	return n
}

// fakeProc is a Proc whose streams are channels the test controls.
type fakeProc struct {
	lines   chan []byte
	stdErr  chan []byte
	written chan []byte

	exitOnce sync.Once
	exited   chan struct{}
	code     int
	waitErr  error

	mu     sync.Mutex
	closed bool

	// tail records every document the client wrote, for assertions that need the whole stream.
	tail []json.RawMessage
	// drops counts writes nobody drained (a test bug, recorded rather than hidden).
	drops int
}

func newFakeProc() *fakeProc {
	return &fakeProc{
		lines:   make(chan []byte, 4096),
		stdErr:  make(chan []byte, 256),
		written: make(chan []byte, 1024),
		exited:  make(chan struct{}),
	}
}

func (p *fakeProc) WriteLine(b []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("acp test: write to a closed fake process")
	}
	p.tail = append(p.tail, append(json.RawMessage(nil), b...))
	p.mu.Unlock()
	select {
	case p.written <- append([]byte(nil), b...):
	default:
		p.mu.Lock()
		p.drops++
		p.mu.Unlock()
	}
	return nil
}

func (p *fakeProc) Lines() <-chan []byte  { return p.lines }
func (p *fakeProc) Stderr() <-chan []byte { return p.stdErr }

func (p *fakeProc) Wait() (int, error) {
	<-p.exited
	return p.code, p.waitErr
}

func (p *fakeProc) CloseInput() error { return nil }
func (p *fakeProc) Kill() error       { p.shutdown(0, nil); return nil }
func (p *fakeProc) Close() error      { p.shutdown(0, nil); return nil }

// shutdown ends the fake child: stdout/stderr close and Wait unblocks.
func (p *fakeProc) shutdown(code int, err error) {
	p.exitOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.code = code
		p.waitErr = err
		close(p.lines)
		close(p.stdErr)
		close(p.exited)
	})
}

// emit queues one line on stdout.
func (p *fakeProc) emit(raw string) {
	select {
	case p.lines <- []byte(raw):
	case <-p.exited:
	}
}

// emitStderr queues one stderr line.
func (p *fakeProc) emitStderr(line string) {
	select {
	case p.stdErr <- []byte(line):
	case <-p.exited:
	default:
	}
}

// awaitWrite waits for the client's next written document.
func (p *fakeProc) awaitWrite(d time.Duration) ([]byte, error) {
	select {
	case b := <-p.written:
		return b, nil
	case <-time.After(d):
		return nil, fmt.Errorf("client wrote nothing within %s (expected the next fixture c2s entry)", d)
	}
}

// drainWrites discards everything the client has already written, so a test that inspects the
// NEXT write sees the one it caused rather than a leftover handshake request.
func (p *fakeProc) drainWrites() {
	for {
		select {
		case <-p.written:
		default:
			return
		}
	}
}

// written returns every document the client has written so far.
func (p *fakeProc) writtenDocs() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.tail...)
}

// c2sDoc is the decoded view of a client-written document.
type c2sDoc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// runReplay walks a fixture in lockstep with the client and returns a channel that carries the
// first failure (closed after a nil send when the whole fixture replayed cleanly).
func runReplay(fp *fakeProc, entries []fixtureEntry) <-chan error {
	done := make(chan error, 1)
	go func() {
		for i, e := range entries {
			switch e.dir {
			case "s2c":
				fp.emit(e.raw)
			case "c2s":
				got, err := fp.awaitWrite(writeTimeout)
				if err != nil {
					done <- fmt.Errorf("fixture entry %d: %w", i, err)
					return
				}
				if err := checkC2S(i, e.raw, got); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	return done
}

// checkC2S compares the client's document with the recording. Params are NOT compared wholesale:
// this client deliberately differs from the recorder (it advertises fs/terminal false, and it
// never sends the number 42 the handshake_auth quirk recorded), so each test asserts the params it
// cares about.
func checkC2S(idx int, want string, got []byte) error {
	var w, g c2sDoc
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return fmt.Errorf("fixture entry %d is not valid JSON: %w", idx, err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Errorf("client wrote invalid JSON at entry %d: %w (%s)", idx, err, got)
	}
	if g.JSONRPC != "2.0" {
		return fmt.Errorf("entry %d: client wrote jsonrpc %q, want \"2.0\"", idx, g.JSONRPC)
	}
	if w.Method != "" {
		if g.Method != w.Method {
			return fmt.Errorf("entry %d: client wrote method %q, want %q", idx, g.Method, w.Method)
		}
		if !sameJSONValue(w.ID, g.ID) {
			return fmt.Errorf("entry %d (%s): client id %s, want %s", idx, w.Method, g.ID, w.ID)
		}
		return nil
	}
	// A c2s entry with no method is the client ANSWERING an agent request.
	if !sameJSONValue(w.ID, g.ID) {
		return fmt.Errorf("agent answer %d: client id %s, want %s", idx, g.ID, w.ID)
	}
	if !sameJSONValue(w.Result, g.Result) {
		return fmt.Errorf("agent answer %d: client answered %s, want %s", idx, g.Result, w.Result)
	}
	return nil
}

// sameJSONValue compares two JSON documents by value, ignoring key order and whitespace.
func sameJSONValue(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// awaitReplay asserts the replay finished cleanly.
func awaitReplay(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("replay never finished (the client did not perform a step the fixture records)")
	}
}

// eventSink drains a session's Events() in the background so a test can assert on the sequence.
type eventSink struct {
	mu     sync.Mutex
	evs    []Update
	closed bool
}

func newEventSink(s *Session) *eventSink {
	k := &eventSink{}
	go func() {
		for u := range s.Events() {
			k.mu.Lock()
			k.evs = append(k.evs, u)
			k.mu.Unlock()
		}
		k.mu.Lock()
		k.closed = true
		k.mu.Unlock()
	}()
	return k
}

// waitForCount blocks until at least n events of the given kind have arrived.
func (k *eventSink) waitForCount(t *testing.T, kind string, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		k.mu.Lock()
		count := 0
		for _, u := range k.evs {
			if u.SessionUpdate() == kind {
				count++
			}
		}
		kinds := kindList(k.evs)
		k.mu.Unlock()
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("acp test: only %d of %d %q events within %s (kinds: %v)", count, n, kind, d, kinds)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForAny blocks until at least n events have arrived at all.
func (k *eventSink) waitForAny(t *testing.T, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		k.mu.Lock()
		got, kinds := len(k.evs), kindList(k.evs)
		k.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("acp test: only %d of %d events within %s (kinds: %v)", got, n, d, kinds)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// findKind returns the first event of the given kind.
func (k *eventSink) findKind(kind string) (Update, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, u := range k.evs {
		if u.SessionUpdate() == kind {
			return u, true
		}
	}
	return nil, false
}

// firstOfKind blocks briefly for the first event of a kind.
func (k *eventSink) firstOfKind(t *testing.T, kind string, d time.Duration) Update {
	t.Helper()
	k.waitForCount(t, kind, 1, d)
	u, ok := k.findKind(kind)
	if !ok {
		t.Fatalf("acp test: %q event disappeared", kind)
	}
	return u
}

// waitStable blocks until at least n events have arrived AND the count has stopped changing,
// so a sequence assertion taken afterwards cannot race a later arrival.
func (k *eventSink) waitStable(t *testing.T, n int, d time.Duration) {
	t.Helper()
	k.waitForAny(t, n, d)
	deadline := time.Now().Add(d)
	last := -1
	for {
		k.mu.Lock()
		got := len(k.evs)
		k.mu.Unlock()
		if got == last {
			return
		}
		last = got
		if time.Now().After(deadline) {
			t.Fatalf("acp test: event count never settled at %d (got %d, kinds: %v)", n, got, k.kinds())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (k *eventSink) all() []Update {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]Update(nil), k.evs...)
}

func (k *eventSink) kinds() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return kindList(k.evs)
}

// kindList renders the kinds in arrival order.
func kindList(evs []Update) []string {
	out := make([]string, 0, len(evs))
	for _, u := range evs {
		out = append(out, u.SessionUpdate())
	}
	return out
}

// mustKindSequence asserts the exact update kind sequence.
func mustKindSequence(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("update kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("update kinds = %v, want %v (first difference at index %d)", got, want, i)
		}
	}
}

// fixtures lists the replayable recordings, one TestReplay_<name> each.
// TestReplay_CoversEveryFixture keeps it in step with the directory.
var fixtures = []string{
	"ask_question",
	"auth_required",
	"cancel_midturn",
	"compact",
	"file_edit_diff",
	"handshake_auth",
	"load_session",
	"mcp_tool_call",
	"simple_turn",
	"thinking_turn",
	"tool_permission_reject",
	"tool_read_file",
	"tool_run_command_permission_allow",
	"unknown_model",
}

// newTestClient starts a Client over a fake process and returns the pieces each test needs.
func newTestClient(t *testing.T, handler Handler) (*Client, *fakeProc) {
	t.Helper()
	fp := newFakeProc()
	c, err := Start(context.Background(), Config{
		Factory: func(context.Context, ProcSpec) (Proc, error) { return fp, nil },
		Handler: handler,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c, fp
}

// allowAllHandler answers every permission request with the allow option.
type allowAllHandler struct{ saw chan PermissionRequest }

func (h allowAllHandler) RequestPermission(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
	if h.saw != nil {
		select {
		case h.saw <- req:
		default:
		}
	}
	id, ok := AllowOptionID(req.Options)
	if !ok {
		return PermissionDecision{Cancelled: true}, nil
	}
	return PermissionDecision{OptionID: id}, nil
}

// rejectHandler answers every permission request with the reject option.
type rejectHandler struct{ saw chan PermissionRequest }

func (h rejectHandler) RequestPermission(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
	if h.saw != nil {
		select {
		case h.saw <- req:
		default:
		}
	}
	id, ok := RejectOptionID(req.Options)
	if !ok {
		return PermissionDecision{Cancelled: true}, nil
	}
	return PermissionDecision{OptionID: id}, nil
}

// testLogger writes at a level that keeps `go test -v` readable but still shows warnings.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}
