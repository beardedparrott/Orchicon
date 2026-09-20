package tui

// categories.go — the TUI's category surfaces (feature: worker / workflow / conversation groupings).
//
// The operator: "conversation/workflows/worker category groupings don't exist (create, rename, and
// delete groupings)" … "For categories, we could assign a key to create new category and a key to
// assign an item to a specific category."
//
// THE SERVER HAD ALL OF IT. `category_service.proto` exposes the full set — List, Create, Update,
// Delete, Assign, Unassign, Reorder — keyed by a `target_type` of worker / workflow / conversation,
// and the GUI consumes them (CategoryFolder, CreateCategoryDialog, CategoryDndContext, wired into
// workers.tsx, workflows.tsx and ask-orchicon.tsx). The TUI had ZERO surface and had not even wired
// the client, so this is a parity gap of the same kind as the rename one: nothing was missing on the
// platform, an entire surface was missing from one client.
//
// WHERE THINGS LIVE, AND WHY THEY ARE SPLIT. There are two acts, and they belong in different places:
//
//  1. MANAGING the groupings (create / rename / delete) is a SETTINGS act — it applies to the target
//     type as a whole, not to any one item — so it lives on the Control tab, which is where this
//     client files its settings surfaces (providers, secrets, webhooks, adapters, themes). One pane
//     lists all three target types together, because "the groupings" is one idea to the operator and
//     three kinds to the API.
//
//  2. ASSIGNING one item is an act ON THAT ITEM, so it is a key on the pane the item lives in — `C`
//     on the Workers and Workflows panes, and a chord on the Ask conversations rail, which is the one
//     list in this app whose keys are driven from the composer and therefore cannot use a bare letter.
//
// ONE MODAL DOES BOTH KINDS OF ASSIGNMENT, and it folds "make a new one" into the same gesture: the
// picker offers the existing categories, "— uncategorized —", and "＋ new category…", and choosing the
// last one reveals a name field. That is what makes "a key to create new category and a key to assign
// an item" a single keystroke rather than a trip to another tab and back.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// Sentinel picker values. They cannot collide with a category id (a ULID), so they are safe as
// options in a picker whose other entries ARE ids.
const (
	assignNoneValue = "\x00none"  // "— uncategorized —"
	assignNewValue  = "\x00new"   // "＋ new category…"
	assignNewField  = "new_name"  // the conditional name field
	assignPickField = "category"  // the picker itself
	assignEntityKey = "entity_id" // carried through OnSubmit, so the target cannot be swapped
	assignTargetKey = "target_type"
)

// targetLabel names a target type the way the operator says it.
func targetLabel(t apiv1.CategoryTargetType) string {
	switch t {
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER:
		return "worker"
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW:
		return "workflow"
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION:
		return "conversation"
	}
	return "other"
}

// allTargetTypes is the fixed set the pickers offer, in the order the GUI's tabs read.
func allTargetTypes() []apiv1.CategoryTargetType {
	return []apiv1.CategoryTargetType{
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER,
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW,
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
}

// --- loading ------------------------------------------------------------------------------------

// categoriesLoadedMsg carries every target type's categories in one round trip.
//
// All three are loaded together because the Control pane lists them together and the assign modal
// needs whichever type its item belongs to — a per-type lazy load would mean the first assign after a
// tab switch opens a picker with nothing in it.
//
// THE ASSIGNMENTS ARE THE POINT. The first version of this fetched only the categories and DISCARDED
// the assignments the same response carries — which is why the operator's "I created a conversation
// category and assigned a conversation to it, but it is not showing up in the UI" was literally true:
// the assignment existed on the server, arrived in this response, and was thrown away before anything
// could render it. A category list is useless to a LIST OF ITEMS without knowing which item is in
// which group.
type categoriesLoadedMsg struct {
	Categories  []*apiv1.Category
	Assignments []*apiv1.CategoryAssignment
	Err         error
}

// loadCategories fetches all three target types.
func (m *App) loadCategories() tea.Cmd {
	cl := m.clients
	if cl == nil || cl.Categories == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out []*apiv1.Category
		var assigns []*apiv1.CategoryAssignment
		for _, t := range allTargetTypes() {
			resp, err := cl.Categories.ListCategories(ctx, connect.NewRequest(&apiv1.ListCategoriesRequest{
				TargetType: t,
			}))
			if err != nil {
				// One type failing must not blank the other two: the picker for the other types is
				// still perfectly usable, and a partial list with a stated error beats an empty pane.
				return categoriesLoadedMsg{Err: fmt.Errorf("%s categories: %w", targetLabel(t), err)}
			}
			out = append(out, resp.Msg.GetCategories()...)
			// The assignments arrive in the SAME response and are what makes a grouping visible on the
			// item it belongs to.
			assigns = append(assigns, resp.Msg.GetAssignments()...)
		}
		return categoriesLoadedMsg{Categories: out, Assignments: assigns}
	}
}

