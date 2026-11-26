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
	// Settings is a `--settings` JSON document: agy's own "Settings for this session only, never
	// saved". It is how an Ask mode's denied tools are ALSO refused before agy runs them
	// (internal/antigravity/modegate.go) — defence in depth behind the in-handler gate that is the
	// actual authority.
	//
	// IT IS ONE ARGV ELEMENT, never shell-quoted into a single string: ProcSpec.Argv is an execve
	// vector, so the JSON travels as ONE argument exactly as written and nothing re-parses it.
	Settings string
}

// Argv renders the child command line. Flag order is fixed so the same options always produce
// the same argv (tests and logs depend on that).
func (o LaunchOptions) Argv() []string {
	bin := o.Binary
	if bin == "" {
		bin = "agy"
	}
	// `--settings` IS A TOP-LEVEL FLAG AND MUST PRECEDE THE SUBCOMMAND. Verified against agy 1.5.13:
	//
	//	agy acp --settings '{"permissions":{"deny":["run_command"]}}'  -> exit 1, prints `agy acp --help`
	//	agy --settings '{"permissions":{"deny":["run_command"]}}' acp  -> exit 0
	//
	// It is absent from `agy acp --help` for exactly that reason: it belongs to the parent command.
	argv := []string{bin}
	if o.Settings != "" {
		argv = append(argv, "--settings", o.Settings)
	}
	argv = append(argv, "acp")
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
	// STDOUT AND STDERR ARE OUR OWN os.Pipe()es, NOT cmd.StdoutPipe()/StderrPipe(), AND THAT IS A
	// CORRECTNESS FIX rather than a style choice.
	//
	// os/exec's documentation is explicit: "Cmd.Wait will close the pipe after seeing the command exit,
	// so most callers need not close the pipe themselves. It is thus incorrect to call Wait before all
	// reads from the pipe have completed." This constructor CANNOT honour that — it starts `p.wait()`
	// (which is `cmd.Wait()`) and the pumps as sibling goroutines, because Wait is also the liveness
	// signal the whole package reads from (`Wait()`, `Close()`, the conn's teardown). So Wait closing
	// StdoutPipe's READ end raced the pump's reads.
	//
	// MEASURED FAILURE THIS REPLACES: `TestListModelsRunsOnTheLocalTransportEvenWithARuntimeClient`
	// (internal/antigravity/modellist_test.go) failed on the full gate with "child output = \"\"" — the
	// model listing's child printed `ok` and exited, and the output was gone before the pump read it,
	// so the picker's listing came back EMPTY on a fast-racing spawn. Reproduced deterministically
	// outside this package: a `sh -c 'echo ok'` spawn in exactly this shape (StdoutPipe + concurrent
	// cmd.Wait, GOMAXPROCS=1) lost the child's output in 5/20000 runs; the same harness with os.Pipe
	// lost it in 0/20000.
	//
	// cmd.Wait only closes the descriptors IT created, so a pipe we create is not touched by it: the
	// child's write end keeps the read end open until the child exits, and every buffered byte is still
	// there for the pump. The parent's copy of the write end is closed immediately after Start (below) —
	// exactly what StdoutPipe arranges internally — so the pump still sees EOF when the child exits.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("acp: stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		for _, c := range []io.Closer{stdoutR, stdoutW, stderrR, stderrW} {
			_ = c.Close()
		}
		return nil, fmt.Errorf("acp: start %s: %w", spec.Argv[0], err)
	}
	// The child holds its own dup of the write ends now; drop ours so EOF is reachable.
	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdout, stderrPipe := stdoutR, stderrR
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
	// THE THIRD wg COUNTER IS THIS GOROUTINE, and it must be released here. It was added by wg.Add(3)
	// above but never Done()d, so `wg.Wait()` in Close could never be satisfied: every Close waited out
	// the full DefaultShutdownGrace (3s) and logged "the output pumps did not end" even on a perfectly
	// clean exit. That is not merely slow — the model picker does one spawn+Close per listing, and the
	// Ask teardown does it on the plane's shutdown path, so a clean shutdown reported a false alarm
	// about a forked helper AND paid 3s it did not owe.
	defer p.wg.Done()
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

// pump reads r line by line into ch, closing ch when the stream ends. An over-long line is
// dropped with a log rather than being allowed to fail the read.
func (p *localProc) pump(r io.Reader, ch chan []byte) {
	defer p.wg.Done()
	defer close(ch)
	// The read end is OURS now (see newLocalProc), so nobody else will close it: do it here, when the
	// stream has ended, so a finished child costs no descriptor.
	if c, ok := r.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
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
			p.droppedLines++
			slog.Default().Warn("acp: dropped over-long output line", "limit_bytes", maxLineBytes, "dropped", p.droppedLines)
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

func trimTrailingNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}
