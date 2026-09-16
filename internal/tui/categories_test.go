package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// stubCategories is a CategoryService that records what it was asked to do.
//
// The tests assert on the RPC PAYLOAD rather than on the message that came back, because the payload
// is the thing the server acts on: a test that only checks "a mutation happened" passes with the
// wrong target, the wrong type, or the wrong id.
type stubCategories struct {
	apiv1connect.UnimplementedCategoryServiceHandler

	mine []*apiv1.Category
	// assignments records AssignToCategory calls.
	assignedCategory, assignedEntity string
	assignedTarget                   apiv1.CategoryTargetType
	// unassigned records UnassignFromCategory calls.
	unassignedEntity string
	unassignedTarget apiv1.CategoryTargetType
	// created records CreateCategory calls and returns a new id.
	createdName   string
	createdTarget apiv1.CategoryTargetType
	createdID     string
	deletedID     string
	updatedID     string
	updatedName   string
	// listErr makes ListCategories fail (the degraded path).
	listErr error
}

func (s *stubCategories) ListCategories(_ context.Context, req *connect.Request[apiv1.ListCategoriesRequest]) (*connect.Response[apiv1.ListCategoriesResponse], error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*apiv1.Category
	for _, c := range s.mine {
		if c.GetTargetType() == req.Msg.GetTargetType() {
			out = append(out, c)
		}
	}
	return connect.NewResponse(&apiv1.ListCategoriesResponse{Categories: out}), nil
}

func (s *stubCategories) CreateCategory(_ context.Context, req *connect.Request[apiv1.CreateCategoryRequest]) (*connect.Response[apiv1.CreateCategoryResponse], error) {
	s.createdName = req.Msg.GetName()
	s.createdTarget = req.Msg.GetTargetType()
	if s.createdID == "" {
		s.createdID = "cat-new"
	}
	return connect.NewResponse(&apiv1.CreateCategoryResponse{
		Category: &apiv1.Category{Id: s.createdID, Name: req.Msg.GetName(), TargetType: req.Msg.GetTargetType()},
	}), nil
}

func (s *stubCategories) UpdateCategory(_ context.Context, req *connect.Request[apiv1.UpdateCategoryRequest]) (*connect.Response[apiv1.UpdateCategoryResponse], error) {
	s.updatedID = req.Msg.GetId()
	if req.Msg.Name != nil {
		s.updatedName = req.Msg.GetName()
	}
	return connect.NewResponse(&apiv1.UpdateCategoryResponse{
		Category: &apiv1.Category{Id: req.Msg.GetId(), Name: s.updatedName},
	}), nil
}

func (s *stubCategories) DeleteCategory(_ context.Context, req *connect.Request[apiv1.DeleteCategoryRequest]) (*connect.Response[apiv1.DeleteCategoryResponse], error) {
	s.deletedID = req.Msg.GetId()
	return connect.NewResponse(&apiv1.DeleteCategoryResponse{}), nil
}

func (s *stubCategories) AssignToCategory(_ context.Context, req *connect.Request[apiv1.AssignToCategoryRequest]) (*connect.Response[apiv1.AssignToCategoryResponse], error) {
	s.assignedCategory = req.Msg.GetCategoryId()
	s.assignedEntity = req.Msg.GetEntityId()
	s.assignedTarget = req.Msg.GetTargetType()
	return connect.NewResponse(&apiv1.AssignToCategoryResponse{}), nil
}

func (s *stubCategories) UnassignFromCategory(_ context.Context, req *connect.Request[apiv1.UnassignFromCategoryRequest]) (*connect.Response[apiv1.UnassignFromCategoryResponse], error) {
	s.unassignedEntity = req.Msg.GetEntityId()
	s.unassignedTarget = req.Msg.GetTargetType()
	return connect.NewResponse(&apiv1.UnassignFromCategoryResponse{}), nil
}

func (s *stubCategories) ReorderCategories(_ context.Context, req *connect.Request[apiv1.ReorderCategoriesRequest]) (*connect.Response[apiv1.ReorderCategoriesResponse], error) {
	return connect.NewResponse(&apiv1.ReorderCategoriesResponse{}), nil
}

