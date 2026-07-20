// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// handlers are mounted here onto a single mux, wrapped by the
// auth-resolution middleware. Phase 9 adds AuthService + WebhookService
// + the RBAC Connect interceptor.
package api

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/execution"
	"github.com/beardedparrott/orchicon/internal/middleware"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/webhook"
	"github.com/beardedparrott/orchicon/internal/worker"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// 	Dependencies bundles the resources the API layer needs. Constructed
// once by the server and passed to Mount.
type Dependencies struct {
	Pool           *db.Pool
	Log            *slog.Logger
	Subscriber     eventbus.Subscriber
	PolicyEngine   *policy.Engine
	RecoveryEngine *recovery.Engine
	SigNozClient   *telemetry.SigNozClient
	// SigNozUIURL is the base URL for the SigNoz query-service UI + API
	// (default http://localhost:3301). Used by the /signoz reverse proxy
	// so the embedded iframe works same-origin (docs/10 §11).
	SigNozURL string
	// Phase 9: auth + webhooks + blobstore.
	AuthHandler          *auth.Handler
	WebhookDispatcher    *webhook.Dispatcher
	Mode                 config.DeploymentMode
	// ModelDiscoverer enumerates models from opencode CLI.
	ModelDiscoverer   *aigateway.ModelDiscoverer
	MCPDiscoverer     *aigateway.MCPDiscoverer
}

