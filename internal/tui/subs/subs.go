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
	// latest is the MOST RECENT status per subscription name. It exists so a
	// status can never be lost: the channels below are wake-ups and drop on
	// overflow, and a delivery can be routed to a different screen than the one
	// that armed it — either way the previous status could freeze forever (the
	// footer stuck on "connecting"). Readers take the VALUE from here.
	latest map[string]string
	// latestErr is the most recent dial/stream ERROR per subscription name, so
	// the shell can say WHY a stream is not up (see LatestError).
	latestErr map[string]string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		chans:     map[string]chan string{},
		events:    map[string]chan struct{}{},
		latest:    map[string]string{},
		latestErr: map[string]string{},
	}
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
		r.latest[name] = string(st)
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

// LatestError is the most recent error reported for a subscription name (""
// when it has none). The shell surfaces it so a failing stream explains itself
// instead of showing a bare "connecting…".
func (r *Registry) LatestError(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latestErr[name]
}

// ReportStatusForTest records a status for a name as if its subscription had reported it, so a test can
// put the registry into a state only a live plane could otherwise produce (a dead connection, a
// reconnecting stream). It goes through the same store `notify` writes, so the reading under test is the
// real one.
func (r *Registry) ReportStatusForTest(name, status string) {
	r.mu.Lock()
	r.latest[name] = status
	r.mu.Unlock()
}

// notifyErr records a stream's last error (see Registry.latestErr).
func (r *Registry) notifyErr(name string) func(error) {
	return func(err error) {
		if err == nil {
			return
		}
		r.mu.Lock()
		r.latestErr[name] = err.Error()
		r.mu.Unlock()
	}
}

// LatestStatus is the most recent status reported for a subscription name
// ("" when the name has never reported). This is the authoritative value — the
// footer reads it rather than trusting the last message a screen happened to
// receive, which could be stale if a delivery was routed elsewhere.
func (r *Registry) LatestStatus(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest[name]
}

// WorstStatus returns the most SEVERE status any live subscription has reported, and the name of the one
// that reported it. ("", "") when nothing has reported yet.
//
// WHY THE FOOTER NEEDS THIS RATHER THAN THE ACTIVE SCREEN'S OWN STREAMS. The shell used to aggregate only
// over the statuses the ACTIVE screen declares, and a screen that declares none — the Ask tab, whose
// conversation list is the shell's rail and which subscribes to no stream of its own — therefore reported
// "open", i.e. CONNECTED, for the whole session. So an operator sitting on Ask while the plane died saw a
// green footer: the operator's "if a connection dies, the GUI tells you, but the TUI conversation does
// not."
//
// The connection is not a property of the tab the operator happens to be looking at. Every subscription in
// the registry is talking to the SAME plane over the SAME credentials, so the worst status among them is
// the honest answer to "am I connected?" — and it is answerable from any tab.
func (r *Registry) WorstStatus() (stream.Status, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	worst := stream.Status("")
	worstName := ""
	for name, st := range r.latest {
		s := stream.Status(st)
		if statusSeverity(s) > statusSeverity(worst) {
			worst, worstName = s, name
		}
	}
	return worst, worstName
}

