package execution

// grouping_fake_test.go — the WORKER LIST on the fake plane, so a pane's grouping can be driven
// through its real fetch rather than by calling the helper directly.

import (
	"context"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// fakeWorkers forwards the worker LIST to the fixture (see the workerItems block on fakePlane).
type fakeWorkers struct {
	apiv1connect.UnimplementedWorkerServiceHandler
	plane *fakePlane
}

// ListWorkers answers the way the real service does: the page of workers, PLUS the worker groupings
// and their assignments in the SAME response (worker/service.go enriches it exactly this way.
func (w *fakeWorkers) ListWorkers(_ context.Context, _ *connect.Request[apiv1.ListWorkersRequest]) (*connect.Response[apiv1.ListWorkersResponse], error) {
	p := w.plane
	p.mu.Lock()
	defer p.mu.Unlock()
	return connect.NewResponse(&apiv1.ListWorkersResponse{
		Items:       p.workerItems,
		Categories:  p.workerCategories,
		Assignments: p.workerAssignments,
	}), nil
}
