package aigateway_test

// ask_kinds_antigravity_test.go — AC1: an Ask-capable antigravity bridge reaches the wire.
//
// THE CLAIM IS END-TO-END AND THAT IS THE POINT. "antigravity is in ask_capable_kinds" is not a registry
// entry anybody sets: Dispatcher.ChatKinds() is a pure TYPE ASSERTION over the registered bridges, and
// AIGatewayService.ListAdapterKinds copies that function's answer. So this test builds a REAL Dispatcher,
// registers the real antigravity bridge, and reads the RPC response — which is exactly what a client's model
// picker receives. If the bridge ever stopped satisfying scheduler.ChatTurnClient, this test goes red and no
// amount of wiring elsewhere would save it.
//
// IT IS AN EXTERNAL TEST PACKAGE (aigateway_test) because internal/antigravity imports internal/aigateway
// (for the usage/pricing path), so an in-package test importing it would be an import cycle.
//
// The claude control case is in service_test.go and stays: claude has no ChatTurnClient on this branch, so
// catalog sourcing must not widen Ask. Antigravity is the opposite — its bridge DOES implement it.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/antigravity"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestListAdapterKindsIncludesAntigravityAsAskCapable is AC1, read off the RPC response.
func TestListAdapterKindsIncludesAntigravityAsAskCapable(t *testing.T) {
	// THE REAL DISPATCHER, with the real bridge — no stub kind list anywhere in this path.
	disp := scheduler.NewDispatcher()
	disp.Register(adapter.KindAntigravity, antigravity.New(slog.New(slog.NewTextHandler(io.Discard, nil))))

	// The wiring the server performs (server.go: AdapterChatKinds: dispatcher.ChatKinds).
	svc := aigateway.NewService(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, disp.Kinds, nil)
	svc.SetChatKinds(disp.ChatKinds)

	resp, err := svc.ListAdapterKinds(context.Background(), connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		t.Fatalf("ListAdapterKinds: %v", err)
	}

	// The RPC response itself, verbatim: this is what a client's model picker receives, and it is the
	// acceptance evidence for "antigravity appears in ask_capable_kinds".
	if wire, jerr := json.Marshal(resp.Msg); jerr == nil {
		t.Logf("ListAdapterKinds JSON: %s", wire)
	}

	contains := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !contains(resp.Msg.AdapterKinds, adapter.KindAntigravity) {
		t.Errorf("AdapterKinds = %v, want %q (the bridge is registered and dispatchable)", resp.Msg.AdapterKinds, adapter.KindAntigravity)
	}
	if !contains(resp.Msg.AskCapableKinds, adapter.KindAntigravity) {
		t.Errorf("ask_capable_kinds = %v, must include %q — its bridge implements scheduler.ChatTurnClient",
			resp.Msg.AskCapableKinds, adapter.KindAntigravity)
	}

	// The direct statement of the same fact, so a failure above names its own cause.
	if kinds := disp.ChatKinds(); !contains(kinds, adapter.KindAntigravity) {
		t.Fatalf("Dispatcher.ChatKinds() = %v, want %q", kinds, adapter.KindAntigravity)
	}
}

// TestAntigravityBridgeSatisfiesTheAskCapabilities pins the WHOLE optional set the Ask path type-asserts, not
// just ChatTurnClient: askorchicon also asserts SessionQueuesPrompts (to decide whether an interjection joins
// the running turn) and ChatToolRestrictor (to report the mode boundary as enforced rather than prose-only).
// A bridge that silently dropped one of them would behave differently with no error anywhere.
func TestAntigravityBridgeSatisfiesTheAskCapabilities(t *testing.T) {
	var bridge any = antigravity.New(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, ok := bridge.(scheduler.ChatTurnClient); !ok {
		t.Error("antigravity.Bridge does not implement scheduler.ChatTurnClient — it would disappear from Ask")
	}
	queuer, ok := bridge.(scheduler.SessionQueuesPrompts)
	if !ok {
		t.Fatal("antigravity.Bridge does not implement scheduler.SessionQueuesPrompts")
	}
	// agy defines NO way to add a message to a running turn (the acp Session serialises prompts), so the
	// honest answer is false: claiming true would make an operator's interjection look folded-in when it is
	// really queued behind the running turn.
	if queuer.QueuesPrompts() {
		t.Error("QueuesPrompts() = true, but agy cannot accept a message into a running turn (it must be false)")
	}
	if _, ok := bridge.(scheduler.ChatToolRestrictor); !ok {
		t.Error("antigravity.Bridge does not implement scheduler.ChatToolRestrictor — the mode boundary would " +
			"be reported as PROSE ONLY on every turn even though it is enforced in the permission handler")
	}
	if _, ok := bridge.(scheduler.SessionOwnerKind); !ok {
		t.Error("antigravity.Bridge does not implement scheduler.SessionOwnerKind")
	}
	// CloseAsk is not an interface — it is the shutdown seam the server calls by name. Assert it via a
	// typed interface so dropping the method is a compile error here rather than a leak at shutdown.
	var _ interface{ CloseAsk() } = antigravity.New(nil)
}
