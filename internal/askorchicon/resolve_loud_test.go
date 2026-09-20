package askorchicon

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// resolve_loud_test.go — the LOUD-FAILURE half of the shared transport
// resolution audit (AC 3 / AC 6).
//
// An unknown or unregistered adapter kind must fail AT RESOLUTION TIME with an
// actionable error — never silently fall back to the host opencode serve. And
// the nil-dispatcher host-serve fallback must be bounded to tests/pre-wiring:
// with a dispatcher wired, nothing falls through to the host serve.

// loudStubBridge is a registered-but-inert adapter bridge that also implements
// scheduler.ChatTurnClient (the Ask chat capability). It exists so the
// dispatcher has REAL registrations to enumerate when a resolution fails; none
// of its methods are ever driven.
type loudStubBridge struct{}

func (loudStubBridge) Start(context.Context, db.ExecutionRow, scheduler.ExecutionManifest, scheduler.ExecutionCallbacks) error {
	return nil
}

func (loudStubBridge) Subscribe(context.Context, string) (scheduler.SessionBus, error) {
	return nil, nil
}

func (loudStubBridge) CreateConversationSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (loudStubBridge) SendTurnMessage(context.Context, string, string, string, string, string) error {
	return nil
}

func (loudStubBridge) AbortConversationSession(context.Context, string) error { return nil }

func (loudStubBridge) ReplyPermission(context.Context, string, string) error { return nil }

// `claude` is a LIVE case, not a hypothetical: it is DECLARED in the builtin
// provider catalog (internal/adapter/providers.go AddAdapterKind("claude", …)),
// so a `claude/anthropic/…` ref parses as a known adapter — but no bridge
// registers that kind. Resolution must then fail loudly with the registered
// kinds named, and must NOT quietly hand back the host opencode serve.
func TestResolveChatClientFailsLoudForADeclaredButUnregisteredKind(t *testing.T) {
	d := scheduler.NewDispatcher()
	d.Register("opencode", loudStubBridge{})
	d.Register("orchicon", loudStubBridge{})

	s := &Service{}
	s.SetDispatcher(d)
	// A host serve IS available — the point is that it is not used as a
	// fallback for a kind the dispatcher could not resolve.
	s.testServeClient = &ownerKindClient{kind: "opencode"}

	const ref = "claude/anthropic/claude-sonnet-4"
	if got := adapter.AdapterKind(ref); got != "claude" {
		t.Fatalf("the declared kind must parse from the ref: AdapterKind(%q) = %q, want %q", ref, got, "claude")
	}

	client, err := s.resolveChatClient("", ref)
	if err == nil {
		t.Fatal("a declared-but-unregistered adapter kind must fail at resolution — it must never fall back to the host serve")
	}
	if client != nil {
		t.Fatalf("a failed resolution must return no client, got %T", client)
	}
	for _, want := range []string{`"claude"`, "registered kinds", `"opencode"`, `"orchicon"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the resolution error must name %q so it is actionable: %v", want, err)
		}
	}
}

// AC 6: with a dispatcher wired, resolution NEVER falls through to the host
// opencode serve — even when a host serve is sitting right there. This is the
// bounded-ness proof for the nil-dispatcher branch.
func TestResolveChatClientNeverUsesHostServeWhenADispatcherIsWired(t *testing.T) {
	d := scheduler.NewDispatcher()
	d.Register("orchicon", loudStubBridge{})

	s := &Service{}
	s.SetDispatcher(d)
	host := &ownerKindClient{kind: "opencode"}
	s.testServeClient = host

	got, err := s.resolveChatClient("", "orchicon/deepseek/deepseek-flash")
	if err != nil {
		t.Fatalf("a registered, chat-capable kind must resolve via the dispatcher: %v", err)
	}
	if _, isHost := got.(*ownerKindClient); isHost {
		t.Fatal("resolution fell through to the host serve client despite a wired dispatcher")
	}
}

// AC 6: the nil-dispatcher branch is only reached when NO dispatcher is wired,
// and even there an absent host serve fails EXPLICITLY rather than inventing a
// transport (no nil client handed back as though it were usable).
func TestResolveChatClientFailsLoudWithNoDispatcherAndNoHostServe(t *testing.T) {
	s := &Service{}

	client, err := s.resolveChatClient("", "opencode/anthropic/claude-sonnet-4")
	if err == nil {
		t.Fatal("with no dispatcher and no host serve there is no transport — resolution must fail")
	}
	if client != nil {
		t.Fatalf("a failed resolution must return no client, got %T", client)
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("the error must say the transport is unavailable: %v", err)
	}
}

// The nil-dispatcher branch still returns the injected test client (the shape
// every existing Ask test relies on) — so the bounded fallback is a real
// pre-wiring path, not dead code that could be deleted without noticing.
func TestResolveChatClientNilDispatcherUsesTheTestClient(t *testing.T) {
	s := &Service{}
	injected := &ownerKindClient{kind: "opencode"}
	s.testServeClient = injected

	got, err := s.resolveChatClient("", "orchicon/deepseek/deepseek-flash")
	if err != nil {
		t.Fatalf("the pre-wiring path must resolve the injected client: %v", err)
	}
	if got != scheduler.ChatTurnClient(injected) {
		t.Fatalf("got %T, want the injected test client", got)
	}
}
