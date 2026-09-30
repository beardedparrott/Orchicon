package execution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// probeWorkers serves a fixed worker + its version trail, so a test can drive the
// real Get/Load path against the shape the plane returns.
type probeWorkers struct {
	apiv1connect.UnimplementedWorkerServiceHandler
	w  *apiv1.Worker
	vs []*apiv1.WorkerVersion
}

func (p *probeWorkers) GetWorker(context.Context, *connect.Request[apiv1.GetWorkerRequest]) (*connect.Response[apiv1.GetWorkerResponse], error) {
	return connect.NewResponse(&apiv1.GetWorkerResponse{Worker: p.w}), nil
}

func (p *probeWorkers) ListWorkerVersions(context.Context, *connect.Request[apiv1.ListWorkerVersionsRequest]) (*connect.Response[apiv1.ListWorkerVersionsResponse], error) {
	return connect.NewResponse(&apiv1.ListWorkerVersionsResponse{Versions: p.vs}), nil
}

func (p *probeWorkers) ListWorkers(context.Context, *connect.Request[apiv1.ListWorkersRequest]) (*connect.Response[apiv1.ListWorkersResponse], error) {
	return connect.NewResponse(&apiv1.ListWorkersResponse{}), nil
}

// probeWorkerModel builds an Execution screen whose worker reads are served by the
// fixture above. The other services are unimplemented — the worker surfaces touch
// none of them.
func probeWorkerModel(t *testing.T, w *apiv1.Worker, vs []*apiv1.WorkerVersion) *Model {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkerServiceHandler(&probeWorkers{w: w, vs: vs}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SetSize(190, 48)
	return m
}
