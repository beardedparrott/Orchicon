package acp

// proc.go — HOW THE `agy acp` CHILD IS SPAWNED.
//
// Two transports, the same shape as internal/claude/proc.go: a local os/exec child on the host
// (local mode) and a child inside the run's runtime container over the daemon's duplex stdio
// transport (container mode). Tests substitute a fake Proc, which is the entire reason the
// factory is injectable.
//
// WINDOWS SAFETY IS A HARD REQUIREMENT HERE. internal/claude's Close sends a POSIX termination
// signal, which this file deliberately does NOT copy: shutdown is stdin close, then wait out a
// grace period, then os.Process.Kill, all of which compile and behave on every GOOS. There is no
// platform-specific syscall package import and no signal NAME anywhere in this package.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// maxLineBytes caps one stdout line. agy tool outputs are pasted into update payloads and can be
// large; an over-long line is DROPPED (with a log) rather than being allowed to kill the pump,
// because losing one tool output must not take the session down.
const maxLineBytes = 8 << 20

// DefaultShutdownGrace is how long Close waits for the child to exit after stdin closes before
// killing it. A var so a test can shorten it.
var DefaultShutdownGrace = 3 * time.Second

// Proc is one live `agy acp` child. Implementations must be safe for the use this package makes
// of them: WriteLine is called from one goroutine at a time (the conn serialises writes),
// Lines/Stderr are drained by the conn and its stderr ring, Close is idempotent.
type Proc interface {
	// WriteLine writes one already-encoded JSON document followed by a newline.
	WriteLine(b []byte) error
	// Lines yields stdout lines, each one JSON document. Closed when the process exits.
	Lines() <-chan []byte
	// Stderr yields stderr lines. Closed when the process exits.
	Stderr() <-chan []byte
	// Wait blocks until the child exits and reports its exit code.
	Wait() (int, error)
	// CloseInput closes stdin and nothing else.
	CloseInput() error
	// Kill stops the child hard. Portable (os.Process.Kill), never a POSIX signal.
	Kill() error
	// Close is CloseInput, then wait up to the grace period, then Kill. Idempotent.
	Close() error
}

// ProcSpec is the spawn request handed to a ProcFactory.
type ProcSpec struct {
	ExecID string
	Argv   []string
	Env    []string
	Cwd    string
	// ProjectDir is the project root the execution is scoped to.
	ProjectDir string
	// WorkflowRunID names the RUN whose runtime container this execution dispatches into.
	// Required by ContainerFactory, ignored by LocalFactory.
	WorkflowRunID string
}

// ProcFactory spawns ONE agy child. Tests substitute a fake.
type ProcFactory func(ctx context.Context, spec ProcSpec) (Proc, error)

// LaunchOptions are the `agy acp` flags this client knows how to set.
type LaunchOptions struct {
	Binary            string
	DisableTelemetry  bool
	ProcessPerSession bool
	Sandbox           bool
	AppDataDir        string
}

// Argv renders the child command line. Flag order is fixed so the same options always produce
// the same argv (tests and logs depend on that).
func (o LaunchOptions) Argv() []string {
	bin := o.Binary
	if bin == "" {
		bin = "agy"
	}
	argv := []string{bin, "acp"}
	if o.DisableTelemetry {
		argv = append(argv, "--disable-telemetry")
	}
	if o.ProcessPerSession {
		argv = append(argv, "--process-per-session")
	}
	if o.AppDataDir != "" {
		argv = append(argv, "--app-data-dir", o.AppDataDir)
	}
	if o.Sandbox {
		argv = append(argv, "--sandbox")
	}
	return argv
}

// LocalFactory spawns the child on this host.
type LocalFactory struct{}

// NewProc satisfies ProcFactory.
func (LocalFactory) NewProc(ctx context.Context, spec ProcSpec) (Proc, error) {
	return newLocalProc(ctx, spec)
}

// StdioOpener is the slice of *runtime.Client ContainerFactory needs.
type StdioOpener interface {
	Stdio(ctx context.Context, workflowID string, req runtime.StdioRequest) (*runtime.StdioSession, error)
}

// ContainerFactory spawns the child inside the run's runtime container.
func ContainerFactory(opener StdioOpener) ProcFactory {
	return func(ctx context.Context, spec ProcSpec) (Proc, error) {
		return newContainerProc(ctx, opener, spec)
	}
}