// onCategoriesLoaded stores the cache and reconciles the open modals.
func (m *App) onCategoriesLoaded(msg categoriesLoadedMsg) tea.Cmd {
	if msg.Err != nil {
		m.dock.SetError(msg.Err.Error())
	}
	m.categories = msg.Categories
	// The assignment map is rebuilt from scratch, not merged: an unassign must CLEAR the entry, and a
	// merge would leave the item showing a grouping it is no longer in.
	m.catAssignedBy = make(map[string]string, len(msg.Assignments))
	for _, a := range msg.Assignments {
		m.catAssignedBy[catEntityKey(a.GetTargetType(), a.GetEntityId())] = a.GetCategoryId()
	}
	// An open assign modal was built before the list arrived; re-point its options at the real list so
	// the operator does not have to close and reopen it.
	m.refreshAssignOptions()
	// A SCREEN THAT GROUPS ITS ROWS needs the new assignments to place them: without this the grouping
	// would stay as it was at the first load until something else happened to refresh the pane.
	if s := m.screens[m.active]; s != nil {
		if r, ok := s.(interface{ RefreshView() tea.Cmd }); ok {
			return r.RefreshView()
		}
	}
	return nil
}

// catEntityKey keys the assignment map. The TARGET TYPE is part of the key because an entity id is only
// unique within its own kind — a bare id would let a worker's grouping leak onto a conversation that
// happened to share it.
func catEntityKey(t apiv1.CategoryTargetType, entityID string) string {
	return fmt.Sprintf("%d\x00%s", int32(t), entityID)
}

// CategoryOf is the shell's read-side hook for a screen that GROUPS its rows by category.
//
// One method, returning the id AND the name, because a caller needs both and two lookups could report a
// different group between them. Unexported work stays in categoryOf; this is the contract with the
// screens, alongside OpenAssignCategory.
func (m *App) CategoryOf(target apiv1.CategoryTargetType, entityID string) (string, string) {
	c := m.categoryOf(target, entityID)
	if c == nil {
		return "", ""
	}
	return c.GetId(), c.GetName()
}

// CategoryGroupsFor is the shell's ordered folder list for a grouped pane.
func (m *App) CategoryGroupsFor(target apiv1.CategoryTargetType) []screenkit.GroupSpec {
	return m.categoryGroupsFor(target)
}

// catByID finds a cached category (nil when the id is unknown — a stale row, or a grouping deleted
// between a fetch and the operator acting on its row).
func (m *App) catByID(id string) *apiv1.Category {
	for _, c := range m.categories {
		if c.GetId() == id {
			return c
		}
	}
	return nil
}

