package work

// projectfilter_test.go — THE QUERY CARRIES THE WORKSPACE, not just the field.
//
// The operator: "When I am viewing work items in the TUI, it should be filtered by current project selected."
//
// ListWorkItemsRequest has carried project_id the whole time; this screen simply never set it. So a test that
// only asserts the model's field would pass over exactly the bug that was there. These tests read the REQUEST
// the fetch builds, which is the thing that decides what the operator sees.

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

// projectRecordingPlane records the project_id every ListWorkItems call carries.
type projectRecordingPlane struct {
	apiv1connect.UnimplementedWorkItemServiceHandler
	// gotProjects records the ProjectId of each call, in order.
	gotProjects []string
}

func (p *projectRecordingPlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.gotProjects = append(p.gotProjects, req.Msg.GetProjectId())
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{}), nil
}

// filterPlane builds a screen wired to a recording plane.
func filterPlane(t *testing.T) (*Model, *projectRecordingPlane) {
	t.Helper()
	plane := &projectRecordingPlane{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(plane))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	return m, plane
}

// THE FETCH ASKS FOR ONE PROJECT'S ITEMS. Without the filter the plane is asked for everything, which is
// 228 rows across unrelated projects in this tenant.
func TestFetchWorkItemsAsksForTheActiveProject(t *testing.T) {
	m, plane := filterPlane(t)
	m.SetActiveProject("p-orch")
	if _, _, err := m.fetchWorkItems(t.Context(), ""); err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if len(plane.gotProjects) == 0 {
		t.Fatal("no ListWorkItems call was made")
	}
	for _, got := range plane.gotProjects {
		if got != "p-orch" {
			t.Errorf("ListWorkItems carried project_id = %q, want p-orch — the pane is not actually filtered",
				got)
		}
	}
}

// AND WITH NO WORKSPACE CHOSEN IT ASKS FOR EVERYTHING, which is the pre-existing behaviour and must not become
// an accidental filter that hides every work item on All projects.
func TestAnEmptyProjectFilterAsksForEverything(t *testing.T) {
	m, plane := filterPlane(t)
	if _, _, err := m.fetchWorkItems(t.Context(), ""); err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if len(plane.gotProjects) == 0 {
		t.Fatal("no ListWorkItems call was made")
	}
	for _, got := range plane.gotProjects {
		if got != "" {
			t.Errorf("ListWorkItems carried project_id = %q with no workspace selected, want empty", got)
		}
	}
}

// THE ARCHIVE VIEW IS FILTERED TOO. It makes its own ListWorkItems call for the active page (the archive
// tree's ghost anchors), and leaving that one unfiltered would resolve anchors from other projects' rows.
func TestTheArchiveViewsActivePageIsAlsoFiltered(t *testing.T) {
	m, plane := filterPlane(t)
	m.SetActiveProject("p-orch")
	m.viewMu.Lock()
	m.view = viewArchive
	m.viewMu.Unlock()
	if _, _, err := m.fetchWorkItems(t.Context(), ""); err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if len(plane.gotProjects) < 2 {
		t.Fatalf("the archive view made %d call(s), want 2 (archived page + active page)", len(plane.gotProjects))
	}
	for i, got := range plane.gotProjects {
		if got != "p-orch" {
			t.Errorf("call %d carried project_id = %q, want p-orch", i, got)
		}
	}
}

// AN UNCHANGED FILTER IS NOT RE-FETCHED. The shell pushes the workspace in on every rebind pass, which happens
// on every dispatch; reloading the work-items page on that cadence would be constant load for no change.
//
// The assertion is the CALL COUNT, not the returned cmd: "returns nil" is satisfied by an implementation that
// reloads nothing at all, which would be the opposite bug.
func TestAnUnchangedProjectFilterDoesNotRefetch(t *testing.T) {
	m, plane := filterPlane(t)
	m.SetActiveProject("p-orch")
	plane.gotProjects = nil
	if cmd := m.SetActiveProject("p-orch"); cmd != nil {
		// Run it if it were returned, so the count below is honest either way.
		_ = cmd()
	}
	if _, _, err := m.fetchWorkItems(t.Context(), ""); err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	// Exactly one call: the explicit fetch. A reload from the unchanged Set would have made it two.
	if len(plane.gotProjects) != 1 {
		t.Errorf("an unchanged project filter made %d extra call(s); every dispatch would re-fetch the page",
			len(plane.gotProjects)-1)
	}
}

// AND CLEARING THE FILTER DOES REFETCH, so the pane stops showing one project's items when the operator moves to
// All projects.
func TestClearingTheProjectFilterRefetches(t *testing.T) {
	m, _ := filterPlane(t)
	m.SetActiveProject("p-orch")
	if cmd := m.SetActiveProject(""); cmd == nil {
		t.Error("clearing the filter returned no reload — the pane would keep one project's items after " +
			"switching to All projects")
	}
}