// localProc is a host os/exec agy child.
type localProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	stderr chan []byte

	done     chan struct{}
	waitOnce sync.Once
	code     int
	waitErr  error

	wg       sync.WaitGroup
	closeOne sync.Once

	droppedLines int
}

func newLocalProc(ctx context.Context, spec ProcSpec) (Proc, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("acp: empty argv")
	}
	// NOT CommandContext: this package owns shutdown, and a context-cancelled exec would kill
	// the child out from under the graceful stdin-close path.
	cmd := procwin.Command(spec.Argv[0], spec.Argv[1:]...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %s: %w", spec.Argv[0], err)
	}
	p := &localProc{
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan []byte, 256),
		stderr: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	p.wg.Add(3)
	go p.wait()
	go p.pump(stdout, p.lines)
	go p.pump(stderrPipe, p.stderr)
	return p, nil
}

func (p *localProc) wait() {
	werr := p.cmd.Wait()
	code := 0
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	p.waitOnce.Do(func() {
		p.code = code
		p.waitErr = werr
		close(p.done)
	})
}

// pump reads r line by line into ch, closing ch when the stream ends.
func (p *localProc) pump(r io.Reader, ch chan []byte) {
	defer p.wg.Done()
	pumpLines(r, ch, func() {
		p.droppedLines++
		slog.Default().Warn("acp: dropped over-long output line", "limit_bytes", maxLineBytes, "dropped", p.droppedLines)
	})
}

// pumpLines reads r line by line into ch, closing ch when the stream ends. An over-long line is
// dropped (onDrop reports it) rather than being allowed to fail the read.
//
// THIS IS THE LINE DECODER, AND IT IS A FREE FUNCTION ON PURPOSE. Line-terminator handling is the
// one thing about the transport that differs by operating system — agy emits `\r\n`-terminated lines
// on Windows — and a decoder welded to an exec.Cmd cannot be fed a `bytes.Reader` in a test, so the
// CRLF case would only ever be reachable on a Windows host. crlf_test.go drives THIS directly with
// both terminators and asserts the messages decode identically (AC3).
func pumpLines(r io.Reader, ch chan []byte, onDrop func()) {
	defer close(ch)
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				ch <- buf
			}
			return
		}
		if len(buf)+len(chunk) > maxLineBytes {
			// Over-long: swallow the remainder of this line and carry on.
			if onDrop != nil {
				onDrop()
			}
			buf = nil
			for isPrefix {
				_, more, derr := br.ReadLine()
				if derr != nil {
					return
				}
				isPrefix = more
			}
			continue
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			out := buf
			buf = nil
			ch <- out
		}
	}
}

func (p *localProc) WriteLine(b []byte) error {
	if _, err := p.stdin.Write(append(append([]byte(nil), b...), '\n')); err != nil {
		return fmt.Errorf("acp: write to agy stdin: %w", err)
	}
	return nil
}

func (p *localProc) Lines() <-chan []byte  { return p.lines }
func (p *localProc) Stderr() <-chan []byte { return p.stderr }

func (p *localProc) Wait() (int, error) {
	<-p.done
	return p.code, p.waitErr
}

func (p *localProc) CloseInput() error {
	err := p.stdin.Close()
	if err != nil && errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (p *localProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *localProc) Close() error {
	p.closeOne.Do(func() {
		_ = p.CloseInput()
		select {
		case <-p.done:
		case <-time.After(DefaultShutdownGrace):
			_ = p.Kill()
			select {
			case <-p.done:
			case <-time.After(DefaultShutdownGrace):
			}
		}
	})
	// Drain the pumps so the channels are closed for the conn's reader before we return.
	//
	// THE DRAIN IS BOUNDED, and that is load-bearing rather than defensive. A pipe's read end EOFs only when
	// EVERY holder of its write end is gone — and agy forks helpers (`--process-per-session` workers, an MCP
	// stdio child) that inherit this process's stdout. So the CHILD can be dead and `p.done` closed while a
	// grandchild still holds the pipe open, and an unbounded `wg.Wait()` would then block for as long as that
	// grandchild lives. MEASURED: the model listing's own teardown (internal/antigravity/modellist.go, which
	// every picker open drives) sat in this `wg.Wait()` past a 45s probe deadline on the real agy 1.5.13, so
	// the picker RPC never answered at all.
	//
	// ABANDONING THE PUMPS IS SAFE. They own nothing but their channels; the conn's reader sees a closed
	// process, the session ends, and a leaked reader goroutine on a dead child costs one goroutine — against a
	// caller that hangs forever, that is not a close call.
	drained := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(DefaultShutdownGrace):
		slog.Default().Warn("acp: the agy output pumps did not end after the child exited — abandoning them "+
			"(a forked helper is still holding the pipe open)", "grace", DefaultShutdownGrace.String())
	}
	return nil
}