// Mount returns an http.Handler serving the Orchicon API. Generated
// connect-go handlers are registered with the RBAC interceptor applied
// per-RPC (docs/07 §6.3). The whole surface is wrapped by the
// auth-resolution middleware so every tenant-scoped RPC carries
// identity + tenant context into the data-access layer.
func Mount(mux *http.ServeMux, deps Dependencies) http.Handler {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})

	// Phase 9: out-of-band auth HTTP endpoints (OIDC code-flow + dev
	// login + refresh + session — docs/07 §6.1).
	if deps.AuthHandler != nil {
		deps.AuthHandler.Register(mux)
	}

	// The RBAC interceptor applies the per-RPC entitlement check on
	// top of the identity resolved by the auth middleware (docs/07 §6.2).
	rbacInterceptor := middleware.NewRBACInterceptor(deps.Mode)
	interceptorOpt := connect.WithInterceptors(rbacInterceptor)

	// ProjectService (docs/07 §3.1).
	projSvc := project.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewProjectServiceHandler(projSvc, interceptorOpt))

	// WorkerService (docs/07 §3.3).
	workerSvc := worker.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkerServiceHandler(workerSvc, interceptorOpt))

	// WorkItemService (docs/07 §3.2).
	workItemSvc := workitem.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(workItemSvc, interceptorOpt))

	// RuntimeAdapterService (docs/07 §3.7).
	adapterSvc := adapter.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewRuntimeAdapterServiceHandler(adapterSvc, interceptorOpt))

	// ExecutionService (docs/07 §3.8).
	execSvc := execution.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewExecutionServiceHandler(execSvc, interceptorOpt))

	// WorkflowService (docs/07 §3.4).
	workflowSvc := workflow.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(workflowSvc, interceptorOpt))

	// PolicyService (docs/07 §3.5).
	policySvc := policy.NewService(deps.Pool, deps.Log, deps.PolicyEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewPolicyServiceHandler(policySvc, interceptorOpt))

	// RecoveryService (docs/07 §3.6, docs/06).
	recoverySvc := recovery.NewService(deps.Pool, deps.Log, deps.RecoveryEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewRecoveryServiceHandler(recoverySvc, interceptorOpt))

	// TelemetryService (docs/07 §3.9, docs/08 §5).
	telemetrySvc := telemetry.NewService(deps.Pool, deps.SigNozClient, deps.Subscriber)
	mux.Handle(apiv1connect.NewTelemetryServiceHandler(telemetrySvc, interceptorOpt))

	// AIGatewayService (docs/07 §3.10).
	aiGatewaySvc := aigateway.NewService(deps.Pool, deps.Log, deps.Subscriber, deps.ModelDiscoverer, deps.MCPDiscoverer)
	mux.Handle(apiv1connect.NewAIGatewayServiceHandler(aiGatewaySvc, interceptorOpt))

	// Phase 9: AuthService (docs/07 §3.12) — API keys, identities, RBAC
	// roles + bindings, tenants, audit.
	authSvc := auth.NewService(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewAuthServiceHandler(authSvc, interceptorOpt))

	// Phase 9: WebhookService (docs/07 §3.11) — subscriptions + deliveries.
	webhookSvc := webhook.NewService(deps.Pool, deps.Log, deps.WebhookDispatcher, deps.Subscriber)
	mux.Handle(apiv1connect.NewWebhookServiceHandler(webhookSvc, interceptorOpt))

	// SigNoz UI reverse proxy (docs/10 §11): serves the SigNoz frontend
	// same-origin under /signoz so the embedded iframe in the Telemetry
	// page works in all deployment modes (not just Vite dev proxy).
	// Mirrors the Vite config: strips /signoz prefix, forwards to the
	// SigNoz query-service (default localhost:3301). The ModifyResponse
	// rewrites ONLY the document's <base href="/"> to <base href="/signoz/">.
	// SigNoz v0.132 ships its asset <link>/<script> tags with relative
	// paths (./assets/..., css/..., favicon.ico) so the base href is
	// sufficient — every relative URL in the document resolves against
	// /signoz/ and lands back inside the proxied path. The earlier
	// /assets/ -> /signoz/assets/ rewrite was a footgun: it matched the
	// substring inside the already-relative ./assets/ paths and produced
	// ./signoz/assets/..., which the base href then double-prefixed into
	// /signoz/signoz/assets/... — the SPA loaded its own HTML repeatedly
	// instead of the JS bundle, leaving the iframe blank.
	if deps.SigNozURL != "" {
		signozTarget, err := url.Parse(deps.SigNozURL)
		if err == nil {
			signozProxy := httputil.NewSingleHostReverseProxy(signozTarget)
			signozProxy.ErrorLog = nil
			signozProxy.ModifyResponse = func(r *http.Response) error {
				ct := r.Header.Get("Content-Type")
				if !strings.HasPrefix(ct, "text/html") {
					return nil
				}
				// The upstream (SigNoz) may send gzip-compressed bodies.
				// Decompress so the string-based rewrite functions work.
				var reader io.ReadCloser
				switch r.Header.Get("Content-Encoding") {
				case "gzip":
					gr, err := gzip.NewReader(r.Body)
					if err != nil {
						return err
					}
					defer gr.Close()
					reader = gr
				default:
					reader = r.Body
				}
				body, readErr := io.ReadAll(reader)
				if readErr != nil {
					return readErr
				}
				r.Body.Close()
				rewritten := rewriteSigNozHTML(string(body), "/signoz")
				r.Body = io.NopCloser(strings.NewReader(rewritten))
				r.Header.Del("Content-Encoding")
				r.Header.Del("Content-Length")
				return nil
			}
			mux.Handle("/signoz", http.StripPrefix("/signoz", signozProxy))
			mux.Handle("/signoz/", http.StripPrefix("/signoz", signozProxy))
		}
	}

	// Phase 9: wrap with the auth-resolution middleware. It resolves the
	// caller's identity from the bearer token (OIDC access token or API
	// key) and stores identity + tenant in the context (docs/07 §6.3).
	var h http.Handler = mux
	if deps.AuthHandler != nil {
		h = middleware.ResolveAuth(h, deps.AuthHandler.Issuer(), deps.AuthHandler.Resolver(), deps.Mode, deps.Log)
	} else {
		// Dev fallback when auth is not configured: resolve tenant only.
		h = middleware.ResolveTenant(mux)
	}
	_ = blobstore.ErrNotFound
	return h
}
