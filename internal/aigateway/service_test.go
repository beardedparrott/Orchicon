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
	return NewService(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, kinds)
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
