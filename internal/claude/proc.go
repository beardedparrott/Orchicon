package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/runtime"
)

// ProcSession is one live claude subprocess. Stdout is line-framed JSONL
// (one stream-json document per line). Two impls exist:
//
//   - localProc — a host os/exec child (local execution mode + the
//     env-gated live smoke test).
//   - containerProc — a duplex streaming child inside the run's runtime
//     container, driven over runtime.Client.Stdio.
//
// The session loop speaks only to this interface, so the core acceptance is
// proven against a fake ProcSession with canned fixtures and zero real
// Anthropic spend.
type ProcSession interface {
	// WriteTurn appends one JSON object + "\n" onto the child's stdin. The
	// process stays alive across calls (long-lived session).
	WriteTurn(payload []byte) error
	// Lines yields decoded stdout lines (each one JSON document).
	Lines() <-chan []byte
	// Stderr yields stderr lines, for the failure message.
	Stderr() <-chan []byte
	// Signal sends a signal by name ("INT" | "TERM" | "KILL").
	Signal(sig string) error
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
	// Close closes stdin then kills the child. Idempotent.
	Close() error
}

// procSpec is the spawn request the bridge hands to its proc factory.
type procSpec struct {
	ExecID     string
	Argv       []string
	Cwd        string
	ProjectDir string
	Env        []string
}

// procFactory spawns ONE claude subprocess. The production default selects
// local vs container from the manifest; tests substitute a fake.
type procFactory func(ctx context.Context, spec procSpec) (ProcSession, error)

// localProc is a host os/exec claude child.
type localProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	stderr chan []byte

	waitOnce sync.Once
	code     int
	waitErr  error
	done     chan struct{}
	wg       sync.WaitGroup
	closeOne sync.Once
}

func newLocalProc(ctx context.Context, spec procSpec) (*localProc, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("claude: empty argv")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &localProc{
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan []byte, 256),
		stderr: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	go p.wait()
	p.wg.Add(2)
	go p.pump(stdout, p.lines)
	go p.pump(stderr, p.stderr)
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

// pump scans r line-by-line into ch, closing ch when the stream ends (the
// process exited). Scanner delivers a final unterminated line too.
func (p *localProc) pump(r io.Reader, ch chan []byte) {
	defer p.wg.Done()
	defer close(ch)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		ch <- append([]byte(nil), sc.Bytes()...)
	}
}

func (p *localProc) WriteTurn(payload []byte) error {
	_, err := p.stdin.Write(append(append([]byte(nil), payload...), '\n'))
	return err
}

func (p *localProc) Lines() <-chan []byte  { return p.lines }
func (p *localProc) Stderr() <-chan []byte { return p.stderr }

func (p *localProc) Signal(sig string) error {
	if p.cmd.Process == nil {
		return nil
	}
	return signalProcess(p.cmd.Process, sig)
}

func (p *localProc) Wait() (int, error) {
	<-p.done
	return p.code, p.waitErr
}

func (p *localProc) Close() error {
	p.closeOne.Do(func() {
		_ = p.stdin.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(timeAfterProcClose):
				_ = p.cmd.Process.Kill()
			}
		}
	})
	p.wg.Wait()
	return nil
}

// containerProc drives a long-lived streaming child inside the run's
// runtime container over the daemon's duplex stdio transport.
type containerProc struct {
	sess      *runtime.StdioSession
	lines     chan []byte
	stderr    chan []byte
	quit      chan struct{}
	done      chan struct{}
	once      sync.Once
	closeOnce sync.Once

	code int
	err  error
}

func newContainerProc(ctx context.Context, rt *runtime.Client, spec procSpec) (*containerProc, error) {
	if rt == nil {
		return nil, fmt.Errorf("claude: no runtime client for container spawn")
	}
	sess, err := rt.Stdio(ctx, spec.ExecID, runtime.StdioRequest{
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

// pump fans the session's events into the line channels and records the
// terminal exit code/error.
func (p *containerProc) pump() {
	defer close(p.lines)
	defer close(p.stderr)
	for {
		select {
		case <-p.quit:
			return
		case ev, ok := <-p.sess.Events():
			if !ok {
				p.finish(1, fmt.Errorf("claude container stdio stream ended"))
				return
			}
			switch {
			case ev.Event == "exit":
				p.finish(ev.ExitCode, nil)
				return
			case ev.Event == "error":
				p.finish(1, fmt.Errorf("%s", ev.Error))
				return
			case ev.Stream == "stdout":
				p.lines <- []byte(stripTrailingNewline(ev.Data))
			case ev.Stream == "stderr":
				p.stderr <- []byte(stripTrailingNewline(ev.Data))
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

func (p *containerProc) WriteTurn(payload []byte) error { return p.sess.Send(string(payload)) }
func (p *containerProc) Signal(sig string) error        { return p.sess.Signal(sig) }
func (p *containerProc) Lines() <-chan []byte           { return p.lines }
func (p *containerProc) Stderr() <-chan []byte          { return p.stderr }

func (p *containerProc) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *containerProc) Close() error {
	p.closeOnce.Do(func() { close(p.quit) })
	_ = p.sess.CloseInput()
	return p.sess.Close()
}

func stripTrailingNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}

// signalProcess maps a signal name onto the process. Unknown names are a
// no-op; a nil process is a no-op. Kept OS-portable via syscall.Signal.
func signalProcess(proc *os.Process, sig string) error {
	var s syscall.Signal
	switch sig {
	case "INT":
		s = syscall.SIGINT
	case "TERM":
		s = syscall.SIGTERM
	case "KILL":
		s = syscall.SIGKILL
	default:
		return nil
	}
	return proc.Signal(s)
}
