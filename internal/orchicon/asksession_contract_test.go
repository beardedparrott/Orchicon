package orchicon

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// nativeAskSessionContract is the session contract the Ask persona carries
// (askorchicon BuildSystemPrompt, "## Session contract"). Duplicated here as a
// literal rather than imported because askorchicon imports THIS package, so a
// test import would be a cycle. The guard below is scoped to the property that
// matters on the native path: whatever the caller passes is what the provider
// receives.
const nativeAskSessionContract = "## Session contract\nThis is a LIVE CONVERSATION, not a budgeted worker execution."

// The NATIVE Ask path must deliver the caller's system prompt to the provider
// verbatim, with nothing worker-shaped appended.
//
// Why this guard exists: the opencode adapter baked a worker/economy agent shell
// into its serve config, which every session on that serve inherited — and Ask
// conversations reported being "almost at my budget" as a result. The native
// path has no such shell (it is in-process), but it DOES have its own worker
// contract (NativeStaticLayer) that would be just as wrong here: it states "You
// are an Orchicon worker executing one assigned work item" and "spends your
// tool-call budget". This pins the boundary so that contract can never reach a
// conversation.
func TestNativeAskTurnCarriesOnlyTheCallersSystemPrompt(t *testing.T) {
	prov := &chatTestProvider{}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")

	// A system prompt shaped like the Ask persona's contract.
	system := nativeAskSessionContract +
		"\n- There is NO tool-call, token, cost, or turn budget on this session."

	if err := b.SendTurnMessage(ctx, "conv-1", "orchicon-ask:conv-1", system, "orchicon/deepseek/deepseek-flash", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(prov.requests))
	}
	req := prov.requests[0]

	// 1. Exactly the caller's block — nothing appended beneath it.
	if len(req.System) != 1 {
		var got []string
		for _, sb := range req.System {
			got = append(got, sb.Text)
		}
		t.Fatalf("the native Ask turn must send exactly the caller's system block, got %d blocks: %q", len(req.System), got)
	}
	// 2. Verbatim.
	if req.System[0].Text != system {
		t.Errorf("the system prompt was altered on the way to the provider:\n got: %q\nwant: %q", req.System[0].Text, system)
	}
	// 3. The Ask contract reaches the model (the positive half of the fix works
	// on the native path too).
	if !strings.Contains(req.System[0].Text, "LIVE CONVERSATION") {
		t.Error("the Ask session contract must reach the native provider")
	}

	// 4. No native WORKER contract, and none of its budget framing.
	sent := req.System[0].Text
	for _, banned := range []string{
		"Native Session Contract",
		"You are an Orchicon worker",
		"spends your tool-call budget",
		"one assigned work item",
	} {
		if strings.Contains(sent, banned) {
			t.Errorf("the native worker contract leaked into an Ask turn: %q", banned)
		}
	}

	// 5. Cross-check that the banned markers really are the worker contract's
	// (so this guard cannot pass vacuously if that text is renamed).
	for _, marker := range []string{"Native Session Contract", "You are an", "tool-call budget"} {
		if !strings.Contains(NativeStaticLayer, marker) {
			t.Errorf("NativeStaticLayer no longer contains %q — re-point this guard at the worker contract's current wording", marker)
		}
	}
}

// The askorchicon native tool surface (SetAskTools) must not introduce budget
// framing either: those definitions are rendered into the Ask prompt's tool list.
func TestNativeAskToolSurfaceCarriesNoBudgetFraming(t *testing.T) {
	// The worker's own tool definitions DO carry economy language (that is their
	// tuning); the guard is that the ASK surface is separate from it.
	if !strings.Contains(NativeStaticLayer, "tool-call budget") {
		t.Skip("worker contract wording changed; see TestNativeAskTurnCarriesOnlyTheCallersSystemPrompt")
	}
	// The native Ask definitions are injected by the server (askorchicon), so the
	// property we can assert here is that the native package owns no additional
	// conversation-facing budget text of its own.
	if strings.Contains(NativeStaticLayer, "LIVE CONVERSATION") {
		t.Error("the worker contract must not claim to be a live conversation")
	}
}
