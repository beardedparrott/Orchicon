package work

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// pagedPlane serves work items in pages of `pageSize`, counting the calls, so
// pagination can be asserted directly.
type pagedPlane struct {
	apiv1connect.UnimplementedWorkItemServiceHandler
	items    []*apiv1.WorkItem
	pageSize int
	calls    int
	// lastToken records the cursor the last call carried.
	lastToken string
}

func (p *pagedPlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.calls++
	p.lastToken = req.Msg.GetPageToken()
	start := 0
	if tok := req.Msg.GetPageToken(); tok != "" {
		// The cursor is the last id of the previous page.
		for i, w := range p.items {
			if w.GetId() == tok {
				start = i + 1
				break
			}
		}
	}
	end := start + p.pageSize
	next := ""
	if end < len(p.items) {
		next = p.items[end-1].GetId()
	} else {
		end = len(p.items)
	}
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{
		WorkItems:     p.items[start:end],
		NextPageToken: next,
	}), nil
}

// The operator: "It is still not showing new work items. I can see them in the
// GUI but even leaving orch and going back into it, it does NOT show them."
//
// Root cause: the server orders `sort_order NULLS LAST, created_at` and a
// TUI-created item has NO sort_order, so it lands at the very END — past the
// first page on any tenant with a few hundred items. The fetch took ONLY page 1
// with no follow-up, so the item was never in the list, no matter how often it
// was reloaded.
func TestWorkItemsFetchFollowsPagination(t *testing.T) {
	p := &pagedPlane{pageSize: 10}
	// 30 items: the newest (which the server sorts LAST) is only on page 3.
	for i := 0; i < 30; i++ {
		p.items = append(p.items, &apiv1.WorkItem{
			Id: "wi-" + string(rune('a'+i)), Title: "Item " + string(rune('A'+i)),
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1",
			Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		})
	}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SelectSource(srcWorkItems)

	items, next, err := m.fetchWorkItems(t.Context(), "")
	if err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if len(items) != 30 {
		t.Fatalf("fetch returned %d items, want all 30 (pagination must be followed)", len(items))
	}
	if next != "" {
		t.Fatalf("a fully-consumed list must report no next page, got %q", next)
	}
	// The LAST item in server order must be present — that is the newly
	// created one the operator could not see.
	if !strings.Contains(items[len(items)-1].Title, "Item ") {
		t.Fatalf("last row = %q", items[len(items)-1].Title)
	}
	if p.calls < 3 {
		t.Fatalf("expected at least 3 page calls, got %d", p.calls)
	}
}

// A single page still works (no needless extra calls), and the fetch reports
// "no more" so the list is not marked truncated.
func TestWorkItemsFetchSinglePage(t *testing.T) {
	p := &pagedPlane{pageSize: 10}
	for i := 0; i < 3; i++ {
		p.items = append(p.items, &apiv1.WorkItem{Id: "wi-" + string(rune('a'+i)), Title: "I", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1"})
	}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	items, next, err := m.fetchWorkItems(t.Context(), "")
	if err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if len(items) != 3 || next != "" {
		t.Fatalf("items=%d next=%q, want 3 and no cursor", len(items), next)
	}
	if p.calls != 1 {
		t.Fatalf("a short list must take one call, got %d", p.calls)
	}
}
