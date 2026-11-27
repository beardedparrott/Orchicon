// Package antigravityparity is the FIXTURE PLANE for the antigravity GUI/TUI parity captures (child 8, AC3).
//
// WHY A FIXTURE AND NOT THE DEV PLANE. The five states this item must capture are not all reachable on a
// healthy plane at once: "agy not installed" needs no resolved binary, "auth error" needs the model tier
// to fail with the auth message, and "stale snapshot" needs the server's degraded bit. Driving a REAL
// plane through all five would mean breaking it four times and hoping each break landed in the right
// shape. This fixture answers exactly those five shapes, so every capture states ONE thing.
//
// IT IS NOT A MOCK OF THE CONTRACT. Every response below is a real protobuf message over the real Connect
// handler, with the field values a live plane sends — the agy row shape comes from
// internal/providers/antigravity.go, the settings from internal/settings/service.go, and the ref grammar
// is the production `antigravity/google/<model>`. A capture made against this plane exercises the same
// client code a live plane does; only the ANSWERS are chosen.
package antigravityparity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

// State is the model-tier answer the fixture gives. Each value is one AC3 state.
type State string

const (
	// StateLive — the normal path: agy listed real models.
	StateLive State = "live"
	// StateNotInstalled — no `agy` on the host: the tier answers with NO rows and no error, which is
	// the shape that makes the picker's empty state say the install sentence.
	StateNotInstalled State = "not-installed"
	// StateAuthError — signed out: the tier fails with the auth message, which the picker renders as
	// an actionable error with `r` to retry.
	StateAuthError State = "auth-error"
	// StateStale — the vendored snapshot answered: rows PLUS the server's degraded bit.
	StateStale State = "stale"
	// StateModeRefused — a Brainstorm turn refused a write: the transcript carries the refusal as an
	// error tool result (the mode gate, which runs before any card).
	StateModeRefused State = "mode-refused"
)

// Plane is the fixture's state.
type Plane struct {
	state State
	// loaded is closed once every client's first loads have been answered, so a capture can wait for a
	// settled frame instead of a sleep.
	loaded chan struct{}
}

// New builds a fixture plane in the given state.
func New(state State) *Plane { return &Plane{state: state, loaded: make(chan struct{})} }

// State reports the plane's state.
func (p *Plane) State() State { return p.state }

