package aigateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
)

// testSvc builds a Service with a discoverer stub (no opencode binary) and
// the built-in provider catalog, plus an optional adapter-kinds func.
func testSvc(t *testing.T, kinds func() []string) *Service {
	t.Helper()
	return NewService(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, kinds, nil)
}

func TestListAdapterKinds(t *testing.T) {
	svc := testSvc(t, func() []string { return []string{"claude", "opencode"} })
	resp, err := svc.ListAdapterKinds(context.Background(), connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		t.Fatalf("ListAdapterKinds: %v", err)
	}
	got := resp.Msg.AdapterKinds
	if len(got) != 2 || got[0] != "claude" || got[1] != "opencode" {
		t.Fatalf("AdapterKinds = %v, want [claude opencode]", got)
	}
}

func TestListAdapterKindsFallback(t *testing.T) {
	// No kinds func injected (headless/test wiring) → default adapter kind,
	// never an empty list (the picker must not blank).
	svc := testSvc(t, nil)
	resp, err := svc.ListAdapterKinds(context.Background(), connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		t.Fatalf("ListAdapterKinds: %v", err)
	}
	got := resp.Msg.AdapterKinds
	if len(got) != 1 || got[0] != adapter.DefaultAdapterKind {
		t.Fatalf("AdapterKinds = %v, want [%s]", got, adapter.DefaultAdapterKind)
	}
}

func TestListAdapterKindsEmptyKindsFunc(t *testing.T) {
	// A registered dispatcher with no bridges → fall back to the default.
	svc := testSvc(t, func() []string { return nil })
	resp, err := svc.ListAdapterKinds(context.Background(), connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		t.Fatalf("ListAdapterKinds: %v", err)
	}
	if got := resp.Msg.AdapterKinds; len(got) != 1 || got[0] != adapter.DefaultAdapterKind {
		t.Fatalf("AdapterKinds = %v, want [%s]", got, adapter.DefaultAdapterKind)
	}
}

// The published model-tier classification, at the wire level: `claude` is
// dispatchable (adapter_kinds) and CATALOG-SOURCED (sourcing_kinds), yet it is
// NOT Ask-capable (ask_capable_kinds) — the Ask path is not widened by
// catalog sourcing. The GUI banner and the TUI flag both read these fields.
func TestListAdapterKindsPublishesClaudeAsCatalogSourcedButNotAskCapable(t *testing.T) {
	svc := testSvc(t, func() []string { return []string{"claude", "opencode", "orchicon"} })
	svc.SetChatKinds(func() []string { return []string{"opencode", "orchicon"} })
	resp, err := svc.ListAdapterKinds(context.Background(), connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		t.Fatalf("ListAdapterKinds: %v", err)
	}
	contains := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !contains(resp.Msg.AdapterKinds, adapter.KindClaude) {
		t.Errorf("AdapterKinds = %v, want %q (registered kinds are dispatchable)", resp.Msg.AdapterKinds, adapter.KindClaude)
	}
	if contains(resp.Msg.AskCapableKinds, adapter.KindClaude) {
		t.Errorf("AskCapableKinds = %v, must NOT include %q (no ChatTurnClient) — catalog sourcing never widens Ask", resp.Msg.AskCapableKinds, adapter.KindClaude)
	}
	if !contains(resp.Msg.SourcingKinds, adapter.KindClaude) {
		t.Errorf("SourcingKinds = %v, want %q (its models come from the catalog, not opencode)", resp.Msg.SourcingKinds, adapter.KindClaude)
	}
	if contains(resp.Msg.SourcingKinds, adapter.KindOpencode) {
		t.Errorf("SourcingKinds = %v, must NOT include %q (its models ARE opencode-CLI discovery)", resp.Msg.SourcingKinds, adapter.KindOpencode)
	}
}

func TestListProvidersUnfiltered(t *testing.T) {
	svc := testSvc(t, nil)
	resp, err := svc.ListProviders(context.Background(), connect.NewRequest(&apiv1.ListProvidersRequest{}))
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	// Unfiltered = the global gateway provider table, unchanged behavior.
	if len(resp.Msg.Providers) != len(defaultProviders()) {
		t.Fatalf("unfiltered ListProviders = %d providers, want %d", len(resp.Msg.Providers), len(defaultProviders()))
	}
}

func TestListProvidersScopedToAdapter(t *testing.T) {
	svc := testSvc(t, nil)
	adapterKind := "opencode"
	resp, err := svc.ListProviders(context.Background(), connect.NewRequest(&apiv1.ListProvidersRequest{Adapter: &adapterKind}))
	if err != nil {
		t.Fatalf("ListProviders(adapter): %v", err)
	}
	got := resp.Msg.Providers
	// opencode's built-in profiles: anthropic, openai, local, opencode,
	// opencode-go (sorted by the registry).
	if len(got) != 5 {
		t.Fatalf("opencode providers = %v, want 5", got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if p.Id == "" {
			t.Fatal("provider with empty id")
		}
		seen[p.Id] = true
	}
	for _, want := range []string{"anthropic", "openai", "local", "opencode", "opencode-go"} {
		if !seen[want] {
			t.Errorf("provider %q missing from adapter-scoped list %v", want, got)
		}
	}
}

func TestListProvidersUnknownAdapterEmpty(t *testing.T) {
	svc := testSvc(t, nil)
	adapterKind := "no-such-adapter"
	resp, err := svc.ListProviders(context.Background(), connect.NewRequest(&apiv1.ListProvidersRequest{Adapter: &adapterKind}))
	if err != nil {
		t.Fatalf("ListProviders(unknown adapter): %v", err)
	}
	// Unknown adapter → empty provider list (never an error) so the picker
	// renders the stored-ref-unknown state flagged for review.
	if len(resp.Msg.Providers) != 0 {
		t.Fatalf("unknown adapter providers = %v, want empty", resp.Msg.Providers)
	}
}
