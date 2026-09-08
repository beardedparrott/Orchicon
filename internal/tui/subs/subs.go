// Package subs wires the tui/stream engine to bubbletea: every screen
// stream subscription forwards status transitions (and optional cache
// invalidation pings) into buffered tea channels, mirroring the footer's
// "disconnected, retrying" subscription in useStream.ts semantics.
package subs

import (
	"context"
	"fmt"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
)

// statusMsg carries a stream status transition to the shell (footer).
// Type lives in subs so screens and the shell share one import.
type StatusMsg struct {
	Name   string
	Status stream.Status
}

// pingMsg tells a screen its source data changed (stream event seen).
type pingMsg struct {
	Name string
}

// Ping returns the tea.Msg a screen's OnEvent hook produces.
func Ping(name string) tea.Msg { return pingMsg{Name: name} }

// EventPokeMsg tells a screen one live stream event arrived (the
// execution screen's session view repaints from the sub's buffer).
type EventPokeMsg struct {
	Name string
}

// WaitEventPoke arms a re-armable tea.Cmd parked on the named sub's
// event channel (cap-1 overflow-dropping; the repaint only needs the
// latest). The screen re-arms after each handled poke.
func (r *Registry) WaitEventPoke(name string) tea.Cmd {
	ch := r.EventPokeChan(name)
	return func() tea.Msg {
		<-ch
		return EventPokeMsg{Name: name}
	}
}

// Registry owns the live subscriptions of the ACTIVE screen. Switching
// tabs closes everything (useStream semantics: navigating away
// unsubscribes) — the shell calls CloseAll from Screen.Close.
type Registry struct {
	mu     sync.Mutex
	subs   []subHandle
	chans  map[string]chan string
	events map[string]chan struct{}
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{chans: map[string]chan string{}, events: map[string]chan struct{}{}}
}

// subHandle is the minimum every registry member implements.
type subHandle interface {
	Close()
	Reconnect()
}

// add registers a sub for CloseAll / ReconnectAll.
func (r *Registry) add(s subHandle) {
	r.mu.Lock()
	r.subs = append(r.subs, s)
	r.mu.Unlock()
}

// CloseAll closes every subscription (idempotent — stream.Sub.Close is).
func (r *Registry) CloseAll() {
	r.mu.Lock()
	old := r.subs
	r.subs = nil
	r.mu.Unlock()
	for _, s := range old {
		s.Close()
	}
}

// ReconnectAll forces every live subscription to redial now (the footer
// "r" key / post-resume nudge).
func (r *Registry) ReconnectAll() {
	r.mu.Lock()
	live := make([]subHandle, len(r.subs))
	copy(live, r.subs)
	r.mu.Unlock()
	for _, s := range live {
		s.Reconnect()
	}
}

// Count returns the number of live subscriptions (diagnostics/tests).
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// StatusChan returns the receive side of the named sub's status channel
// (buffered; overflow drops — the UI only needs the latest).
func (r *Registry) StatusChan(name string) <-chan string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.chans[name]
	if !ok {
		ch = make(chan string, 16)
		r.chans[name] = ch
	}
	return ch
}

// notify pushes status strings into the named channel (non-blocking).
func (r *Registry) notify(name string) func(stream.Status) {
	return func(st stream.Status) {
		r.mu.Lock()
		ch := r.chans[name]
		r.mu.Unlock()
		if ch == nil {
			return
		}
		select {
		case ch <- string(st):
		default:
		}
	}
}

