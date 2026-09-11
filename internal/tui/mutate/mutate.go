// Package mutate is the single write path for the TUI: one executor runs
// every mutating RPC, reports progress in the dock, surfaces failures,
// rolls the local model back on error, and reconciles the affected source.
//
// No screen calls a write RPC directly — a screen builds a mutate.Request
// (via a kit2.Action) and hands it here.
package mutate

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Request is one mutation: an optimistic local apply, the RPC thunk, and
// the inverse of the apply used to roll back on failure.
type Request struct {
	Name     string
	Source   string
	Apply    func() // optimistic local change (nil = none)
	Rollback func() // inverse of Apply (nil = nothing to undo)
	Do       func(ctx context.Context) error
}

// Result is the async outcome of a Request, delivered as a tea.Msg.
type Result struct {
	Name     string
	Source   string
	Err      error
	Rollback func()
}

// Sink is the feedback surface (the dock's notice/error strips).
type Sink interface {
	Progress(msg string)
	Fail(msg string)
	Notice(msg string)
}

// Executor runs mutations. A zero Reconcile/Sink is fine (tests).
type Executor struct {
	Sink      Sink
	Reconcile func(source string) tea.Cmd
	Timeout   time.Duration
}

func (e *Executor) timeout() time.Duration {
	if e.Timeout <= 0 {
		return 30 * time.Second
	}
	return e.Timeout
}

// Run applies the optimistic change, reports progress, and returns the cmd
// that performs the RPC off the update loop.
func (e *Executor) Run(req Request) tea.Cmd {
	if req.Apply != nil {
		req.Apply()
	}
	if e.Sink != nil {
		e.Sink.Progress("running " + req.Name + "…")
	}
	name, src, rb, do := req.Name, req.Source, req.Rollback, req.Do
	if do == nil {
		do = func(context.Context) error { return nil }
	}
	to := e.timeout()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), to)
		defer cancel()
		return Result{Name: name, Source: src, Err: do(ctx), Rollback: rb}
	}
}

// Apply handles a Result: on error it rolls the local model back and pins
// the failure into the dock; on success it clears progress and reconciles
// the affected source. Returns the reconcile cmd (nil when none).
func (e *Executor) Apply(res Result) tea.Cmd {
	if res.Err != nil {
		if res.Rollback != nil {
			res.Rollback()
		}
		if e.Sink != nil {
			e.Sink.Fail(fmt.Sprintf("%s failed: %v", res.Name, res.Err))
		}
		return nil
	}
	if e.Sink != nil {
		e.Sink.Notice(res.Name + " succeeded")
	}
	if res.Source != "" && e.Reconcile != nil {
		return e.Reconcile(res.Source)
	}
	return nil
}

// RunSync runs a Request synchronously (used in tests + simple call sites):
// apply → RPC → rollback/reconcile. Returns the reconcile cmd.
func (e *Executor) RunSync(req Request) tea.Cmd {
	cmd := e.Run(req)
	var res Result
	if cmd != nil {
		if msg, ok := cmd().(Result); ok {
			res = msg
		}
	}
	// Run already set the source/rollback on the Result; carry them when the
	// cmd returned nothing.
	if res.Name == "" {
		res = Result{Name: req.Name, Source: req.Source, Rollback: req.Rollback}
	}
	return e.Apply(res)
}
