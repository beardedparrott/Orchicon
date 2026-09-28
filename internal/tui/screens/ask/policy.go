package ask

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// policy.go is the Ask screen's two list surfaces, both driven by the ONE
// storage owner (the policy file, whose storage the sibling task owns):
//
//   - /grants      the SESSION roll-up  — what has been allowed for this
//                  conversation's directories, revocable.
//   - /permissions the PERSISTENT list  — allow/deny entries, listable,
//                  addable and removable.
//
// The persistent list is deliberately NOT called /policy: the GUI's /policies
// route is the Rego policy ENGINE, a different concept, and reusing the word
// would be a parity lie.
//
// IT IS A MODAL THE SCREEN OWNS (the enforcement screen's overlay shape): the
// screen claims keys while one is open, so a Rego body or a path containing "q"
// can never reach a shell chord.

type ovKind int

const (
	ovNone ovKind = iota
	ovGrants
	ovPolicyList
	ovPolicyAdd
)

type askOverlay struct {
	kind ovKind
	tbl  *kit2.Table
	form *kit2.Form
	err  string
	// hint states the source-of-truth rule on the persistent list.
	hint string
}

// overlayTableHeight is the row budget a LIST overlay gives its table: the pane
// region minus the block's own furniture (the table's title and header rows, its
// cursor line and the hint under it), so the centered block still fits the frame.
//
// IT MUST BE SET, and leaving it at the zero value was a real defect: kit2.Table
// windows its body to Height-3 with a floor of ONE row, so `/permissions` and
// `/grants` drew a single row however much room the pane had — a two-rule list
// showed "1-1/2" and one rule, which is not a list the operator can read.
func (m *Model) overlayTableHeight() int {
	h := m.h - 4
	if h < 8 {
		h = 8
	}
	return h
}

// openGrants builds the session-grant roll-up.
func (m *Model) openGrants() tea.Cmd {
	ov := &askOverlay{kind: ovGrants, tbl: kit2.NewTable("Session grants",
		kit2.Column{Title: "directory"}, kit2.Column{Title: "tool"}, kit2.Column{Title: "count", Right: true}),
		hint: "enter revokes · esc closes · grants are per session"}
	ov.tbl.Focused = true
	ov.tbl.Width = m.DetailWidth()
	ov.tbl.Height = m.overlayTableHeight()
	ov.tbl.Empty = "no session grants — every write and execution is asked once per directory"
	m.ov = ov
	m.reloadGrants()
	return nil
}

// openPermissions builds the persistent allow/deny list.
func (m *Model) openPermissions() tea.Cmd {
	ov := &askOverlay{kind: ovPolicyList, tbl: kit2.NewTable("Permissions",
		kit2.Column{Title: "effect"}, kit2.Column{Title: "tool"}, kit2.Column{Title: "pattern"}),
		hint: "a adds · enter removes · esc closes · the file is the source of truth — the GUI and a hand-edit see the same list"}
	ov.tbl.Focused = true
	ov.tbl.Width = m.DetailWidth()
	ov.tbl.Height = m.overlayTableHeight()
	ov.tbl.Empty = "no permission rules — every write and execution is asked"
	m.ov = ov
	m.reloadRules()
	return nil
}

func (m *Model) reloadGrants() {
	ov := m.ov
	if ov == nil || ov.kind != ovGrants {
		return
	}
	h, ok := m.consentHost()
	if !ok {
		return
	}
	grants, available := h.ConsentGrants(m.Base.DetailID())
	if !available {
		ov.err = "unavailable on this plane"
		ov.tbl.SetRows(nil)
		return
	}
	ov.err = ""
	rows := make([]kit2.Row, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, kit2.Row{ID: g.Directory, Cells: []string{g.Directory, g.Tool, itoa(g.Count)}})
	}
	ov.tbl.SetRows(rows)
}

func (m *Model) reloadRules() {
	ov := m.ov
	if ov == nil || (ov.kind != ovPolicyList && ov.kind != ovPolicyAdd) {
		return
	}
	h, ok := m.consentHost()
	if !ok {
		return
	}
	store, available := h.ConsentStore()
	if !available {
		ov.err = "unavailable on this plane"
		if ov.tbl != nil {
			ov.tbl.SetRows(nil)
		}
		return
	}
	rules, err := store.Rules()
	if err != nil {
		ov.err = err.Error()
		return
	}
	ov.err = ""
	rows := make([]kit2.Row, 0, len(rules))
	for _, r := range rules {
		rows = append(rows, kit2.Row{ID: r.Effect + "\x00" + r.Tool + "\x00" + r.Pattern, Cells: []string{r.Effect, r.Tool, r.Pattern}})
	}
	ov.tbl.SetRows(rows)
}