// pokeEvent delivers one event-poke to the named channel (non-blocking,
// cap-1: only the latest matters).
func (r *Registry) pokeEvent(name string) func(any) {
	return func(any) {
		r.mu.Lock()
		ch := r.events[name]
		r.mu.Unlock()
		if ch == nil {
			return
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// EventPokeChan returns (creating) the named sub's event-poke channel.
func (r *Registry) EventPokeChan(name string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.events[name]
	if !ok {
		ch = make(chan struct{}, 1)
		r.events[name] = ch
	}
	return ch
}

// ProjectEvents subscribes to StreamProjectEvents for the tenant.
func (r *Registry) ProjectEvents(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamProjectEventsResponse] {
	name := "project-events"
	cfg := stream.Config[*apiv1.StreamProjectEventsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamProjectEventsResponse, error), error) {
			req := &apiv1.StreamProjectEventsRequest{TenantId: tenantID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.Projects.StreamProjectEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID: func(m *apiv1.StreamProjectEventsResponse) string { return m.GetEvent().GetEventId() },
		GetSequence: func(m *apiv1.StreamProjectEventsResponse) int64 {
			return m.GetSequence()
		},
		OnStatus: r.notify(name),
		OnEvent:  func(*apiv1.StreamProjectEventsResponse) {},
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// ExecutionEvents subscribes to StreamExecutionEvents.
func (r *Registry) ExecutionEvents(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamExecutionEventsResponse] {
	name := "execution-events"
	cfg := stream.Config[*apiv1.StreamExecutionEventsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamExecutionEventsResponse, error), error) {
			req := &apiv1.StreamExecutionEventsRequest{TenantId: tenantID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.Executions.StreamExecutionEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID:  func(m *apiv1.StreamExecutionEventsResponse) string { return m.GetEvent().GetEventId() },
		GetSequence: func(m *apiv1.StreamExecutionEventsResponse) int64 { return m.GetSequence() },
		OnStatus:    r.notify(name),
		OnEvent: func(*apiv1.StreamExecutionEventsResponse) {
			r.pokeEvent(name)(nil)
		},
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// WorkflowEvents subscribes to StreamWorkflowEvents.
func (r *Registry) WorkflowEvents(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamWorkflowEventsResponse] {
	name := "workflow-events"
	cfg := stream.Config[*apiv1.StreamWorkflowEventsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamWorkflowEventsResponse, error), error) {
			req := &apiv1.StreamWorkflowEventsRequest{TenantId: tenantID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.Workflows.StreamWorkflowEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID:  func(m *apiv1.StreamWorkflowEventsResponse) string { return m.GetEvent().GetEventId() },
		GetSequence: func(m *apiv1.StreamWorkflowEventsResponse) int64 { return m.GetSequence() },
		OnStatus:    r.notify(name),
		OnEvent:     func(*apiv1.StreamWorkflowEventsResponse) {},
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// RecoveryEvents subscribes to StreamRecoveryEvents (enforcement UX).
func (r *Registry) RecoveryEvents(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamRecoveryEventsResponse] {
	name := "recovery-events"
	cfg := stream.Config[*apiv1.StreamRecoveryEventsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamRecoveryEventsResponse, error), error) {
			req := &apiv1.StreamRecoveryEventsRequest{TenantId: tenantID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.Recovery.StreamRecoveryEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID:  func(m *apiv1.StreamRecoveryEventsResponse) string { return m.GetEvent().GetEventId() },
		GetSequence: func(m *apiv1.StreamRecoveryEventsResponse) int64 { return m.GetSequence() },
		OnStatus:    r.notify(name),
		OnEvent:     func(*apiv1.StreamRecoveryEventsResponse) {},
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// Telemetry subscribes to StreamTelemetry (control screen worker status).
// The response carries a oneof update (no event id) — dedup keys off the
// sequence number.
func (r *Registry) Telemetry(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamTelemetryResponse] {
	name := "telemetry"
	cfg := stream.Config[*apiv1.StreamTelemetryResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamTelemetryResponse, error), error) {
			req := &apiv1.StreamTelemetryRequest{TenantId: tenantID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.Telemetry.StreamTelemetry(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID: func(m *apiv1.StreamTelemetryResponse) string {
			return fmt.Sprintf("telemetry:%d", m.Sequence)
		},
		GetSequence: func(m *apiv1.StreamTelemetryResponse) int64 { return m.GetSequence() },
		OnStatus:    r.notify(name),
		OnEvent:     func(*apiv1.StreamTelemetryResponse) {},
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// WaitStatus is the re-armable tea.Cmd that consumes the next status from
// the named channel and converts it to StatusMsg. Screens call it in Init
// and re-arm after each delivered StatusMsg (the tea.Msg loop pattern).
func (r *Registry) WaitStatus(name string) tea.Cmd {
	ch := r.StatusChan(name)
	return func() tea.Msg {
		v, ok := <-ch
		if !ok {
			return nil
		}
		return StatusMsg{Name: name, Status: stream.Status(v)}
	}
}
