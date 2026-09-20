package execution

// grouping_fake_test.go — the WORKER LIST on the fake plane, so a pane's grouping can be driven through
// its real fetch rather than by calling the helper directly.
//
// The fake plane did not serve the Worker service at all: the worker LIST was never exercised here, which
// is why a bug in what the pane did with its response could sit unnoticed. Serving it means the fixture
// can hand back categories and assignments the way the real service does, and a test can assert what the
// pane makes of them.
//
// fakeWorkers is a SEPARATE, tiny handler rather than another embed on fakePlane: the workflow and worker
// services share RPC names (AcquireEditLock), so embedding both makes those selectors ambiguous and the
// fixture stops compiling. Only the one RPC this fixture needs is implemented.

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

// ListWorkers answers the way the real service does: the page of workers, PLUS the worker groupings and
// their assignments in the SAME response (worker/service.go enriches it exactly this way, which is how the
// GUI's own screens get them — and what this pane used to throw away).
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
