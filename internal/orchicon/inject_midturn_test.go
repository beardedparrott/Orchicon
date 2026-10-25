package orchicon

// inject_midturn_test.go — an interjection JOINS the running turn and the model sees it at the NEXT
// provider call. It is not held until the turn ends.
//
// THE OPERATOR: "what I would like is the message a user sends gets injected into the current context window
// without an abort, but the model sees it right away. Instead what is happening, is it doesn't get seen until
// the turn is completely stopped."
//
// WHAT IS AND IS NOT POSSIBLE. A provider call's input is fixed for the duration of that call, so "right away"
// cannot mean mid-generation — that is inference, not a gap in this code. It DOES mean the next provider call,
// and a turn is a LOOP of them (one per tool round, plus one forced by an interjection that lands as the model
// is finishing). These tests pin that the note reaches the very next request.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// gatedStream blocks inside Next until release is closed, so a test can act while a round is genuinely in
// flight — which is the only state in which an interjection can join a turn.
type gatedStream struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	events    []Event
	i         int
}

func (s *gatedStream) Next(context.Context) (Event, bool, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.release // returns immediately once the channel is closed
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil
}
func (s *gatedStream) Close() error { return nil }

// injectProvider records every TurnRequest and gates the FIRST round so the test can inject mid-round.
type injectProvider struct {
	mu       sync.Mutex
	requests []TurnRequest
	release  chan struct{}
	started  chan struct{}
	startOne sync.Once
}

func (p *injectProvider) StreamTurn(_ context.Context, req TurnRequest) (TurnStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	n := len(p.requests)
	p.mu.Unlock()
	if n == 1 {
		return &gatedStream{
			started: p.started,
			release: p.release,
			// No tool calls: the model's first round decides to finish. That is the WORST case for an
			// interjection — there is no tool boundary left — and it must still be answered in this turn.
			events: []Event{Finish{StopReason: StopStop}},
		}, nil
	}
	return &chatTestStream{events: []Event{
		TextDelta{Text: "Noted — checking the logs too."},
		Finish{StopReason: StopStop},
	}}, nil
}

func (p *injectProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *injectProvider) Capabilities() Capabilities {
	return Capabilities{Streaming: true}
}

func (p *injectProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *injectProvider) requestAt(i int) TurnRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.requests) {
		return TurnRequest{}
	}
	return p.requests[i]
}

// userTexts returns every USER message's text in a request, for "did the model see it" assertions.
func userTexts(req TurnRequest) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role != RoleUser {
			continue
		}
		var b strings.Builder
		for _, c := range m.Content {
			if c.Text != nil {
				b.WriteString(*c.Text)
			}
		}
		out = append(out, b.String())
	}
	return out
}

func containsText(texts []string, want string) bool {
	for _, s := range texts {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// THE CORE CONTRACT: a message sent while a turn is live does NOT start a second provider turn, and the model
// reads it on the NEXT provider call of the SAME turn.
func TestAnInterjectionReachesTheNextProviderCallOfTheSameTurn(t *testing.T) {
	prov := &injectProvider{release: make(chan struct{}), started: make(chan struct{})}
	b := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-inject", "ask-orchicon:conv-inject")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	bus, err := b.Subscribe(ctx, "conv-inject")
	if err != nil || bus == nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := b.SendTurnMessage(ctx, "conv-inject", sid, "system", "orchicon/ollama/deepseek-v4-flash", "start the work"); err != nil {
		t.Fatalf("first SendTurnMessage: %v", err)
	}

	// WAIT UNTIL THE ROUND IS GENUINELY IN FLIGHT — the state an interjection has to join. Injecting before
	// this would just start a normal turn and prove nothing.
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first round never started; the fixture cannot observe a mid-turn injection")
	}

	// THE INTERJECTION.
	if err := b.SendTurnMessage(ctx, "conv-inject", sid, "system", "orchicon/ollama/deepseek-v4-flash", "also check the logs"); err != nil {
		t.Fatalf("interjection reported an error: %v — it must join the running turn, not fail", err)
	}

	// IT DID NOT START A COMPETING PROVIDER TURN. That was the whole reason the old code held the message.
	if n := prov.count(); n != 1 {
		t.Fatalf("provider calls after the interjection = %d, want 1 — a mid-turn send must join the "+
			"running turn, never start a second stream on the same session", n)
	}

	// Let round 1 finish with NO tool calls: the model has decided to stop, which is exactly where the note
	// used to be stranded until the next turn.
	close(prov.release)
	_ = drainBus(t, bus)

	if n := prov.count(); n != 2 {
		t.Fatalf("provider calls at turn end = %d, want 2 — a pending interjection must force ONE more "+
			"round so the model ANSWERS the note inside this turn", n)
	}
	if got := userTexts(prov.requestAt(1)); !containsText(got, "also check the logs") {
		t.Errorf("the next provider call did not carry the operator's note; user messages were %q — "+
			"the model cannot read what is not in the request", got)
	}
	if got := userTexts(prov.requestAt(1)); !containsText(got, "start the work") {
		t.Errorf("the second request lost the earlier user message; user messages were %q", got)
	}

	// The note is in the session history too, so the NEXT turn replays it rather than losing it.
	b.mu.Lock()
	hist := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if !containsMessageText(hist, "also check the logs") {
		t.Error("the interjection is missing from the committed session history — a restart or a follow-up " +
			"turn would lose it")
	}
}

func containsMessageText(msgs []Message, want string) bool {
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Text != nil && strings.Contains(*c.Text, want) {
				return true
			}
		}
	}
	return false
}