// categoryApp builds a shell whose CategoryService is the stub, with the Ask rail loaded.
func categoryApp(t *testing.T, cats ...*apiv1.Category) (*App, *stubCategories) {
	t.Helper()
	stub := &stubCategories{mine: cats}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewCategoryServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.SwitchTo(TabAsk)
	return m, stub
}

func workerCat(id, name string) *apiv1.Category {
	return &apiv1.Category{Id: id, Name: name, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER}
}

func convCat(id, name string) *apiv1.Category {
	return &apiv1.Category{Id: id, Name: name, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION}
}

// loadCats runs the loader synchronously so the cache is populated the way a real session's startup
// load would leave it.
func loadCats(t *testing.T, m *App) *App {
	t.Helper()
	cmd := m.loadCategories()
	if cmd == nil {
		t.Fatal("loadCategories returned no command with a category client wired")
	}
	next, _ := m.Update(cmd())
	return next.(*App)
}

// TestCategoriesLoadOnceForEveryTargetType: one round trip per target type at startup, so the first
// assignment already has its picker populated rather than opening an empty one.
func TestCategoriesLoadOnceForEveryTargetType(t *testing.T) {
	m, _ := categoryApp(t, workerCat("w1", "Frontend"), convCat("c1", "Research"))
	m = loadCats(t, m)

	if got := len(m.categoriesFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)); got != 1 {
		t.Fatalf("worker categories = %d, want 1", got)
	}
	if got := len(m.categoriesFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)); got != 1 {
		t.Fatalf("conversation categories = %d, want 1", got)
	}
	// The types are kept apart: a worker grouping must never appear in a conversation's picker, which
	// is the whole reason the API keys them.
	if got := len(m.categoriesFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW)); got != 0 {
		t.Fatalf("workflow categories = %d, want 0 (none were loaded)", got)
	}
}

// TestCategorizeChordOnTheWorkersPaneOpensTheModal is the operator's ask — "a key to assign an item to
// a specific category" — through the shell hook the pane calls.
func TestCategorizeChordOnTheWorkersPaneOpensTheModal(t *testing.T) {
	m, _ := categoryApp(t, workerCat("w1", "Frontend"))
	m = loadCats(t, m)

	m.OpenAssignCategory("worker-42", "Quick Software Engineer",
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)

	if m.assignForm == nil {
		t.Fatal("the hook must open the assign modal")
	}
	if m.assignEntity != "worker-42" {
		t.Fatalf("the modal must target the selected entity, got %q", m.assignEntity)
	}
	// The picker offers the existing groupings, an explicit uncategorized, and a create-in-place.
	opts := m.assignForm.Specs[0].Options
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"Frontend", "uncategorized", "new category"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the picker is missing %q: %v", want, labels)
		}
	}
	// And the composer advertises the FORM's keys, since the form now owns the keyboard.
	if !strings.Contains(m.dock.View(), "ctrl+s") {
		t.Fatalf("the composer must advertise the form's save chord: %q", m.dock.View())
	}
}

// TestAssignWritesTheChosenCategory runs the real keys and then RUNS the command, asserting the RPC
// payload — the id, the entity and the target type the server will act on.
func TestAssignWritesTheChosenCategory(t *testing.T) {
	m, stub := categoryApp(t, workerCat("w1", "Frontend"), workerCat("w2", "Backend"))
	m = loadCats(t, m)
	m.OpenAssignCategory("worker-42", "Quick Software Engineer",
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)

	// The picker's own cursor navigation is already covered by the form's tests (form_picker_test.go);
	// WHAT THIS TEST IS ABOUT is that the chosen grouping reaches the RPC intact. So the value is set
	// through the form's own Setter and the SAVE is pressed for real — the key path that this layer
	// owns, and the one a test that called Submit directly would skip.
	m.assignForm.Set(assignPickField, "w2")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)
	if m.assignForm != nil {
		t.Fatal("a successful save must close the modal")
	}
	if cmd == nil {
		t.Fatal("ctrl+s must produce the write")
	}
	if nm, _ := m.Update(cmd()); nm != nil {
		m = nm.(*App)
	}
	m = nm.(*App)
	if stub.assignedCategory != "w2" || stub.assignedEntity != "worker-42" {
		t.Fatalf("AssignToCategory got (%q, %q), want (w2, worker-42)", stub.assignedCategory, stub.assignedEntity)
	}
	if stub.assignedTarget != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER {
		t.Fatalf("AssignToCategory got target %v, want WORKER", stub.assignedTarget)
	}
}