// openPolicyAdd opens the add form (effect · tool · pattern).
func (m *Model) openPolicyAdd() {
	f := kit2.NewForm("Add permission rule",
		kit2.FieldSpec{Name: "effect", Label: "Effect", Kind: kit2.KSelect, Required: true,
			Options: []kit2.Option{{Value: "allow", Label: "allow"}, {Value: "deny", Label: "deny"}}, Initial: "deny"},
		kit2.FieldSpec{Name: "tool", Label: "Tool", Kind: kit2.KText, Placeholder: "write | bash | (blank = any)"},
		kit2.FieldSpec{Name: "pattern", Label: "Pattern", Kind: kit2.KText, Required: true, Placeholder: "/path/to/dir/**"},
	)
	f.Width = m.DetailWidth()
	f.Height = 24
	f.Focused = true
	m.ov = &askOverlay{kind: ovPolicyAdd, form: f, tbl: kit2.NewTable("Permissions"),

		hint: "ctrl+s saves · esc cancels · the file is the source of truth"}
	m.reloadRules()
}

// handleOverlayKey routes a key to the open overlay. handled is false when no
// overlay is up.
func (m *Model) handleOverlayKey(k tea.KeyMsg) (tea.Cmd, bool) {
	ov := m.ov
	if ov == nil {
		return nil, false
	}
	switch ov.kind {
	case ovGrants:
		switch k.String() {
		case "esc", "q":
			m.ov = nil
			return nil, true
		case "up", "k":
			ov.tbl.Move(-1)
			return nil, true
		case "down", "j":
			ov.tbl.Move(1)
			return nil, true
		case "enter":
			id := ov.tbl.SelectedID()
			if id == "" {
				return nil, true
			}
			if h, ok := m.consentHost(); ok {
				_ = h.ConsentRevoke(m.Base.DetailID(), id)
			}
			m.reloadGrants()
			return m.repaint(), true
		}
		return nil, true
	case ovPolicyList:
		switch k.String() {
		case "esc", "q":
			m.ov = nil
			return nil, true
		case "up", "k":
			ov.tbl.Move(-1)
			return nil, true
		case "down", "j":
			ov.tbl.Move(1)
			return nil, true
		case "a":
			m.openPolicyAdd()
			return nil, true
		case "enter":
			id := ov.tbl.SelectedID()
			if id == "" {
				return nil, true
			}
			if h, ok := m.consentHost(); ok {
				if store, available := h.ConsentStore(); available {
					effect, tool, pattern := splitRuleID(id)
					if err := store.DeleteRule(effect, tool, pattern); err != nil {
						ov.err = err.Error()
					} else {
						m.reloadRules()
					}
				}
			}
			return nil, true
		}
		return nil, true
	case ovPolicyAdd:
		f := ov.form
		switch k.String() {
		case "esc":
			m.openPermissions()
			return nil, true
		case "ctrl+s":
			if _, err := f.Submit(); err != nil {
				ov.err = err.Error()
				return nil, true
			}
			if f.Submitted {
				m.saveNewRule(f)
			}
			return nil, true
		}
		_, _ = f.HandleKey(k)
		if f.Submitted {
			m.saveNewRule(f)
		}
		return nil, true
	}
	return nil, true
}

func (m *Model) saveNewRule(f *kit2.Form) {
	ov := m.ov
	h, ok := m.consentHost()
	if !ok {
		return
	}
	store, available := h.ConsentStore()
	if !available {
		if ov != nil {
			ov.err = "unavailable on this plane"
		}
		return
	}
	r := chat.PolicyRule{
		Effect:  strings.TrimSpace(f.Values["effect"]),
		Tool:    strings.TrimSpace(f.Values["tool"]),
		Pattern: strings.TrimSpace(f.Values["pattern"]),
	}
	if r.Effect == "" {
		r.Effect = "deny"
	}
	if err := store.UpsertRule(r); err != nil {
		if ov != nil {
			ov.err = err.Error()
		}
		return
	}
	// RE-READ rather than patch: the file is the source of truth, so what is
	// shown after a write is what the file now holds.
	m.openPermissions()
}

func splitRuleID(id string) (effect, tool, pattern string) {
	parts := strings.SplitN(id, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// overlayView renders the open overlay as a block of lines.
func (m *Model) overlayView() string {
	ov := m.ov
	if ov == nil {
		return ""
	}
	var b strings.Builder
	if ov.kind == ovPolicyAdd && ov.form != nil {
		b.WriteString(ov.form.View())
	} else if ov.tbl != nil {
		b.WriteString(ov.tbl.View())
	}
	if ov.err != "" {
		b.WriteString("\n" + ov.err)
	}
	if ov.hint != "" {
		b.WriteString("\n" + ov.hint)
	}
	return b.String()
}
