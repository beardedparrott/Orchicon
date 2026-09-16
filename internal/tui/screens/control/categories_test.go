package control

// categories_test.go — the Control tab's grouping management (create / rename / delete).
//
// The operator: "conversation/workflows/worker category groupings don't exist (create, rename, and
// delete groupings)."
//
// These tests exist because the surface SHIPPED WITHOUT ANY: it called m.cl.Categories directly rather
// than through the package's rpc* thunks, so there was no seam to test through. Routing the writes
// through thunks (screen.go) is what makes the three acts assertable at all — and the payloads matter,
// because a create that sends the wrong target_type files the grouping under the wrong tab in the GUI
// with no error anywhere.

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// categorySpy records the category RPCs a test exercises, so an assertion can name the call AND its
// payload (an error alone cannot tell "created the right grouping" from "created something").
type categorySpy struct {
	created   []*apiv1.CreateCategoryRequest
	updated   []*apiv1.UpdateCategoryRequest
	deleted   []string
	listCalls []apiv1.CategoryTargetType
	listErr   error
	// byTarget is what the list thunk answers per target type.
	byTarget map[apiv1.CategoryTargetType][]*apiv1.Category
}

func (s *categorySpy) install(m *Model) {
	m.rpcListCategories = func(_ context.Context, t apiv1.CategoryTargetType) ([]*apiv1.Category, []*apiv1.CategoryAssignment, error) {
		s.listCalls = append(s.listCalls, t)
		if s.listErr != nil {
			return nil, nil, s.listErr
		}
		return s.byTarget[t], nil, nil
	}
	m.rpcCreateCategory = func(_ context.Context, r *apiv1.CreateCategoryRequest) error {
		s.created = append(s.created, r)
		return nil
	}
	m.rpcUpdateCategory = func(_ context.Context, r *apiv1.UpdateCategoryRequest) error {
		s.updated = append(s.updated, r)
		return nil
	}
	m.rpcDeleteCategory = func(_ context.Context, id string) error {
		s.deleted = append(s.deleted, id)
		return nil
	}
}

// loadCategories runs the pane's fetch through the source loader and returns the rows the operator
// would see.
func loadCategories(t *testing.T, m *Model) []kit2.Item {
	t.Helper()
	items, _, err := m.fetchCategories(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchCategories: %v", err)
	}
	return items
}

// TestCategoryFetchFansOutOverEveryTargetType pins the shape of the load: THREE requests, one per
// target type, because the pane lists all three together. A load that skipped a type would render a
// pane that looks complete while silently hiding a whole kind of grouping.
func TestCategoryFetchFansOutOverEveryTargetType(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{byTarget: map[apiv1.CategoryTargetType][]*apiv1.Category{
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER: {
			{Id: "cat_w", Name: "Backend", TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER},
		},
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION: {
			{Id: "cat_c", Name: "Research", TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION},
		},
	}}
	spy.install(m)

	items := loadCategories(t, m)

	if len(spy.listCalls) != 3 {
		t.Fatalf("the load made %d list calls, want one per target type (3): %v", len(spy.listCalls), spy.listCalls)
	}
	// Grouped by type, with a heading row per type — so the row's own meta does not have to repeat it.
	var headings, rows []string
	for _, it := range items {
		if it.ID == "" {
			headings = append(headings, it.Title)
			continue
		}
		rows = append(rows, it.Title)
	}
	if len(headings) != 2 {
		t.Fatalf("want a heading per NON-EMPTY type (2), got %d: %v", len(headings), headings)
	}
	if strings.Join(rows, ",") != "Backend,Research" {
		t.Fatalf("rows = %v, want the two groupings grouped by type", rows)
	}
	if !strings.Contains(headings[0], "worker") || !strings.Contains(headings[1], "conversation") {
		t.Fatalf("headings = %v, want one naming workers and one naming conversations", headings)
	}
	// THE EMPTY-STATE ROW must not appear when there are rows: it is for an empty pane only, and
	// leaving it in would tell the operator "no groupings yet" while listing two.
	for _, it := range items {
		if strings.Contains(it.Title, "no groupings yet") {
			t.Fatalf("the empty-state row appeared alongside real rows: %v", items)
		}
	}
}

// TestCategoryFetchEmptyPaneSaysSo: an empty pane must say the operator has to create one, rather
// than looking broken.
func TestCategoryFetchEmptyPaneSaysSo(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{byTarget: map[apiv1.CategoryTargetType][]*apiv1.Category{}}
	spy.install(m)

	items := loadCategories(t, m)

	if len(items) != 1 || !strings.Contains(items[0].Title, "no groupings yet") {
		t.Fatalf("an empty pane must state that nothing exists yet, got %v", items)
	}
}