// TestUncategorizedUnassigns: "— uncategorized —" is a REAL state with its own RPC, not "assign to
// nothing" — so it must call UnassignFromCategory rather than being a no-op.
func TestUncategorizedUnassigns(t *testing.T) {
	m, stub := categoryApp(t, convCat("c9", "Research"))
	m = loadCats(t, m)
	m.OpenAssignCategory("conv-7", "a conversation",
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)

	// The default selection IS "uncategorized" (the first option), so ctrl+s straight away unassigns.
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)
	if cmd == nil {
		t.Fatal("saving the default choice must issue the unassign")
	}
	if nm, _ := m.Update(cmd()); nm != nil {
		m = nm.(*App)
	}
	m = nm.(*App)
	if stub.unassignedEntity != "conv-7" {
		t.Fatalf("UnassignFromCategory got entity %q, want conv-7", stub.unassignedEntity)
	}
	if stub.unassignedTarget != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION {
		t.Fatalf("UnassignFromCategory got target %v, want CONVERSATION", stub.unassignedTarget)
	}
}

// TestCreateAndAssignInOneGesture is the other half of the operator's ask — "a key to create new
// category AND a key to assign an item" — in ONE save: the grouping is created, then the item is put
// in it, and both halves are reported as one result.
func TestCreateAndAssignInOneGesture(t *testing.T) {
	m, stub := categoryApp(t)
	m = loadCats(t, m)
	stub.createdID = "cat-created"
	m.OpenAssignCategory("worker-99", "A worker",
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)

	// Choose "＋ new category…", then move to the NAME field and type it.
	//
	// The move is not incidental: choosing the option reveals the field but leaves the cursor on the
	// picker (the form's conditional-field rule), so typing without moving would be a picker QUERY — the
	// first version of this test did exactly that and the name arrived empty.
	m.assignForm.Set(assignPickField, assignNewValue)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(*App)
	nm, _ = m.Update(keyRunes("Platform"))
	m = nm.(*App)
	if got := m.assignForm.Values[assignNewField]; got != "Platform" {
		t.Fatalf("the typed name did not reach the field: %q", got)
	}

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)
	if cmd == nil {
		t.Fatalf("a new-grouping save must issue the writes (errors=%v)", m.assignForm)
	}
	if nm, _ := m.Update(cmd()); nm != nil {
		m = nm.(*App)
	}
	m = nm.(*App)
	if stub.createdName != "Platform" {
		t.Fatalf("CreateCategory got name %q, want Platform", stub.createdName)
	}
	if stub.createdTarget != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER {
		t.Fatalf("CreateCategory got target %v, want WORKER", stub.createdTarget)
	}
	if stub.assignedCategory != "cat-created" || stub.assignedEntity != "worker-99" {
		t.Fatalf("the new grouping was not assigned: (%q, %q)", stub.assignedCategory, stub.assignedEntity)
	}
}

// TestNewCategoryWithoutANameIsRefusedWithTheFormOpen: a save that closes and writes nothing is the
// silent-rejection class, and here it would also throw away the name they were about to give.
func TestNewCategoryWithoutANameIsRefusedWithTheFormOpen(t *testing.T) {
	m, stub := categoryApp(t)
	m = loadCats(t, m)
	m.OpenAssignCategory("worker-1", "W", apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)
	// "new category…" with the name field left EMPTY: choosing the option is enough to arm the refusal
	// (the field is revealed but not filled).
	m.assignForm.Set(assignPickField, assignNewValue)

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)
	if m.assignForm == nil {
		t.Fatal("the modal must stay open when the new name is missing")
	}
	if m.assignForm.SubmitErr == "" {
		t.Fatalf("the refusal must state a reason, got pick=%q name=%q errors=%v",
			m.assignForm.Values[assignPickField], m.assignForm.Values[assignNewField], m.assignForm.Errors)
	}
	// The invariant is that NOTHING WAS WRITTEN, asked of the SERVER rather than of the returned
	// command: App.Update also drains the composer's caret-blink starter, so a non-nil cmd can be a
	// blink timer with no write behind it — an earlier version of this test asserted `cmd != nil` and
	// failed on exactly that. Running whatever came back and checking the stub is the honest check.
	if cmd != nil {
		if nm2, _ := m.Update(cmd()); nm2 != nil {
			m = nm2.(*App)
		}
	}
	if stub.createdName != "" {
		t.Fatalf("CreateCategory was called anyway: %q", stub.createdName)
	}
	if stub.assignedEntity != "" {
		t.Fatalf("AssignToCategory was called anyway: %q", stub.assignedEntity)
	}
}

