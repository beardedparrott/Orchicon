package modelpick

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// contextwindow_fidelity_test.go — the CLIENT-side half of the audit (AC 8).
//
// The adapter segment picks the source of a context-window read and is NEVER
// crossed. The real occurrence this pins: the Ask composer's context-window
// lookup fell back from the native providers view to
// AIGatewayService.ListOpenCodeModels — which SHELLS OUT TO THE OPENCODE BINARY.
// On a box with no opencode that lookup cannot answer at all, so a cross-source
// fallback silently reintroduces the exact dependency the native adapter exists
// to remove.

// fidelityProvider records every PROVIDERS sourcing view read (the native,
// no-binary substrate).
type fidelityProvider struct {
	apiv1connect.UnimplementedProviderServiceHandler
	modelsCalls []string // provider ids read through ProviderService.ListProviderModels
}

func (s *fidelityProvider) ListProviderModels(_ context.Context, req *connect.Request[apiv1.ProviderModelsRequest]) (*connect.Response[apiv1.ProviderModelsResponse], error) {
	s.modelsCalls = append(s.modelsCalls, req.Msg.GetProviderId())
	return connect.NewResponse(&apiv1.ProviderModelsResponse{
		Models: []*apiv1.ProviderModel{{Id: "deepseek-flash", Context: 128000, Visible: true}},
	}), nil
}

// fidelityGateway records every CLI-backed discovery read.
type fidelityGateway struct {
	apiv1connect.UnimplementedAIGatewayServiceHandler
	openCodeCalls []string // "<adapter>/<provider>" read through ListOpenCodeModels
}

func (s *fidelityGateway) ListOpenCodeModels(_ context.Context, req *connect.Request[apiv1.ListOpenCodeModelsRequest]) (*connect.Response[apiv1.ListOpenCodeModelsResponse], error) {
	s.openCodeCalls = append(s.openCodeCalls, req.Msg.GetAdapter()+"/"+req.Msg.GetProvider())
	return connect.NewResponse(&apiv1.ListOpenCodeModelsResponse{
		Models: []*apiv1.OpenCodeModel{{Id: "claude-sonnet-4", Limits: &apiv1.ModelLimits{Context: 200000}}},
	}), nil
}

func fidelityClients(t *testing.T, prov *fidelityProvider, gw *fidelityGateway) *client.Clients {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewProviderServiceHandler(prov))
	mux.Handle(apiv1connect.NewAIGatewayServiceHandler(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
}

// An `orchicon` ref reads ONLY the native providers sourcing view. It must never
// touch ListOpenCodeModels.
func TestContextWindowNativeRefNeverTouchesCLIDiscovery(t *testing.T) {
	prov, gw := &fidelityProvider{}, &fidelityGateway{}
	cl := fidelityClients(t, prov, gw)

	got, err := ContextWindow(context.Background(), cl, "orchicon/deepseek/deepseek-flash")
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if got != 128000 {
		t.Fatalf("window = %d, want the native sourcing value 128000", got)
	}
	if len(gw.openCodeCalls) != 0 {
		t.Fatalf("an orchicon ref CROSSED into CLI discovery (the removed dependency): %v", gw.openCodeCalls)
	}
	if len(prov.modelsCalls) != 1 || prov.modelsCalls[0] != "deepseek" {
		t.Fatalf("the native providers view must be the single source read, got %v", prov.modelsCalls)
	}
}

// The recorded EXCEPTION: a legacy `opencode/...` ref MAY read the CLI, because
// for THAT adapter the CLI is the authority on its own models (ADR-0004; the
// picker has always worked this way).
func TestContextWindowOpencodeRefReadsCLIDiscovery(t *testing.T) {
	prov, gw := &fidelityProvider{}, &fidelityGateway{}
	cl := fidelityClients(t, prov, gw)

	got, err := ContextWindow(context.Background(), cl, "opencode/anthropic/claude-sonnet-4")
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if got != 200000 {
		t.Fatalf("window = %d, want the CLI discovery value 200000", got)
	}
	if len(prov.modelsCalls) != 0 {
		t.Fatalf("an opencode ref must not read the native providers view: %v", prov.modelsCalls)
	}
	if len(gw.openCodeCalls) != 1 || gw.openCodeCalls[0] != "opencode/anthropic" {
		t.Fatalf("the CLI discovery read must carry the ref's OWN adapter+provider, got %v", gw.openCodeCalls)
	}
}

// A ref with no resolvable provider/model yields no window and reads NEITHER
// source (it must not guess a denominator).
func TestContextWindowUnresolvableRefReadsNeitherSource(t *testing.T) {
	prov, gw := &fidelityProvider{}, &fidelityGateway{}
	cl := fidelityClients(t, prov, gw)

	got, err := ContextWindow(context.Background(), cl, "llama3")
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if got != 0 {
		t.Fatalf("window = %d, want 0 for a partial ref", got)
	}
	if len(prov.modelsCalls) != 0 || len(gw.openCodeCalls) != 0 {
		t.Fatalf("a partial ref must read neither source (providers=%v cli=%v)", prov.modelsCalls, gw.openCodeCalls)
	}
}