// TestCategoryCreateSendsTheChosenTargetType: the target type is what makes a grouping a WORKER
// grouping or a CONVERSATION one, so a create that sent the wrong one would file it under the wrong
// list with no error anywhere — the failure has no natural symptom.
func TestCategoryCreateSendsTheChosenTargetType(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{}
	spy.install(m)

	f := m.newCategoryForm()
	if f == nil {
		t.Fatal("the pane must offer a create form")
	}
	// Drive the REAL form: set the picker to the WORKFLOW option and type a name, then submit — the
	// same path the operator's keys take.
	f.Set("target", categoryTargetValue(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW))
	f.Set("name", "Ingestion")
	f.Set("description", "runs that pull data in")
	cmd, err := f.Submit()
	if err != nil {
		t.Fatalf("a valid create must submit, got %v", err)
	}
	if cmd == nil {
		t.Fatal("a valid create must produce the write command")
	}
	cmd()

	if len(spy.created) != 1 {
		t.Fatalf("want exactly one CreateCategory, got %d", len(spy.created))
	}
	got := spy.created[0]
	if got.GetTargetType() != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW {
		t.Fatalf("created with target %v, want WORKFLOW (the chosen one)", got.GetTargetType())
	}
	if got.GetName() != "Ingestion" {
		t.Fatalf("created name %q, want %q", got.GetName(), "Ingestion")
	}
	if got.GetDescription() != "runs that pull data in" {
		t.Fatalf("created description %q, want the typed one", got.GetDescription())
	}
}

// TestCategoryCreateWithoutANameIsRefusedWithTheFormOpen: a create that closes and writes nothing is
// the silent-rejection class — the operator sees the form go away and no grouping appears.
func TestCategoryCreateWithoutANameIsRefusedWithTheFormOpen(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{}
	spy.install(m)

	f := m.newCategoryForm()
	f.Set("target", categoryTargetValue(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER))
	f.Set("name", "   ") // whitespace only — must not be accepted as a name
	cmd, err := f.Submit()

	if err == nil {
		t.Fatal("a blank name must be refused, not written")
	}
	if cmd != nil {
		t.Fatal("nothing may be written when the name is blank")
	}
	if len(spy.created) != 0 {
		t.Fatalf("CreateCategory ran for a blank name: %+v", spy.created)
	}
	// THE FORM STAYS OPEN. `Submitted` is what the host reads to close it, so a rejected submit that
	// set it would make the form vanish with nothing created and nothing said — the exact
	// silent-rejection failure this asserts against. The blank name is caught by the field's Required
	// rule before OnSubmit (so the message is the form's own "form has errors"); either way the
	// operator must still be looking at the box they need to fill in.
	if f.Submitted {
		t.Fatal("a refused create must leave the form open (Submitted must stay false)")
	}
	if strings.TrimSpace(f.SubmitErr) == "" && err == nil {
		t.Fatal("a refused create must say why")
	}
}

// TestCategoryRenameIsPrefilledAndUpdatesTheSameGrouping: an edit box that opens EMPTY makes the
// operator retype a value they cannot see (the same rule the conversation rename follows), and the
// update must carry the id of the row that was opened.
func TestCategoryRenameIsPrefilledAndUpdatesTheSameGrouping(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{byTarget: map[apiv1.CategoryTargetType][]*apiv1.Category{
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER: {
			{Id: "cat_w", Name: "Backend", Description: "server work",
				TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER},
		},
	}}
	spy.install(m)
	// Load first: the edit form is built FROM the loaded row, which is what makes it prefilled.
	m.Base.LoadItems("categories", loadCategories(t, m), "")

	// The pane must be FOCUSED for the generic edit path to resolve this source.
	if !m.Base.SelectSource("categories") {
		t.Skip("categories source is not reachable in this fixture")
	}
	m.Base.SelectItem("categories", "cat_w")
	form := m.editFormForSource()
	if form == nil {
		t.Fatal("`e` on a grouping must open the rename form")
	}
	if got := form.Values["name"]; got != "Backend" {
		t.Fatalf("the rename form opened with name %q, want the current name %q — it must be prefilled", got, "Backend")
	}
	if got := form.Values["description"]; got != "server work" {
		t.Fatalf("the rename form opened with description %q, want the current one", got)
	}

	form.Set("name", "Platform")
	cmd, err := form.Submit()
	if err != nil {
		t.Fatalf("a valid rename must submit, got %v", err)
	}
	cmd()

	if len(spy.updated) != 1 {
		t.Fatalf("want exactly one UpdateCategory, got %d", len(spy.updated))
	}
	up := spy.updated[0]
	if up.GetId() != "cat_w" {
		t.Fatalf("rename targeted %q, want the opened row cat_w", up.GetId())
	}
	if up.GetName() != "Platform" {
		t.Fatalf("rename sent name %q, want %q", up.GetName(), "Platform")
	}
}

