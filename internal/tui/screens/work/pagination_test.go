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
	// gotPageSize records the page size the last call asked for.
	gotPageSize int32
}

func (p *pagedPlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.calls++
	p.lastToken = req.Msg.GetPageToken()
	p.gotPageSize = req.Msg.GetPageSize()
	// Honour the REQUESTED page size, defaulting to this fixture's own when the caller omits it.
	pageSize := int(req.Msg.GetPageSize())
	if pageSize <= 0 {
		pageSize = p.pageSize
	}
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
	end := start + pageSize
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
// ROOT CAUSE (second version). The server orders page 1 by the SEQUENCE CHAIN (`sort_order NULLS
// LAST, created_at, id`) and a TUI-created item has NO sort_order, so it lands at the very END —
// beyond page 1 on any tenant with a few hundred items. The screen's answer was to FOLLOW the
// cursors, which is what this test used to pin. That was wrong, because the server's cursor is a
// bare `id > token` while page 1 is chain-ordered: the two orders disagree, so following the cursor
// DUPLICATES rows that were already on page 1 and — worse — MISSES rows that fall into the id-gap
// the cursor skipped past. Measured on the live dev tenant (327 matching items): 200 + 14 rows,
// 9 duplicated, 122 MISSED.
//
// The fix is the GUI's shape: ONE request large enough for a real tenant, no cursor. So what this
// test asserts now is the INTENT — the newest item, which sorts LAST on the server, must be present
// — while the mechanism is a single large page.
func TestWorkItemsFetchGetsTheWholeSetInOneRequest(t *testing.T) {
	// A plane that honours PageSize, so a small page would truncate.
	p := &pagedPlane{pageSize: 10}
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
		t.Fatalf("fetch returned %d items, want all 30", len(items))
	}
	if next != "" {
		t.Fatalf("a fully-consumed list must report no next page, got %q", next)
	}
	// The LAST item in server order must be present — that is the newly created one the operator
	// could not see, and it is the whole reason this fetch asks for a large page.
	if !strings.Contains(items[len(items)-1].Title, "Item ") {
		t.Fatalf("last row = %q", items[len(items)-1].Title)
	}
	// ONE call: no cursor is followed, so the broken page-2 ordering is never reached.
	if p.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 — the screen must not walk the server's cursor", p.calls)
	}
	if p.lastToken != "" {
		t.Fatalf("the request carried a cursor (%q) — it must ask for the whole set", p.lastToken)
	}
}

// The request asks for the RPC's maximum page size, so a real tenant arrives in one request. This is
// the number the GUI sends, which is why the two clients agree about what "the work items" are.
func TestWorkItemsFetchAsksForTheMaximumPage(t *testing.T) {
	p := &pagedPlane{pageSize: 1000}
	p.items = append(p.items, &apiv1.WorkItem{Id: "wi-1", Title: "I", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1"})
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	if _, _, err := m.fetchWorkItems(t.Context(), ""); err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	if p.gotPageSize != workItemPageSize {
		t.Errorf("page size = %d, want %d (the RPC maximum, and the GUI's own number)", p.gotPageSize, workItemPageSize)
	}
}

// The DEFECT ITSELF, as a property of the screen: no duplicate rows, ever.
//
// A server that returns the SAME work item twice (which is precisely what the chain-ordered page 1
// plus the id-ordered page 2 produced) must not produce two rows. This is the operator's report
// stated as an assertion: "There are two of them showing up in the TUI but only one in the GUI."
func TestWorkItemsDoNotRenderDuplicateRows(t *testing.T) {
	p := &pagedPlane{pageSize: 1000}
	twin := &apiv1.WorkItem{
		Id: "01M0NAYG0PKJ7EB7ZKNAQSMF9T", Title: "Fix update-path auto-start",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED,
	}
	p.items = append(p.items, twin, twin) // the same row, twice
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SelectSource(srcWorkItems)
	items, _, err := m.fetchWorkItems(t.Context(), "")
	if err != nil {
		t.Fatalf("fetchWorkItems: %v", err)
	}
	seen := map[string]int{}
	for _, it := range items {
		seen[it.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("work item %s rendered %d times — a work item must appear exactly once", id, n)
		}
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

// The operator: "New work items (parents) are not showing up in the parent drop
// down list on new work items."
//
// Same root cause as the list itself: the dropdown was built from ONE page while
// the server orders NULL sort_order LAST, so a freshly created parent sat on a
// later page and never appeared as a choice.
//
// Same correction as the list: the dropdown asks for the whole set in ONE request rather than walking
// the server's cursor, because that cursor is a bare `id > token` against a chain-ordered page 1 — so
// walking it could append the SAME parent twice (two options with one value) as well as miss rows.
func TestParentOptionsGetEveryItemInOneRequest(t *testing.T) {
	p := &pagedPlane{pageSize: 5}
	for i := 0; i < 12; i++ {
		p.items = append(p.items, &apiv1.WorkItem{
			Id: "wi-" + string(rune('a'+i)), Title: "Parent " + string(rune('A'+i)),
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1",
		})
	}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.New(client.Options{BaseURL: srv.URL})
	opts, kinds, projects := loadParentOptions(context.Background(), cl)

	// The "none" option plus EVERY item.
	if len(opts) != 13 {
		t.Fatalf("parent options = %d, want 13 (none + 12 items)", len(opts))
	}
	// The LAST item in server order (the newest) must be offered — it is the one that was missing.
	last := p.items[len(p.items)-1]
	if opts[len(opts)-1].Value != last.GetId() {
		t.Fatalf("the last item must be offered, got %q", opts[len(opts)-1].Value)
	}
	// The kind and project maps must cover it too, or the form cannot derive the child kind or
	// validate the project.
	if kinds[last.GetId()] != apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC {
		t.Fatalf("kind map missing the last item: %v", kinds[last.GetId()])
	}
	if projects[last.GetId()] != "proj-1" {
		t.Fatalf("project map missing the last item: %q", projects[last.GetId()])
	}
	// ONE call, no cursor — the broken page-2 ordering is never reached.
	if p.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (the whole set in one request)", p.calls)
	}
	if p.lastToken != "" {
		t.Fatalf("the request carried a cursor (%q)", p.lastToken)
	}
}

// A REPEATED id must not become two picker options.
//
// The option list is keyed by id, so a duplicate makes the picker ambiguous — the operator would see
// one parent listed twice and could not tell the two rows apart. The fetch cannot produce a repeat any
// more; this pins the guard in case a future server change does.
func TestParentOptionsNeverRepeatAnItem(t *testing.T) {
	p := &pagedPlane{pageSize: 1000}
	twin := &apiv1.WorkItem{
		Id: "wi-twin", Title: "Same Parent", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1",
	}
	p.items = append(p.items, twin, twin)
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.New(client.Options{BaseURL: srv.URL})
	opts, _, _ := loadParentOptions(context.Background(), cl)

	// The "none" option plus the item ONCE.
	if len(opts) != 2 {
		t.Fatalf("parent options = %d, want 2 (none + the item once)", len(opts))
	}
	seen := map[string]int{}
	for _, o := range opts {
		seen[o.Value]++
	}
	for v, n := range seen {
		if n > 1 {
			t.Errorf("option %q offered %d times — a parent must be listed once", v, n)
		}
	}
}
