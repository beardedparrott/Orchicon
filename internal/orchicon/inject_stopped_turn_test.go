package orchicon

// inject_stopped_turn_test.go — a send must NEVER be absorbed into a turn that has been aborted.
//
// THE OPERATOR'S SYMPTOM, and why this is a test rather than a comment: they pressed Stop, sent their
// next message, and nothing ever answered it. Both sends logged "interjection joined the running turn"
// and returned nil, the conversation's session history ended with TWO CONSECUTIVE UNANSWERED USER
// MESSAGES, and no error was reported anywhere. A hang with no error in it is the worst shape a failure
// can take, because there is nothing to act on.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// errStopForTest is the abort cause this fixture issues. askorchicon's errUserStop is a different
// package's sentinel; the property under test is that WHATEVER cause was given comes back to the caller.
var errStopForTest = errors.New("turn stopped by the user")

// swallowProvider blocks inside the FIRST round until release is closed, then ends the round. It stands
// in for the provider that went silent: the abort cancels the context, but the drain goroutine is held in
// the read and cannot return for as long as the release channel stays open.
type swallowProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *swallowProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	return &gatedStream{
		started: make(chan struct{}),
		release: p.release,
		events:  []Event{Finish{StopReason: StopStop}},
	}, nil
}

func (p *swallowProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *swallowProvider) Capabilities() Capabilities {
	return Capabilities{Streaming: true}
}

func (p *swallowProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// stopAndAssertNormalSend aborts the session exactly as the Stop button does and then asserts that the
// operator's next message is either accepted as a NORMAL turn or rejected — never silently absorbed.
func stopAndAssertNormalSend(t *testing.T, b *NativeBridge, prov *swallowProvider, ctx context.Context, sid string) {
	t.Helper()

	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the provider; the fixture cannot observe a joinable turn")
	}

	// THE STOP. A cancelled cause, exactly as the collector issues it (chat.go: AbortConversationSession
	// with errStopForTest), so the assertion also covers the cause being carried into the refusal.
	stopCtx, stop := context.WithCancelCause(context.Background())
	stop(errStopForTest)
	if err := b.AbortConversationSession(stopCtx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}

	// The drain goroutine is STILL RUNNING — gatedStream's buffer is empty only after release. This is the
	// window in which the defect lived, and the whole point of the fixture is to own it: the release
	// channel is closed by the caller AFTER these assertions.
	joined, err := b.enqueueInjection(sid, "are you waiting on?", nil)
	if joined {
		t.Errorf("enqueueInjection reported JOINED a turn that was already aborted — the message would be " +
			"queued into a turn that is about to die, and the operator would see a spinner with no error")
	}

	// And through the public entry point, which is what the collector actually calls.
	sendErr := b.SendTurnMessage(ctx, "conv-stopped", sid, "system", "orchicon/ollama/deepseek-v4-flash", "are you waiting on?")
	if sendErr == nil {
		b.mu.Lock()
		registered := b.chatTurns[sid] != nil
		b.mu.Unlock()
		if !registered {
			t.Error("SendTurnMessage returned NIL without starting a turn — the message was accepted and " +
				"stranded: nothing is registered to drain it, and no error reaches the operator")
		}
		return
	}
	if err != nil && !errors.Is(err, errStopForTest) {
		t.Errorf("the refusal does not carry the abort cause: %v — the operator needs to be told WHY the "+
			"turn ended, not just that it did", err)
	}
	t.Logf("a send onto an aborted turn is refused with: %v", sendErr)
}

// THE PROPERTY: a turn that has been aborted can no longer absorb the operator's next message.
func TestASendIsNeverAbsorbedIntoAnAbortedTurn(t *testing.T) {
	prov := &swallowProvider{started: make(chan struct{}), release: make(chan struct{})}
	b := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-stopped", "ask-orchicon:conv-stopped")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if _, err := b.Subscribe(ctx, "conv-stopped"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.SendTurnMessage(ctx, "conv-stopped", sid, "system", "orchicon/ollama/deepseek-v4-flash", "start the work"); err != nil {
		t.Fatalf("first SendTurnMessage: %v", err)
	}

	stopAndAssertNormalSend(t, b, prov, ctx, sid)

	close(prov.release)
}

// AND THE MESSAGE IS NOT LOST: refusing it is only acceptable because the caller can send again. Once the
// aborted turn's goroutine has returned, a fresh send must start a real turn rather than being refused
// forever — otherwise the fix would trade a hang for a dead conversation.
func TestASessionAcceptsAFreshTurnOnceTheAbortedTurnReturns(t *testing.T) {
	prov := &swallowProvider{started: make(chan struct{}), release: make(chan struct{})}
	b := NewBridge(ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-stopped", "ask-orchicon:conv-stopped")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if _, err := b.Subscribe(ctx, "conv-stopped"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.SendTurnMessage(ctx, "conv-stopped", sid, "system", "orchicon/ollama/deepseek-v4-flash", "start the work"); err != nil {
		t.Fatalf("first SendTurnMessage: %v", err)
	}
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the provider")
	}
	stopCtx, stop := context.WithCancelCause(context.Background())
	stop(errStopForTest)
	if err := b.AbortConversationSession(stopCtx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}

	// Let the aborted turn's goroutine finish and clear its own registration.
	close(prov.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		stopping := b.chatTurnStopping[sid] != nil
		b.mu.Unlock()
		if !stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the aborted turn never cleared its registration — the session would refuse every " +
				"later send, which is a dead conversation rather than a fixed one")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := b.SendTurnMessage(ctx, "conv-stopped", sid, "system", "orchicon/ollama/deepseek-v4-flash", "try again"); err != nil {
		t.Fatalf("a send after the aborted turn returned was refused: %v — the conversation can never "+
			"continue", err)
	}
	if n := prov.count(); n < 2 {
		t.Errorf("provider calls = %d, want at least 2 — the fresh message never reached the model", n)
	}
}