// containerProc drives a long-lived streaming child inside the run's runtime container.
type containerProc struct {
	sess   *runtime.StdioSession
	lines  chan []byte
	stderr chan []byte
	quit   chan struct{}
	done   chan struct{}

	once      sync.Once
	closeOnce sync.Once

	code int
	err  error
}

func newContainerProc(ctx context.Context, rt StdioOpener, spec ProcSpec) (Proc, error) {
	if rt == nil {
		return nil, errors.New("acp: no runtime client for container spawn")
	}
	if spec.WorkflowRunID == "" {
		return nil, errors.New("acp: no workflow run id for container spawn — the daemon leases containers by RUN id")
	}
	sess, err := rt.Stdio(ctx, spec.WorkflowRunID, runtime.StdioRequest{
		Argv:       spec.Argv,
		Env:        spec.Env,
		Cwd:        spec.Cwd,
		ProjectDir: spec.ProjectDir,
		ExecID:     spec.ExecID,
	})
	if err != nil {
		return nil, err
	}
	p := &containerProc{
		sess:   sess,
		lines:  make(chan []byte, 256),
		stderr: make(chan []byte, 64),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go p.pump()
	return p, nil
}

func (p *containerProc) pump() {
	defer close(p.lines)
	defer close(p.stderr)
	for {
		select {
		case <-p.quit:
			p.finish(1, ErrClosed)
			return
		case ev, ok := <-p.sess.Events():
			if !ok {
				p.finish(1, errors.New("acp: container stdio stream ended"))
				return
			}
			switch {
			case ev.Event == "exit":
				p.finish(ev.ExitCode, nil)
				return
			case ev.Event == "error":
				p.finish(1, fmt.Errorf("acp: container stdio error: %s", ev.Error))
				return
			case ev.Stream == "stdout":
				p.lines <- []byte(trimTrailingNewline(ev.Data))
			case ev.Stream == "stderr":
				p.stderr <- []byte(trimTrailingNewline(ev.Data))
			}
		}
	}
}

func (p *containerProc) finish(code int, err error) {
	p.once.Do(func() {
		p.code = code
		p.err = err
		close(p.done)
	})
}

func (p *containerProc) WriteLine(b []byte) error { return p.sess.Send(string(b)) }
func (p *containerProc) Lines() <-chan []byte    { return p.lines }
func (p *containerProc) Stderr() <-chan []byte   { return p.stderr }

func (p *containerProc) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *containerProc) CloseInput() error { return p.sess.CloseInput() }
func (p *containerProc) Kill() error       { return p.sess.Close() }

func (p *containerProc) Close() error {
	p.closeOnce.Do(func() {
		_ = p.sess.CloseInput()
		select {
		case <-p.done:
		case <-time.After(DefaultShutdownGrace):
			_ = p.sess.Close()
		}
		close(p.quit)
	})
	return nil
}

// trimTrailingNewline strips ONE line terminator — `\n`, `\r\n`, or a lone trailing `\r`.
//
// THE CARRIAGE RETURN IS THE WINDOWS BUG. The container transport hands us a stream of raw chunks,
// and agy on Windows terminates every JSON-RPC line with `\r\n` (the headless doc's own sample shows
// `"output":"hello_headless_demo\r\n"`). Stripping only `\n` left the `\r` glued to the end of the
// JSON object, which some decoders tolerate as trailing whitespace and others do not — and a
// trailing `\r` inside a *string* value would be a protocol error instead of a formatting one. A
// lone `\r` is stripped too, because a partial chunk boundary can deliver it alone.
func trimTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