// categoryGroupsFor returns the categories to render as folders for one target type, IN THE SERVER'S
// ORDER (sort_order, then name — the same rule categoriesFor applies to the assign picker).
//
// EMPTY CATEGORIES ARE INCLUDED, which is the GUI's behaviour: it builds a group for every category it
// knows about and only then appends the Uncategorized folder, so an operator who created "Frontend"
// sees the folder even before anything is in it. Passing the ORDERED list in (rather than letting the
// list helper invent an order) is also what keeps the two clients' folder order identical.
func (m *App) categoryGroupsFor(target apiv1.CategoryTargetType) []screenkit.GroupSpec {
	cats := m.categoriesFor(target)
	out := make([]screenkit.GroupSpec, 0, len(cats))
	for _, c := range cats {
		out = append(out, screenkit.GroupSpec{ID: c.GetId(), Name: c.GetName()})
	}
	return out
}

// categoryOf returns the category an entity is assigned to, or nil.
func (m *App) categoryOf(t apiv1.CategoryTargetType, entityID string) *apiv1.Category {
	id := m.catAssignedBy[catEntityKey(t, entityID)]
	if id == "" {
		return nil
	}
	for _, c := range m.categories {
		if c.GetId() == id {
			return c
		}
	}
	return nil
}

// categoriesFor returns one target type's categories, ordered by the server's sort_order then name.
func (m *App) categoriesFor(t apiv1.CategoryTargetType) []*apiv1.Category {
	var out []*apiv1.Category
	for _, c := range m.categories {
		if c.GetTargetType() == t {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GetSortOrder() != out[j].GetSortOrder() {
			return out[i].GetSortOrder() < out[j].GetSortOrder()
		}
		return strings.ToLower(out[i].GetName()) < strings.ToLower(out[j].GetName())
	})
	return out
}

// categoryName resolves an id to its name ("" when unknown, so a stale assignment reads as blank
// rather than as a raw id).
func (m *App) categoryName(id string) string {
	for _, c := range m.categories {
		if c.GetId() == id {
			return c.GetName()
		}
	}
	return ""
}

// --- the assign modal ---------------------------------------------------------------------------

// openAssignCategory opens the assign-or-create modal for one entity.
//
// entityID is carried in the form's Values rather than read from the shell at submit time, so a
// selection change behind the modal (the rolling refresh re-seats a cursor, a stream pokes a list)
// cannot retarget the write to a different item.
func (m *App) openAssignCategory(entityID, entityLabel string, target apiv1.CategoryTargetType) {
	m.openAssignCategories([]string{entityID}, entityLabel, target)
}

// openAssignCategories is the general form: ONE OR MANY entities.
//
// It is the same modal either way — the picker, the "— uncategorized —" row and the "+ new category…"
// row all mean the same thing for a list as for a row, and the write loops the ids. Splitting it into
// two modals would mean two implementations of one write, which is how the TUI's tab-selection gesture
// ended up written three times and drifted.
func (m *App) openAssignCategories(entityIDs []string, entityLabel string, target apiv1.CategoryTargetType) {
	// Drop empty ids: a caller passing [] is a bug, and a modal that writes to "" is worse than a
	// refusal.
	ids := make([]string, 0, len(entityIDs))
	for _, id := range entityIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		m.dock.SetError("nothing selected to categorize")
		return
	}
	title := "Categorize " + targetLabel(target)
	if len(ids) > 1 {
		title = fmt.Sprintf("Categorize %d %ss", len(ids), targetLabel(target))
	}
	f := kit2.NewForm(title,
		kit2.FieldSpec{
			Name:  assignPickField,
			Label: "Category",
			Kind:  kit2.KPicker,
		},
		// The new-name field is present from the start and HIDDEN until "new category…" is chosen: the
		// form's Visible predicate reads the picker's live value, so the modal grows a row exactly when
		// the operator asks for one.
		assignNewSpec(),
	)
	f.Focused = true
	f.OnSubmit = func(vals map[string]string, _ map[string][]string) (tea.Cmd, error) {
		if vals[assignPickField] == assignNewValue && strings.TrimSpace(vals[assignNewField]) == "" {
			// Refuse with a reason and KEEP the form open: a save that closes and writes nothing is the
			// silent-rejection class, and here it would also lose the name they were about to give.
			return nil, fmt.Errorf("name the new grouping")
		}
		return m.assignSubmit(vals), nil
	}
	f.Values[assignEntityKey] = strings.Join(ids, "\x00")
	f.Values[assignNewField] = ""
	f.Values[assignPickField] = assignNoneValue
	f.Width = m.modalWidth()
	m.assignForm = f
	m.assignTarget = target
	m.assignEntities = ids
	m.refreshAssignOptions()
	// ALWAYS RELOAD WHEN THE PICKER OPENS, so the options are as fresh as the pane's own list is. The
	// reply fills the picker IN PLACE (onCategoriesLoaded → refreshAssignOptions), so the modal appears
	// immediately and its options land a moment later — better than offering a grouping that was deleted
	// in the other client, which is the one failure a picker cannot recover from.
	if m.clients != nil && m.clients.Categories != nil {
		m.pendingCatCmd = m.loadCategories()
	}
	m.refreshComposerHint()
}

