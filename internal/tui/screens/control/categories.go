package control

// categories.go — the tenant's CATEGORY groupings (worker / workflow / conversation).
//
// The operator: "conversation/workflows/worker category groupings don't exist (create, rename, and
// delete groupings)."
//
// They existed on the SERVER the whole time — category_service.proto has List/Create/Update/Delete/
// Assign/Unassign/Reorder keyed by target_type, and the GUI consumes all of it (CategoryFolder,
// CreateCategoryDialog, CategoryDndContext). The TUI had no surface and had not even wired the
// client, so this is parity work rather than new capability.
//
// WHY THE MANAGEMENT LIVES HERE. Creating, renaming and deleting a grouping applies to the target
// TYPE, not to any one item, so it is a SETTINGS act and belongs with the other settings surfaces
// this client files under Control (providers, secrets, webhooks, adapters, themes). Assigning an ITEM
// is the opposite kind of act and lives on the pane the item is on — see App.openAssignCategory.
//
// ONE PANE FOR ALL THREE TYPES. To the operator "the groupings" is one idea; to the API it is three
// lists. Listing them together (with the type in the row's meta) keeps it one idea on screen, and it
// is why the fetch fans out over the three types rather than making the operator pick a tab first.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// categoryTargetTypes is the fixed set, in the order the rows are grouped.
func categoryTargetTypes() []apiv1.CategoryTargetType {
	return []apiv1.CategoryTargetType{
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER,
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW,
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
}

// categoryTargetLabel names a type the way the operator says it.
func categoryTargetLabel(t apiv1.CategoryTargetType) string {
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

// categoryTargetOptions is the picker's option list (value = the enum's NUMBER, because a picker
// carries strings and this is the one place the mapping is decided).
func categoryTargetOptions() []kit2.Option {
	var out []kit2.Option
	for _, t := range categoryTargetTypes() {
		out = append(out, kit2.Option{Value: categoryTargetValue(t), Label: categoryTargetLabel(t)})
	}
	return out
}

func categoryTargetValue(t apiv1.CategoryTargetType) string {
	return fmt.Sprintf("%d", int32(t))
}

// categoryTargetFromValue parses a picker value back to the enum (UNSPECIFIED when it does not name
// one, which the forms then refuse).
func categoryTargetFromValue(v string) apiv1.CategoryTargetType {
	for _, t := range categoryTargetTypes() {
		if categoryTargetValue(t) == v {
			return t
		}
	}
	return apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_UNSPECIFIED
}

// categoryByID caches the loaded rows so a form can resolve its own target type and current name
// without a round trip at open time.
type categoryRow struct {
	ID         string
	Name       string
	Desc       string
	Target     apiv1.CategoryTargetType
	Slug       string
	SortOrder  int32
	AssignedN  int
	TargetName string
}

// fetchCategories lists every target type and flattens the result into rows.
//
// The fan-out is THREE requests for the whole pane, not one per row: the assignment counts come from
// the same responses (ListCategories returns the assignments alongside the categories), so nothing
// here is N+1.
func (m *Model) fetchCategories(ctx context.Context, _ string) ([]kit2.Item, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	m.catRows = nil
	assigned := map[string]int{}
	for _, t := range categoryTargetTypes() {
		cats, assigns, err := m.rpcListCategories(ctx, t)
		if err != nil {
			return nil, "", err
		}
		for _, a := range assigns {
			assigned[a.GetCategoryId()]++
		}
		for _, c := range cats {
			m.catRows = append(m.catRows, categoryRow{
				ID: c.GetId(), Name: c.GetName(), Desc: c.GetDescription(),
				Target: c.GetTargetType(), Slug: c.GetSlug(), SortOrder: c.GetSortOrder(),
				TargetName: categoryTargetLabel(c.GetTargetType()),
			})
		}
	}
	for i := range m.catRows {
		m.catRows[i].AssignedN = assigned[m.catRows[i].ID]
	}

	// Group by target type in the fixed order, with a heading row per type — the same shape the
	// themes pane uses, and the reason the type does not have to be repeated on every row.
	var items []kit2.Item
	for _, t := range categoryTargetTypes() {
		var group []categoryRow
		for _, r := range m.catRows {
			if r.Target == t {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		items = append(items, kit2.Item{
			ID: "", Title: categoryTargetLabel(t) + " categories", Meta: fmt.Sprintf("%d", len(group)),
		})
		for _, r := range group {
			meta := fmt.Sprintf("%d assigned", r.AssignedN)
			if r.Slug != "" {
				meta = r.Slug + " · " + meta
			}
			items = append(items, kit2.Item{ID: r.ID, Title: r.Name, Meta: meta})
		}
	}
	if len(items) == 0 {
		// An empty pane must SAY so rather than looking broken: the first thing the operator needs to
		// know is that they have to create one.
		items = append(items, kit2.Item{ID: "", Title: "no groupings yet — press n to create one"})
	}
	return items, "", nil
}

// categoryRowFor finds a loaded row (nil when the id is not in the cache).
func (m *Model) categoryRowFor(id string) *categoryRow {
	for i := range m.catRows {
		if m.catRows[i].ID == id {
			return &m.catRows[i]
		}
	}
	return nil
}

// categoryActions are the row's own actions, following the pane's other lists exactly: a create on the
// pane, and edit/delete on the selected row.
func (m *Model) categoryActions(item kit2.Item) []kit2.Action {
	id := item.ID
	if id == "" {
		// A heading (or the empty-state row): only creation applies.
		return nil
	}
	row := m.categoryRowFor(id)
	if row == nil {
		return nil
	}
	targetName := row.TargetName
	return []kit2.Action{
		{
			// RENAME opens the prefilled edit form. It carries no `Do`: the form's own OnSubmit issues
			// the UpdateCategory call, because the write needs a value the operator is about to type —
			// which is exactly the case this action layer cannot express.
			Label: "rename", Key: "e", Source: "categories",
			Apply: func() {},
			Do:    func(context.Context) error { return nil },
		},
		{
			Label: "delete", Key: "x", Source: "categories", Danger: true,
			Confirm: "Delete the " + targetName + " grouping “" + row.Name + "”? Items keep their data and move to Uncategorized.",
			Apply:   func() {},
			Do: func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				return m.rpcDeleteCategory(ctx, id)
			},
		},
	}
}

// newCategoryForm is the create form (the pane's `n`).
func (m *Model) newCategoryForm() *kit2.Form {
	f := kit2.NewForm("New grouping",
		kit2.FieldSpec{
			Name: "target", Label: "Applies to", Kind: kit2.KSelect,
			Options: categoryTargetOptions(), Initial: categoryTargetValue(categoryTargetTypes()[0]),
		},
		kit2.FieldSpec{
			Name: "name", Label: "Name", Kind: kit2.KText, Required: true,
			Placeholder: "e.g. Frontend",
		},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KText},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		target := categoryTargetFromValue(v["target"])
		if target == apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_UNSPECIFIED {
			return nil, fmt.Errorf("choose what the grouping applies to")
		}
		name := strings.TrimSpace(v["name"])
		if name == "" {
			return nil, fmt.Errorf("a name is required")
		}
		desc := strings.TrimSpace(v["description"])
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := m.rpcCreateCategory(ctx, &apiv1.CreateCategoryRequest{
				TargetType: target, Name: name, Description: desc,
			})
			return categoryCreatedMsg{Err: err}
		}, nil
	}
	return f
}

// categoryEditForm is the rename form, PREFILLED with the current name and description — the same
// rule as the conversation rename: an edit box that opens empty makes the operator retype a value
// they cannot see.
func (m *Model) categoryEditForm(id, name, desc string) *kit2.Form {
	f := kit2.NewForm("Rename grouping",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: name},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KText, Initial: desc},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		newName := strings.TrimSpace(v["name"])
		if newName == "" {
			return nil, fmt.Errorf("a name is required")
		}
		newDesc := strings.TrimSpace(v["description"])
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			n, d := newName, newDesc
			err := m.rpcUpdateCategory(ctx, &apiv1.UpdateCategoryRequest{
				Id: id, Name: &n, Description: &d,
			})
			return categoryCreatedMsg{Err: err}
		}, nil
	}
	return f
}

// categoryCreatedMsg triggers a reload of the pane after a create or an update. It does not carry the
// row: the list is the source of truth for names AND for the grouping, and rebuilding it from the
// server is what keeps the two in step.
type categoryCreatedMsg struct{ Err error }
