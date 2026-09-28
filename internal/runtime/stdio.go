package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StdioRequest opens a long-lived duplex streaming child inside the run's
// leased container (the claude adapter's transport). Unlike Exec (one-shot
// shell, collected output), stdio keeps the child alive across many stdin
// frames and streams its stdout/stderr events back live, so a session can
// span multiple turns on ONE subprocess.
type StdioRequest struct {
	Argv       []string `json:"argv"`
	Env        []string `json:"env,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	ProjectDir string   `json:"project_dir,omitempty"`
	ExecID     string   `json:"exec_id,omitempty"`
}

// StdioSession is one live duplex streaming child. Events() yields the
// supervisor's AgentEvents (started/stdout/stderr/exit/error) until the
// child exits or the stream drops.
type StdioSession struct {
	execID string

	pr  *io.PipeReader
	pw  *io.PipeWriter
	mu  sync.Mutex
	enc *json.Encoder

	events chan AgentEvent
	cancel context.CancelFunc
	body   io.Closer

	done chan struct{}
	once sync.Once
}

// Stdio opens the duplex streaming child. The initial frame is sent through
// an io.Pipe so the constructor can block on the response HEADERS (which the
// daemon writes only after it decodes that first frame) without the caller
// having to sequence the write itself.
func (c *Client) Stdio(ctx context.Context, workflowID string, req StdioRequest) (*StdioSession, error) {
	if len(req.Argv) == 0 {
		return nil, fmt.Errorf("runtime stdio: argv required")
	}
	sctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	httpReq, err := http.NewRequestWithContext(sctx, http.MethodPost, "http://runtime"+"/v1/runtimes/"+workflowID+"/stdio", pr)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &StdioSession{
		execID: req.ExecID,
		pr:     pr,
		pw:     pw,
		enc:    json.NewEncoder(pw),
		events: make(chan AgentEvent, 256),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	ready := make(chan error, 1)
	go func() {
		resp, derr := c.hc.Do(httpReq)
		if derr != nil {
			ready <- derr
			s.finish(1, derr)
			return
		}
		if resp.StatusCode != http.StatusOK {
			rerr := readError(resp.Body)
			_ = resp.Body.Close()
			ready <- rerr
			s.finish(1, rerr)
			return
		}
		s.mu.Lock()
		s.body = resp.Body
		s.mu.Unlock()
		ready <- nil
		s.relay(resp.Body)
	}()
	// Send the opening frame: it must reach the server for the handler to
	// write response headers, so it rides its own goroutine (a synchronous
	// write would deadlock until Do returns, and Do waits on headers).
	initial := map[string]any{
		"cmd":         "stdio",
		"argv":        req.Argv,
		"env":         req.Env,
		"cwd":         req.Cwd,
		"project_dir": req.ProjectDir,
		"exec_id":     req.ExecID,
	}
	go func() { _ = s.writeFrame(initial) }()
	if err := <-ready; err != nil {
		_ = pw.Close()
		cancel()
		return nil, err
	}
	return s, nil
}

// Send writes one stdin frame (a user turn) to the child.
func (s *StdioSession) Send(data string) error {
	return s.writeFrame(map[string]any{"cmd": "stdin", "data": data, "exec_id": s.execID})
}

// Signal delivers a signal name ("INT"|"TERM"|"KILL") to the child.
func (s *StdioSession) Signal(name string) error {
	return s.writeFrame(map[string]any{"cmd": "signal", "signal": name, "exec_id": s.execID})
}

// CloseInput sends the "close" frame and closes the request body, which
// makes the supervisor close the child's stdin (the child then exits).
func (s *StdioSession) CloseInput() error {
	err := s.writeFrame(map[string]any{"cmd": "close", "exec_id": s.execID})
	_ = s.pw.Close()
	return err
}

// Events yields the child's AgentEvents until it exits or the stream drops.
func (s *StdioSession) Events() <-chan AgentEvent { return s.events }

// Close tears the session down (best effort) and releases the transport.
func (s *StdioSession) Close() error {
	_ = s.pw.Close()
	s.mu.Lock()
	body := s.body
	s.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (s *StdioSession) writeFrame(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc == nil {
		return fmt.Errorf("runtime stdio: session closed")
	}
	return s.enc.Encode(v)
}

func (s *StdioSession) relay(body io.Reader) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var ev AgentEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		s.emit(ev)
		if ev.Event == "exit" {
			s.finish(ev.ExitCode, nil)
			return
		}
		if ev.Event == "error" {
			s.finish(1, fmt.Errorf("runtime stdio: %s", ev.Error))
			return
		}
	}
	if err := sc.Err(); err != nil {
		s.emit(AgentEvent{Event: "error", Error: "stream dropped: " + err.Error()})
		s.finish(1, &StreamDroppedError{Err: err})
		return
	}
	s.emit(AgentEvent{Event: "error", Error: "stream ended without an exit event"})
	s.finish(1, &StreamDroppedError{Err: fmt.Errorf("stream ended without exit event")})
}

func (s *StdioSession) emit(ev AgentEvent) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// finish marks the session terminal. It closes done but NOT events: the
// consumer learns of termination from the terminal exit/error event, so
// closing the channel here would race a concurrent emit (send-on-closed).
func (s *StdioSession) finish(_ int, _ error) {
	s.once.Do(func() { close(s.done) })
}

// stripNewline trims one trailing newline (the supervisor appends one per
// line so frames stay line-delimited).
func stripNewline(s string) string {
	return strings.TrimSuffix(s, "\n")
}