// refreshAssignOptions rebuilds the picker's option list from the cache.
//
// It is called both when the modal opens and when a load lands, which is why the options are a
// function of state rather than a snapshot taken at open time.
func (m *App) refreshAssignOptions() {
	if m.assignForm == nil {
		return
	}
	opts := []kit2.Option{{Value: assignNoneValue, Label: "— uncategorized —"}}
	for _, c := range m.categoriesFor(m.assignTarget) {
		opts = append(opts, kit2.Option{Value: c.GetId(), Label: c.GetName()})
	}
	opts = append(opts, kit2.Option{Value: assignNewValue, Label: "＋ new category…"})
	for i := range m.assignForm.Specs {
		s := &m.assignForm.Specs[i]
		switch s.Name {
		case assignPickField:
			s.Options = opts
			// Default to the current value only if it is still a valid option; otherwise fall back to
			// "uncategorized" so the picker never shows a value it cannot offer.
			if !hasOption(opts, m.assignForm.Values[assignPickField]) {
				m.assignForm.Values[assignPickField] = assignNoneValue
			}
		case assignNewField:
			// The name field appears ONLY when "new category…" is chosen, so the common case (assign to
			// something that exists) stays a one-line modal.
			//
			// VISIBLE READS THE LIVE VALUES MAP — that is why the field's signature takes one. An
			// earlier version captured the picker's value in a local at build time, so the field could
			// never become visible: the form asks Visible AFTER a value changes, and a captured
			// snapshot always answered with the value from before the operator chose anything. A test
			// caught it (the typed name arrived empty, because the field was still hidden), and the
			// same bug would have made "create a grouping while assigning" impossible in the real UI.
			s.Visible = func(values map[string]string) bool {
				return values[assignPickField] == assignNewValue
			}
		}
	}
}

func hasOption(opts []kit2.Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

// assignPlaceholder is the conditional field's own spec, added on demand so a form with no "new"
// choice does not carry an invisible required field (a hidden Required field cannot block a save —
// that is the form's rule — but keeping it out entirely is simpler to reason about).
func assignNewSpec() kit2.FieldSpec {
	return kit2.FieldSpec{
		Name:        assignNewField,
		Label:       "New category name",
		Kind:        kit2.KText,
		Placeholder: "a name for the new grouping",
	}
}

// assignCategoryKey drives the assign modal; it owns every key while open.
func (m *App) assignCategoryKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.assignForm == nil {
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.closeAssign()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}
	cmd, _ := m.assignForm.HandleKey(k)
	if m.assignForm != nil && m.assignForm.Submitted {
		m.closeAssign()
	}
	return m, cmd
}

func (m *App) closeAssign() {
	m.assignForm = nil
	m.assignEntities = nil
	m.refreshComposerHint()
}