// TestCategoryDeleteIsGuardedByAConfirm: deleting a grouping is destructive and moves its items to
// Uncategorized, so it must ASK first — and the confirm must say what happens to the items, because
// "delete the grouping" does not tell the operator whether their workers are destroyed with it.
func TestCategoryDeleteIsGuardedByAConfirm(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{byTarget: map[apiv1.CategoryTargetType][]*apiv1.Category{
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER: {
			{Id: "cat_w", Name: "Backend", TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER},
		},
	}}
	spy.install(m)
	m.Base.LoadItems("categories", loadCategories(t, m), "")
	if !m.Base.SelectSource("categories") {
		t.Skip("categories source is not reachable in this fixture")
	}
	m.Base.SelectItem("categories", "cat_w")

	item, ok := m.ActiveItem()
	if !ok {
		t.Fatal("fixture: the grouping row must be selectable")
	}
	actions := m.categoryActions(item)
	var del *kit2.Action
	for i := range actions {
		if actions[i].Key == "x" {
			del = &actions[i]
		}
	}
	if del == nil {
		t.Fatal("a grouping row must offer a delete action on `x`")
	}
	if !del.NeedsConfirm() {
		t.Fatal("delete must require confirmation")
	}
	if !strings.Contains(del.Confirm, "Uncategorized") {
		t.Fatalf("the confirm %q must say where the items go — that is the operator's real question", del.Confirm)
	}

	// Cancelling writes nothing. The dialog's dismissal is the EMPTY choice (screen.go: `choice == ""`
	// is the dismissed path), not a word like "no" — asserting with "no" would have confirmed the
	// delete and then reported that cancelling deletes, which is a test lying about the code.
	if cmd := m.confirmAndRun(*del, ""); cmd != nil {
		t.Fatalf("a dismissed confirm must dispatch nothing, got a cmd")
	}
	if len(spy.deleted) != 0 {
		t.Fatalf("dismissing must not delete, got %v", spy.deleted)
	}
	// ...and confirming deletes exactly that grouping. The confirm runs through the mutation executor,
	// so the returned cmd must actually be RUN — merely receiving it would let a delete that never
	// fires pass this test.
	if res := runCmd(t, m.confirmAndRun(*del, del.Label)); res.Err != nil {
		t.Fatalf("confirmed delete failed: %v", res.Err)
	}
	if len(spy.deleted) != 1 || spy.deleted[0] != "cat_w" {
		t.Fatalf("confirming must delete cat_w, got %v", spy.deleted)
	}
}

// TestCategoryFetchFailureSurfaces: a pane that silently renders empty when the list RPC fails tells
// the operator they have no groupings, which is a lie — the error must reach the source's error slot.
func TestCategoryFetchFailureSurfaces(t *testing.T) {
	m, _ := newWriteModel(t)
	spy := &categorySpy{listErr: errors.New("connection refused")}
	spy.install(m)

	if _, _, err := m.fetchCategories(context.Background(), ""); err == nil {
		t.Fatal("a failed list must return an error rather than an empty pane")
	}
}

// TestCategoryThunksWorkWithNoClient: every other write in this package reports "no X client" instead
// of panicking when the plane is absent. The categories code called m.cl.Categories directly, which
// would have been a nil dereference — a crash on a screen the operator reached by pressing `n`.
func TestCategoryThunksWorkWithNoClient(t *testing.T) {
	// A FRESH construction over a nil client set — New(nil, nil) — so this asserts the constructor's
	// own guards rather than a hand-installed stub.
	m := New(nil, nil)
	if m.rpcCreateCategory == nil || m.rpcUpdateCategory == nil || m.rpcDeleteCategory == nil || m.rpcListCategories == nil {
		t.Fatal("the constructor must install the category thunks")
	}
	if err := m.rpcCreateCategory(context.Background(), &apiv1.CreateCategoryRequest{Name: "x"}); err == nil {
		t.Fatal("a create with no client must return an error, not panic")
	}
	if err := m.rpcUpdateCategory(context.Background(), &apiv1.UpdateCategoryRequest{Id: "c"}); err == nil {
		t.Fatal("an update with no client must return an error, not panic")
	}
	if err := m.rpcDeleteCategory(context.Background(), "c"); err == nil {
		t.Fatal("a delete with no client must return an error, not panic")
	}
	if _, _, err := m.rpcListCategories(context.Background(), apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER); err == nil {
		t.Fatal("a list with no client must return an error, not panic")
	}
}
