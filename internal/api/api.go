// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// handlers are mounted here onto a single mux, wrapped by the
// tenant-resolution middleware. v0.1 mounts ProjectService; later
// phases add the remaining services.
package api

import (
	"log/slog"
	"net/http"

	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/middleware"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/worker"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// Dependencies bundles the resources the API layer needs. Constructed
// once by the server and passed to Mount.
type Dependencies struct {
	Pool       *db.Pool
	Log        *slog.Logger
	Subscriber eventbus.Subscriber
}

// Mount returns an http.Handler serving the Orchicon API. Generated
// connect-go handlers are registered as they are added. The whole
// surface is wrapped by the tenant-resolution middleware so every
// tenant-scoped RPC carries tenant context into the data-access layer.
func Mount(mux *http.ServeMux, deps Dependencies) http.Handler {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})

	// ProjectService (docs/07 §3.1). The Vite dev-server proxy
	// (frontend/vite.config.ts) forwards /orchicon.api.v1 paths here,
	// so no CORS headers are needed in dev (docs/10 §9).
	projSvc := project.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewProjectServiceHandler(projSvc))

	// WorkerService (docs/07 §3.3). Worker CRUD + versioning lifecycle
	// (publish/deprecate/retire) + edit locks for the visual editor.
	workerSvc := worker.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkerServiceHandler(workerSvc))

	// WorkItemService (docs/07 §3.2). Work item CRUD + dependency DAG
	// (recursive CTE cycle detection — docs/09 §11) + worker assignment.
	workItemSvc := workitem.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(workItemSvc))

	return middleware.ResolveTenant(mux)
}