// assignSubmit performs the write the modal describes: assign, unassign, or create-then-assign — for
// ONE entity or for a whole marked selection.
//
// The three branches each LOOP the entity list. Every write is a separate RPC per entity (the API is
// per-entity: AssignToCategory / UnassignFromCategory take one entity_id each), and a partial failure
// is REPORTED as one — "assigned 3 of 5" — rather than stopping at the first error, because the ids
// after the failure are just as valid as the ones before it and abandoning them would leave the
// selection half-applied with no explanation.
func (m *App) assignSubmit(vals map[string]string) tea.Cmd {
	cl := m.clients
	if cl == nil || cl.Categories == nil {
		return nil
	}
	entities := append([]string(nil), m.assignEntities...)
	if len(entities) == 0 {
		return nil
	}
	target := m.assignTarget
	choice := vals[assignPickField]
	client := cl.Categories

	switch choice {
	case assignNoneValue:
		// "Uncategorized" is a REAL state with its own RPC — it is not "assign to nothing" — and for a
		// selection it means "clear the grouping on all of these", which is a legitimate bulk act.
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			failed := 0
			for _, entity := range entities {
				if _, err := client.UnassignFromCategory(ctx, connect.NewRequest(&apiv1.UnassignFromCategoryRequest{
					EntityId:   entity,
					TargetType: target,
				})); err != nil {
					failed++
				}
			}
			return categoriesMutatedMsg{Op: "unassign", Count: len(entities), Failed: failed}
		}
	case assignNewValue:
		name := strings.TrimSpace(vals[assignNewField])
		if name == "" {
			return nil
		}
		// CREATE THEN ASSIGN, in one command: the operator asked for one gesture that makes a grouping
		// AND puts the item in it, so a failure of either half must be reported as one failure.
		//
		// The CREATE happens ONCE for the whole selection: creating one grouping per entity would make
		// five groupings with five suffixed slugs, when the operator asked for one.
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			created, err := client.CreateCategory(ctx, connect.NewRequest(&apiv1.CreateCategoryRequest{
				TargetType: target,
				Name:       name,
			}))
			if err != nil {
				return categoriesMutatedMsg{Op: "create", Err: err}
			}
			id := created.Msg.GetCategory().GetId()
			if id == "" {
				return categoriesMutatedMsg{Op: "create", Err: fmt.Errorf("created category came back without an id")}
			}
			failed := 0
			for _, entity := range entities {
				if _, err := client.AssignToCategory(ctx, connect.NewRequest(&apiv1.AssignToCategoryRequest{
					CategoryId: id,
					EntityId:   entity,
					TargetType: target,
				})); err != nil {
					failed++
				}
			}
			return categoriesMutatedMsg{Op: "assign", Count: len(entities), Failed: failed}
		}
	default:
		if choice == "" {
			return nil
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			failed := 0
			for _, entity := range entities {
				if _, err := client.AssignToCategory(ctx, connect.NewRequest(&apiv1.AssignToCategoryRequest{
					CategoryId: choice,
					EntityId:   entity,
					TargetType: target,
				})); err != nil {
					failed++
				}
			}
			return categoriesMutatedMsg{Op: "assign", Count: len(entities), Failed: failed}
		}
	}
}

// categoriesMutatedMsg is the result of any category write. It always RELOADS the list: an assignment
// changes no name and a create changes no assignment, but every one of them changes what the next
// picker must offer, and a stale picker offers a category that no longer exists.
//
// Count/Failed describe a BULK write, so a partial failure can be reported as one ("3 of 5 failed")
// instead of the first error hiding the rest. Failed == 0 with Count > 1 is a clean bulk write.
type categoriesMutatedMsg struct {
	Op     string
	Err    error
	Count  int
	Failed int
}

