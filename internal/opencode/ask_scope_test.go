package opencode

// ask_scope_test.go — an Ask turn must resolve the conversation's MCP scope, like every other adapter.
//
// REPORTED as one of three requirements: "Scoped MCP servers added by the user in Conversations should
// work for all adapters."
//
// claude and the native bridge both resolved ScopeConversation (project-owned ∪ conversation-owned)
// for an Ask turn. opencode did not resolve ANYTHING: it took a single pre-built Ask serve whose config
// carries only the built-in Orchicon server, so a server the operator added to a project or to the
// conversation was simply not there — with no error, because "no servers" and "servers I was never
// told about" look identical from inside a session.

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askScopeRecordingResolver records the ref the Ask path resolved.
type askScopeRecordingResolver struct {
	calls  int
	gotRef mcpclient.ScopeRef
	gotTen string
}

func (r *askScopeRecordingResolver) ResolveScope(ctx context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	r.calls++
	r.gotRef = ref
	r.gotTen = tenant.FromContext(ctx)
	return mcpclient.Resolution{}, nil
}

func TestAskResolvesTheConversationScope(t *testing.T) {
	a := newTestAdapter(t)
	rr := &askScopeRecordingResolver{}
	a.SetHostScopeResolver(rr)
	// A POOL IS REQUIRED FOR RESOLUTION TO MEAN ANYTHING: without one there is no serve to put the
	// resolved set on, so askServeFor correctly skips the work. A t.TempDir base keeps the pool from
	// touching the real data dir; ServeFor only builds the entry, it does not start a serve.
	a.SetAskHostServePool(NewHostServePool(a.log, t.TempDir(), "", ProfileInteractive))

	// A turn context carrying what the Ask collector stamps: the conversation, its project, its tenant.
	ctx := tenant.WithID(context.Background(), "tnt_ask")
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{
		ConversationID: "conv-1", ProjectID: "proj-1",
	})

	_ = a.askServeFor(ctx)

	if rr.calls != 1 {
		t.Fatalf("the Ask path resolved the MCP scope %d time(s), want 1.\n"+
			"  Zero means Ask took the pre-built serve and a conversation's own MCP servers never "+
			"reached it — the reported gap on this adapter.", rr.calls)
	}
	if rr.gotRef.Kind != mcpclient.ScopeConversation {
		t.Errorf("resolved kind = %v, want ScopeConversation — the SAME scope the native and claude Ask "+
			"paths use, so all three agree about what a conversation is entitled to", rr.gotRef.Kind)
	}
	if rr.gotRef.ConversationID != "conv-1" || rr.gotRef.ProjectID != "proj-1" {
		t.Errorf("resolved ref = %+v, want conversation=conv-1 project=proj-1", rr.gotRef)
	}
	if rr.gotTen != "tnt_ask" {
		t.Errorf("the resolver saw tenant %q, want tnt_ask — mcpsettings.Resolver REFUSES an unscoped "+
			"context, so a missing tenant here silently resolves the empty set", rr.gotTen)
	}
}

// TestAskWithoutScopeKeepsTheDefaultServe — an unstamped context (a test, or a caller that never set
// one) must behave exactly as before: no resolution, the default Ask serve, no error.
func TestAskWithoutScopeKeepsTheDefaultServe(t *testing.T) {
	a := newTestAdapter(t)
	rr := &askScopeRecordingResolver{}
	a.SetHostScopeResolver(rr)

	_ = a.askServeFor(context.Background())

	if rr.calls != 0 {
		t.Errorf("resolved the MCP scope with no conversation stamped (%d calls) — there is nothing to "+
			"resolve, and an unscoped ResolveScope errors", rr.calls)
	}
}