// statusSeverity ranks stream statuses by how much they should worry the operator. It mirrors the shell's
// own ranking (statusRank in the tui package) and lives here because the registry is what now decides the
// worst; keeping ONE ordering stops the two from disagreeing about "worse".
//
// An empty status ranks lowest, so a subscription that has never reported cannot masquerade as healthy.
func statusSeverity(s stream.Status) int {
	switch s {
	case stream.StatusIdle:
		return 0
	case stream.StatusOpen:
		return 1
	case stream.StatusConnecting:
		return 2
	case stream.StatusClosed:
		return 3
	case stream.StatusError:
		return 4
	case stream.StatusReconnecting:
		return 5
	default:
		return 0
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

// guard reports a missing API client for a stream. A nil service client would
// otherwise PANIC inside the subscription goroutine (dialing dereferences it),
// which takes the whole TUI down — a misconfigured or partially-built client set
// must degrade to a reported stream error and a reconnect, never a crash.
func guard(ok bool, what string) error {
	if !ok {
		return fmt.Errorf("tui: no API client for %s", what)
	}
	return nil
}

// ProjectEvents subscribes to StreamProjectEvents for the tenant.
func (r *Registry) ProjectEvents(cl *client.Clients, tenantID string) *stream.Sub[*apiv1.StreamProjectEventsResponse] {
	name := "project-events"
	cfg := stream.Config[*apiv1.StreamProjectEventsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamProjectEventsResponse, error), error) {
			if err := guard(cl != nil && cl.Projects != nil, "project events"); err != nil {
				return nil, err
			}
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
		OnStatus: r.notify(name), OnError: r.notifyErr(name),
		OnEvent: func(*apiv1.StreamProjectEventsResponse) {},
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
			if err := guard(cl != nil && cl.Executions != nil, "execution events"); err != nil {
				return nil, err
			}
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
		OnStatus:    r.notify(name), OnError: r.notifyErr(name),
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
			if err := guard(cl != nil && cl.Workflows != nil, "workflow events"); err != nil {
				return nil, err
			}
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
		OnStatus:    r.notify(name), OnError: r.notifyErr(name),
		OnEvent: func(*apiv1.StreamWorkflowEventsResponse) {},
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
			if err := guard(cl != nil && cl.Recovery != nil, "recovery events"); err != nil {
				return nil, err
			}
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
		OnStatus:    r.notify(name), OnError: r.notifyErr(name),
		OnEvent: func(*apiv1.StreamRecoveryEventsResponse) {},
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
			if err := guard(cl != nil && cl.Telemetry != nil, "telemetry"); err != nil {
				return nil, err
			}
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
		OnStatus:    r.notify(name), OnError: r.notifyErr(name),
		OnEvent: func(*apiv1.StreamTelemetryResponse) { r.pokeEvent(name)(nil) },
	}
	sub := stream.New(cfg)
	r.add(sub)
	return sub
}

// FileEdits subscribes to StreamFileEdits for one owner (an execution or
// an Ask conversation). Unlike the project/execution streams, the
// FileEditService RPCs REQUIRE a non-empty tenant_id (internal/fileedit/
// rpc.go → "tenant_id must not be empty"), so the caller must pass the
// resolved tenant (the shell resolves it once via Auth.ListIdentities).
// event_id == the ledger row id, so dedup survives reconnect exactly like
// the GUI's useStream. OnEvent pokes the named channel so the pane repaints
// as live entries land.
func (r *Registry) FileEdits(cl *client.Clients, tenantID, ownerKind, ownerID string) *stream.Sub[*apiv1.StreamFileEditsResponse] {
	name := "file-edits"
	cfg := stream.Config[*apiv1.StreamFileEditsResponse]{
		Name: name,
		Open: func(ctx context.Context, fromSequence int64) (func() (*apiv1.StreamFileEditsResponse, error), error) {
			if err := guard(cl != nil && cl.FileEdits != nil, "file edits"); err != nil {
				return nil, err
			}
			req := &apiv1.StreamFileEditsRequest{TenantId: tenantID, OwnerKind: ownerKind, OwnerId: ownerID}
			if fromSequence > 0 {
				req.FromSequence = &fromSequence
			}
			s, err := cl.FileEdits.StreamFileEdits(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return client.ConnectRecv(s), nil
		},
		GetEventID:  func(m *apiv1.StreamFileEditsResponse) string { return m.GetEventId() },
		GetSequence: func(m *apiv1.StreamFileEditsResponse) int64 { return m.GetSequence() },
		OnStatus:    r.notify(name), OnError: r.notifyErr(name),
		OnEvent: func(*apiv1.StreamFileEditsResponse) { r.pokeEvent(name)(nil) },
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
		// Read the CURRENT value rather than the woken-up one: the wake-up queue
		// drops on overflow, so an older status could arrive after a newer one was
		// reported. The registry's latest is the truth.
		st := stream.Status(r.LatestStatus(name))
		if st == "" {
			st = stream.Status(v)
		}
		return StatusMsg{Name: name, Status: st}
	}
}