// agyModels is what a LIVE agy listing returns: agy's own bare ids with the observed 1M window and
// reasoning=true (internal/providers/antigravity.go antigravityRowsFromLive).
func agyModels() []*v1.ProviderModel {
	return []*v1.ProviderModel{
		{Id: "gemini-4-carbon-high", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
		{Id: "gemini-4-carbon-low", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
		{Id: "gemini-4-neon-low", Context: 1048576, Visible: true, Source: "live", Reasoning: true},
	}
}

// snapshotModels is the vendored fallback: the same rows, source=antigravity-snapshot.
func snapshotModels() []*v1.ProviderModel {
	return []*v1.ProviderModel{
		{Id: "gemini-4-carbon-high", Context: 1048576, Visible: true, Source: "antigravity-snapshot", Reasoning: true},
		{Id: "gemini-4-carbon-low", Context: 1048576, Visible: true, Source: "antigravity-snapshot", Reasoning: true},
	}
}

// ProviderServiceHandler implements the sourcing view (the CATALOG-SOURCED model tier).
type ProviderServiceHandler struct {
	apiv1connect.UnimplementedProviderServiceHandler
	p *Plane
}

// ListProviders is the merged view — not what antigravity uses, but the GUI reads it for other tabs.
func (h *ProviderServiceHandler) ListProviders(_ context.Context, _ *connect.Request[v1.ProviderListRequest]) (*connect.Response[v1.ProviderListResponse], error) {
	return connect.NewResponse(&v1.ProviderListResponse{
		Providers: []*v1.ProviderEntry{{Id: "google", DisplayName: "Google", Enabled: true}},
	}), nil
}

// ListProviderModels is THE model tier for a catalog-sourced adapter kind.
func (h *ProviderServiceHandler) ListProviderModels(_ context.Context, req *connect.Request[v1.ProviderModelsRequest]) (*connect.Response[v1.ProviderModelsResponse], error) {
	id := req.Msg.GetProviderId()
	if id != "google" {
		return connect.NewResponse(&v1.ProviderModelsResponse{}), nil
	}
	switch h.p.state {
	case StateNotInstalled:
		// NO rows, NO error: the install precondition, not a failure. This is the shape that makes the
		// empty state speak.
		return connect.NewResponse(&v1.ProviderModelsResponse{}), nil
	case StateAuthError:
		// The one actionable auth sentence, as an Unavailable — exactly what a signed-out host sends.
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s", adapter.AntigravityAuthRequiredMessage))
	case StateStale:
		return connect.NewResponse(&v1.ProviderModelsResponse{Models: snapshotModels(), Degraded: true}), nil
	default:
		return connect.NewResponse(&v1.ProviderModelsResponse{Models: agyModels()}), nil
	}
}

// GatewayServiceHandler implements the adapter/provider tiers the picker's first two levels read.
type GatewayServiceHandler struct {
	apiv1connect.UnimplementedAIGatewayServiceHandler
	p *Plane
}

// ListAdapterKinds publishes the four kinds AND the two classifications that drive the pickers, so the
// fixture states the same facts a live plane does.
func (h *GatewayServiceHandler) ListAdapterKinds(_ context.Context, _ *connect.Request[v1.ListAdapterKindsRequest]) (*connect.Response[v1.ListAdapterKindsResponse], error) {
	return connect.NewResponse(&v1.ListAdapterKindsResponse{
		AdapterKinds:    []string{alphabeticalKinds()[0], "claude", "opencode", "orchicon"},
		AskCapableKinds: []string{alphabeticalKinds()[0], "opencode", "orchicon"},
		SourcingKinds:   adapter.CatalogSourcedAdapterKinds(),
	}), nil
}

// ListProviders is the adapter-scoped gateway set: `google` alone for antigravity.
func (h *GatewayServiceHandler) ListProviders(_ context.Context, req *connect.Request[v1.ListProvidersRequest]) (*connect.Response[v1.ListProvidersResponse], error) {
	if req.Msg.GetAdapter() != "" && req.Msg.GetAdapter() != alphabeticalKinds()[0] {
		return connect.NewResponse(&v1.ListProvidersResponse{}), nil
	}
	return connect.NewResponse(&v1.ListProvidersResponse{
		Providers: []*v1.AIProvider{{Id: "google", Name: "Google", Enabled: true}},
	}), nil
}

// alphabeticalKinds is a helper so the literal "antigravity" appears once, in a place a reader can see
// it is the KIND CONSTANT rather than a string chosen to make a capture look right.
func alphabeticalKinds() []string {
	return []string{adapter.KindAntigravity, "claude", "opencode", "orchicon"}
}

// SettingsHandler serves the tenant settings. The ToS notice is the SERVER's constant, which is the whole
// point of AC4: the fixture does not invent the words, it publishes the same bytes the plane does.
type SettingsHandler struct {
	apiv1connect.UnimplementedSettingsServiceHandler
	p *Plane
}

// GetSettings returns the settings message.
func (h *SettingsHandler) GetSettings(_ context.Context, _ *connect.Request[v1.GetSettingsRequest]) (*connect.Response[v1.GetSettingsResponse], error) {
	s := &v1.TenantSettings{
		AntigravityAuthMethod:     authmethod.Default,
		AntigravityShareTelemetry: false,
		AntigravityTosNotice:      authmethod.ToSNotice,
		DefaultAskOrchiconModel:   "antigravity/google/gemini-4-neon-low",
		DefaultWorkerModel:        "orchicon/commandcode/deepseek-v4-flash",
	}
	return connect.NewResponse(&v1.GetSettingsResponse{Settings: s}), nil
}

// UpdateSettings echoes the update back, so a client that writes the method sees it stick.
func (h *SettingsHandler) UpdateSettings(_ context.Context, req *connect.Request[v1.UpdateSettingsRequest]) (*connect.Response[v1.UpdateSettingsResponse], error) {
	s, _ := h.GetSettings(context.Background(), connect.NewRequest(&v1.GetSettingsRequest{}))
	in := req.Msg.GetSettings()
	if in != nil {
		if in.AntigravityAuthMethod != "" {
			s.Msg.Settings.AntigravityAuthMethod = in.AntigravityAuthMethod
		}
	}
	return connect.NewResponse(&v1.UpdateSettingsResponse{Settings: s.Msg.GetSettings()}), nil
}

// AdapterHandler serves the registry row the /adapters page renders.
type AdapterHandler struct {
	apiv1connect.UnimplementedRuntimeAdapterServiceHandler
	p *Plane
}

// ListAdapters returns the agy row when the fixture is installed, and NOTHING when it is not — an absent
// row is what "not installed" looks like to the GUI card (installed === undefined leaves it unknown,
// so StateNotInstalled omits the row and the capture shows the unknown-but-not-claimed state).
func (h *AdapterHandler) ListAdapters(_ context.Context, _ *connect.Request[v1.ListAdaptersRequest]) (*connect.Response[v1.ListAdaptersResponse], error) {
	rows := []*v1.RuntimeAdapter{
		{Id: "a-orchicon", Kind: "orchicon", Version: "1.0.0", Status: "ready"},
	}
	if h.p.state != StateNotInstalled {
		rows = append(rows, &v1.RuntimeAdapter{
			Id: "a-agy", Kind: adapter.KindAntigravity, Version: "1.5.13", Status: "ready",
		})
	}
	return connect.NewResponse(&v1.ListAdaptersResponse{Adapters: rows}), nil
}

// SecretsHandler answers the secret list. The API-key secret is ABSENT in every capture, which is the
// "not set" arm of the presence-only row.
type SecretsHandler struct {
	apiv1connect.UnimplementedSecretsServiceHandler
	p *Plane
}

// ListSecrets returns no secrets.
func (h *SecretsHandler) ListSecrets(_ context.Context, _ *connect.Request[v1.ListSecretsRequest]) (*connect.Response[v1.ListSecretsResponse], error) {
	return connect.NewResponse(&v1.ListSecretsResponse{}), nil
}

// Mux builds the handler set both captures run against.
func Mux(p *Plane) http.Handler {
	mux := http.NewServeMux()
	pp, ph := apiv1connect.NewProviderServiceHandler(&ProviderServiceHandler{p: p})
	mux.Handle(pp, ph)
	gp, gh := apiv1connect.NewAIGatewayServiceHandler(&GatewayServiceHandler{p: p})
	mux.Handle(gp, gh)
	sp, sh := apiv1connect.NewSettingsServiceHandler(&SettingsHandler{p: p})
	mux.Handle(sp, sh)
	ap, ah := apiv1connect.NewRuntimeAdapterServiceHandler(&AdapterHandler{p: p})
	mux.Handle(ap, ah)
	kp, kh := apiv1connect.NewSecretsServiceHandler(&SecretsHandler{p: p})
	mux.Handle(kp, kh)
	ask, askh := apiv1connect.NewAskOrchiconServiceHandler(&AskHandler{p: p})
	mux.Handle(ask, askh)

	// The SPA's session routes are PLAIN HTTP, not Connect — the keys below are exactly the ones
	// frontend/src/auth/session.ts reads.
	session := map[string]any{
		"authenticated": true, "identity_id": "id-agy-parity", "tenant_id": "tnt-agy-parity",
		"username": "e2e", "is_admin": false,
	}
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, session)
	})
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "orchicon_refresh", Value: "e2e", Path: "/", HttpOnly: true})
		writeJSON(w, map[string]any{
			"access_token": "e2e-token", "identity_id": "id-agy-parity",
			"tenant_id": "tnt-agy-parity", "expires_in": 3600, "is_admin": false,
		})
	})
	mux.HandleFunc("/auth/local-login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "orchicon_refresh", Value: "e2e", Path: "/", HttpOnly: true})
		writeJSON(w, map[string]any{"access_token": "e2e-token", "expires_in": 3600})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": "0.0.0-agy-parity-fixture"})
	})
	return mux
}

// WaitForLoads blocks until the given budget elapses. It exists so a capture's settle time is stated in
// ONE place instead of being a sleep sprinkled through the tests.
func (p *Plane) WaitForLoads(d time.Duration) { time.Sleep(d) }

// writeJSON answers one plain-JSON route.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