// A SECOND interjection while the first is still queued must not be dropped, and must keep its order: the
// operator's words read in the order they were typed.
func TestQueuedInterjectionsKeepTheirOrder(t *testing.T) {
	b := NewBridge(nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	sid := "ses-order"

	// A turn is "live" for this session (that is all enqueueInjection asks).
	b.mu.Lock()
	if b.chatTurns == nil {
		b.chatTurns = map[string]context.CancelFunc{}
	}
	b.chatTurns[sid] = func() {}
	b.mu.Unlock()

	for _, text := range []string{"first note", "second note"} {
		joined, err := b.enqueueInjection(sid, text, nil)
		if err != nil || !joined {
			t.Fatalf("enqueueInjection(%q) = (%v, %v), want (true, nil)", text, joined, err)
		}
	}

	var working []Message
	if !b.drainInjections(sid, &working) {
		t.Fatal("drainInjections found nothing after two enqueues")
	}
	if len(working) != 2 {
		t.Fatalf("drained %d messages, want 2", len(working))
	}
	if !containsMessageText(working[:1], "first note") || !containsMessageText(working[1:], "second note") {
		t.Error("the notes were drained out of order — the operator's words must read as typed")
	}
	// Drained means drained: a second drain must find nothing, or the note would be sent twice.
	var again []Message
	if b.drainInjections(sid, &again) {
		t.Error("drainInjections returned the same notes a second time — they would appear twice in the context")
	}
}

// THE RACE THE DRAIN POINTS CANNOT COVER: a note that lands after the turn's last drain has no round left to
// join. It must not be stranded in the queue, invisible until some later turn happens to drain it.
func TestAnInjectionLeftAfterTheTurnIsCarriedIntoTheNextTurn(t *testing.T) {
	b := NewBridge(nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	sid := "ses-orphan"

	b.mu.Lock()
	if b.chatTurns == nil {
		b.chatTurns = map[string]context.CancelFunc{}
	}
	b.chatTurns[sid] = func() {}
	b.mu.Unlock()

	if joined, err := b.enqueueInjection(sid, "late note", nil); err != nil || !joined {
		t.Fatalf("enqueueInjection = (%v, %v), want (true, nil)", joined, err)
	}

	orphans := b.takeOrphanInjections(sid)
	if len(orphans) != 1 {
		t.Fatalf("took %d orphans, want 1 — a note left in the queue would be invisible to every turn", len(orphans))
	}
	b.mu.Lock()
	left := b.askInjections[sid]
	b.mu.Unlock()
	if len(left) != 0 {
		t.Errorf("%d messages stayed in the queue after the sweep", len(left))
	}
	// And once the turn marker is gone, a later send is a NORMAL turn rather than an injection.
	b.mu.Lock()
	delete(b.chatTurns, sid)
	b.mu.Unlock()
	if joined, err := b.enqueueInjection(sid, "after the turn", nil); joined || err != nil {
		t.Errorf("enqueueInjection after the turn ended = (%v, %v), want (false, nil) — it must start a "+
			"normal turn instead of queueing against nothing", joined, err)
	}
}