// TestAssignModalOwnsItsKeys: while it is up, typing must not leak into the composer behind it.
func TestAssignModalOwnsItsKeys(t *testing.T) {
	m, _ := categoryApp(t)
	m = loadCats(t, m)
	m.OpenAssignCategory("worker-1", "W", apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)
	nm, _ := m.Update(keyRunes("zzz"))
	m = nm.(*App)
	if got := m.dock.Value(); got != "" {
		t.Fatalf("typing in the assign modal leaked into the composer: %q", got)
	}
}

// TestAssignEscClosesWithoutWriting: esc is a cancel, and it must forget the target so a later save
// cannot write to an item the operator has stopped looking at.
func TestAssignEscClosesWithoutWriting(t *testing.T) {
	m, stub := categoryApp(t, workerCat("w1", "Frontend"))
	m = loadCats(t, m)
	m.OpenAssignCategory("worker-1", "W", apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(*App)
	if m.assignForm != nil || m.assignEntity != "" {
		t.Fatal("esc must close the modal and forget its target")
	}
	// Same reasoning as the refusal test: the returned command is not evidence of a write (App.Update
	// drains the caret-blink starter too), so run it and ask the SERVER.
	if cmd != nil {
		if nm2, _ := m.Update(cmd()); nm2 != nil {
			m = nm2.(*App)
		}
	}
	if stub.assignedEntity != "" {
		t.Fatalf("esc wrote an assignment to %q", stub.assignedEntity)
	}
	if stub.createdName != "" {
		t.Fatalf("esc created a grouping: %q", stub.createdName)
	}
}

// TestCategorizeChordOnTheRailUsesTheSelectedConversation is the conversation half: the rail's chord
// targets whatever the cursor is on, with the CONVERSATION target type.
func TestCategorizeChordOnTheRailUsesTheSelectedConversation(t *testing.T) {
	m, _ := categoryApp(t, convCat("c1", "Research"))
	m = loadCats(t, m)
	m.askMode = askConversations
	m.convRailOpen = true
	m.conversations = []chat.Conversation{{ID: "conv-a", Title: "first"}, {ID: "conv-b", Title: "second"}}
	m.convSel = 1

	// The REAL chord, through Update — not a test-only shortcut. The rail's keys are driven from the
	// composer, so this also proves the chord wins over typing in that state.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = nm.(*App)
	if m.assignForm == nil {
		t.Fatal("ctrl+t must open the assign modal on the rail")
	}
	if m.assignEntity != "conv-b" {
		t.Fatalf("the modal must target the SELECTED conversation, got %q", m.assignEntity)
	}
	if m.assignTarget != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION {
		t.Fatalf("wrong target type: %v", m.assignTarget)
	}
}

// TestCategoriesLoadFailureIsReportedNotSwallowed: one type failing must SAY so — a picker that is
// silently empty reads as "you have no groupings", which is a different and misleading claim.
func TestCategoriesLoadFailureIsReportedNotSwallowed(t *testing.T) {
	m, stub := categoryApp(t, workerCat("w1", "Frontend"))
	stub.listErr = connect.NewError(connect.CodeUnavailable, errors.New("category service down"))
	cmd := m.loadCategories()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	// App is a VALUE model: Update returns the next App, and asserting on the input would read a stale
	// one. (This test caught that in itself — the failure was an empty dock error, not a missing one.)
	nm2, _ := m.Update(msg)
	m = nm2.(*App)
	if got := m.dock.Err; !strings.Contains(got, "categories") {
		t.Fatalf("a category load failure must be surfaced in the dock, got %q", got)
	}
}