func (m *App) onCategoriesMutated(msg categoriesMutatedMsg) tea.Cmd {
	if msg.Err != nil {
		m.dock.SetError("category " + msg.Op + ": " + msg.Err.Error())
		return nil
	}
	// A PARTIAL bulk write is REPORTED, not swallowed: some of the operator's selection did not take,
	// and a clean "ok" would leave them believing it all did.
	switch {
	case msg.Failed > 0:
		m.dock.SetError(fmt.Sprintf("category %s: %d of %d failed", msg.Op, msg.Failed, msg.Count))
	case msg.Count > 1:
		m.dock.SetNotice(fmt.Sprintf("category %s: %d ok", msg.Op, msg.Count))
	default:
		m.dock.SetNotice("category " + msg.Op + " ok")
	}
	local := m.pendingCatCmd
	m.pendingCatCmd = nil
	return tea.Batch(local, m.loadCategories())
}

// assignCategoryView composes the modal over the base view, inside a SOLID panel.
func (m *App) assignCategoryView(base string, w, h int) string {
	if m.assignForm == nil {
		return base
	}
	// Same rule as the rename modal: the form gets the panel's INTERIOR width and the panel gives the
	// modal the solid rectangle the operator asked for.
	m.assignForm.Width = m.modalInnerWidth()
	return m.overlayCentered(base, m.modalPanel(m.assignForm.View(), m.modalWidth()))
}

// The rename form's field names, shared with the tests so they drive the form the way the operator
// does rather than by a literal that could drift.
const (
	catAdminName = "name"
	catAdminDesc = "description"
)

// --- managing a grouping FROM THE PANE THAT SHOWS IT -------------------------------------------
//
// The operator: "In the GUI all of that is handled per screen and not in a special section" … "Both
// should honor the same and work in the same way. People should be able to go back and forth between
// TUI and GUI and feel at home."
//
// He is right, and the GUI's placement is specific: rename and delete live ON THE FOLDER ROW itself
// (`CategoryFolder`'s onRename/onDelete, wired in workers.tsx / workflows.tsx / ask-orchicon.tsx), with
// the create dialog per screen too. So these live on the shell — the shell owns the modal host and the
// category cache — and every grouped pane reaches them through ONE pair of hooks. That is what lets
// Control → Categories be removed rather than kept as a second, divergent home for the same actions.

// openRenameCategory opens the PREFILLED rename form for a grouping.
//
// Prefilled for the reason that keeps recurring in this client: an edit box that opens EMPTY makes the
// operator retype a value they cannot see. The GUI's folder rename starts from the current name.
func (m *App) openRenameCategory(categoryID string) {
	cat := m.catByID(categoryID)
	if cat == nil {
		// A stale row: the grouping was deleted (here or in the GUI) between the fetch that drew the row
		// and the keypress. Say so rather than opening a form against an id that no longer exists.
		m.dock.SetError("that grouping is no longer loaded — press r to refresh")
		return
	}
	prevName, prevDesc := cat.GetName(), cat.GetDescription()
	f := kit2.NewForm("Rename grouping",
		kit2.FieldSpec{Name: catAdminName, Label: "Name", Kind: kit2.KText, Required: true, Initial: prevName},
		kit2.FieldSpec{Name: catAdminDesc, Label: "Description", Kind: kit2.KText, Initial: prevDesc},
	)
	f.OnSubmit = func(vals map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(vals[catAdminName])
		if name == "" {
			return nil, fmt.Errorf("a name is required")
		}
		desc := strings.TrimSpace(vals[catAdminDesc])
		if name == prevName && desc == prevDesc {
			return nil, nil // unchanged: valid, and nothing to write (the GUI behaves the same)
		}
		return m.mutateCategory("update", func(ctx context.Context) error {
			n, d := name, desc
			_, err := m.clients.Categories.UpdateCategory(ctx, connect.NewRequest(&apiv1.UpdateCategoryRequest{
				Id: categoryID, Name: &n, Description: &d,
			}))
			return err
		}), nil
	}
	f.Width = m.modalInnerWidth()
	m.catForm = f
	m.catFormID = categoryID
	m.refreshComposerHint()
}

// openDeleteCategory raises the confirm for deleting a grouping, saying where its items go.
//
// The wording is the GUI's own: `Delete "<name>"? Items will move to Uncategorized.` — the operator's
// real question is not whether the grouping goes but what happens to what was in it.
func (m *App) openDeleteCategory(categoryID string) {
	cat := m.catByID(categoryID)
	if cat == nil {
		m.dock.SetError("that grouping is no longer loaded — press r to refresh")
		return
	}
	n := 0
	for _, id := range m.catAssignedBy {
		if id == categoryID {
			n++
		}
	}
	body := fmt.Sprintf("%q? Its %d item(s) will move to Uncategorized.", cat.GetName(), n)
	if n == 0 {
		body = fmt.Sprintf("%q? Nothing is in it.", cat.GetName())
	}
	m.openBulkConfirm("Delete grouping", body, "delete", func() tea.Cmd {
		return m.mutateCategory("delete", func(ctx context.Context) error {
			_, err := m.clients.Categories.DeleteCategory(ctx, connect.NewRequest(&apiv1.DeleteCategoryRequest{Id: categoryID}))
			return err
		})
	})
}

// mutateCategory runs one category write and reloads the cache with it.
func (m *App) mutateCategory(op string, fn func(context.Context) error) tea.Cmd {
	cl := m.clients
	if cl == nil || cl.Categories == nil {
		m.dock.SetError("no category client")
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return categoriesMutatedMsg{Op: op, Err: fn(ctx)}
	}
}

// catFormKey drives the rename form. Like every shell modal it OWNS each key while it is open, so a
// save chord cannot land in the composer behind it.
func (m *App) catFormKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.catForm == nil {
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.catForm = nil
		m.catFormID = ""
		m.refreshComposerHint()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}
	cmd, _ := m.catForm.HandleKey(k)
	if m.catForm != nil && m.catForm.Submitted {
		m.catForm = nil
		m.catFormID = ""
		m.refreshComposerHint()
	}
	return m, cmd
}

// catAdminView composes the rename form over the base view, inside a SOLID panel.
func (m *App) catAdminView(base string, w, h int) string {
	if m.catForm == nil {
		return base
	}
	m.catForm.Width = m.modalInnerWidth()
	return m.overlayCentered(base, m.modalPanel(m.catForm.View(), m.modalWidth()))
}

// applyCategorySet replaces the cached CATEGORIES AND ASSIGNMENTS for one target type with a fresher
// set, leaving the other two kinds alone.
//
// Why per-type rather than "replace everything": the three sets arrive from three different responses,
// each carrying only its own kind (ListWorkers sends worker groupings, ListWorkflows workflow ones,
// ListConversations conversation ones). Replacing wholesale would make whichever response landed last
// the only one that existed.
//
// It is called from the MAIN LOOP only — from a response handler, never from a fetch goroutine.
func (m *App) applyCategorySet(target apiv1.CategoryTargetType, cats []*apiv1.Category, assigns []*apiv1.CategoryAssignment) {
	if len(cats) == 0 && len(assigns) == 0 {
		return
	}
	kept := m.categories[:0:0]
	for _, c := range m.categories {
		if c.GetTargetType() != target {
			kept = append(kept, c)
		}
	}
	m.categories = append(kept, cats...)

	if m.catAssignedBy == nil {
		m.catAssignedBy = map[string]string{}
	}
	// The assignments are REPLACED for this type, never merged: an unassign has to CLEAR the entry, and a
	// merge would leave an item showing a grouping it is no longer in. The map's key already carries the
	// target type (catEntityKey), so the type's own slice is a prefix sweep.
	prefix := fmt.Sprintf("%d\x00", int32(target))
	for k := range m.catAssignedBy {
		if strings.HasPrefix(k, prefix) {
			delete(m.catAssignedBy, k)
		}
	}
	for _, a := range assigns {
		if a.GetTargetType() != target {
			continue
		}
		m.catAssignedBy[catEntityKey(target, a.GetEntityId())] = a.GetCategoryId()
	}
}
