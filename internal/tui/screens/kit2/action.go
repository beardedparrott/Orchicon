package kit2

import (
	"context"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Action is an RPC bound to the selected entity. It carries:
//
//   - a label + keybinding for the action bar / popover,
//   - an optional confirm prompt (destructive actions),
//   - an optimistic Apply (mutate the local model immediately),
//   - the RPC thunk Do,
//   - and a Rollback that restores the local model when the RPC fails.
//
// Actions never call their RPC directly from a screen: the screen hands the
// Action to mutate.Executor, which runs Do off the update loop, reports
// progress in the dock, rolls back on error, and reconciles the affected
// source.
type Action struct {
	Label   string
	Key     string
	Confirm string // non-empty = require confirmation before running
	Danger  bool
	Apply   func()
	// Rollback undoes Apply (a no-op when Apply is nil).
	Rollback func()
	// Do performs the write. It is only called after confirm + Apply.
	Do func(ctx context.Context) error
	// Source names the list that must be reconciled after success.
	Source string
}

// NeedsConfirm reports whether the action is gated behind a dialog.
func (a Action) NeedsConfirm() bool { return a.Confirm != "" }

// Invoke applies the optimistic change immediately and returns the thunk
// pair the mutation executor needs. It never runs Do itself.
func (a Action) Invoke() (bool, func() error) {
	if a.Apply != nil {
		a.Apply()
	}
	do := a.Do
	if do == nil {
		do = func(context.Context) error { return nil }
	}
	return a.Apply != nil, func() error { return do(context.Background()) }
}

// ActionBar is the footer strip of the selected entity's actions.
type ActionBar struct {
	Actions []Action
	Sel     int
	Width   int
	// HoverHint is the currently highlighted action's label.
	Hint string
}

// NewActionBar builds the bar.
func NewActionBar(actions ...Action) *ActionBar {
	return &ActionBar{Actions: actions}
}

// Selected returns the highlighted action (nil when the bar is empty).
func (b *ActionBar) Selected() *Action {
	if b.Sel < 0 || b.Sel >= len(b.Actions) {
		return nil
	}
	return &b.Actions[b.Sel]
}

// Move shifts the highlight.
func (b *ActionBar) Move(delta int) {
	if len(b.Actions) == 0 {
		return
	}
	b.Sel = (b.Sel + delta + len(b.Actions)) % len(b.Actions)
}

// ByKey returns the action bound to a keybinding (nil when unbound).
func (b *ActionBar) ByKey(key string) *Action {
	for i := range b.Actions {
		if b.Actions[i].Key == key {
			return &b.Actions[i]
		}
	}
	return nil
}

// View renders the bar: "key label" chips, the selected one highlighted.
func (b *ActionBar) View() string {
	if len(b.Actions) == 0 {
		return theme.HintText.Render("no actions")
	}
	parts := make([]string, 0, len(b.Actions))
	for i, a := range b.Actions {
		label := a.Label
		if a.Key != "" {
			label = a.Key + ":" + a.Label
		}
		if i == b.Sel {
			style := theme.ListItemSelected
			if a.Danger {
				style = theme.StatusErr
			}
			parts = append(parts, style.Render(" "+label+" "))
		} else {
			parts = append(parts, theme.HintText.Render("["+label+"]"))
		}
	}
	return strings.Join(parts, " ")
}

// Popover renders the actions as an overlay panel (the "actions popover").
type Popover struct {
	Title   string
	Actions []Action
	Sel     int
}

// NewPopover builds the popover.
func NewPopover(title string, actions ...Action) *Popover {
	return &Popover{Title: title, Actions: actions}
}

// Selected returns the highlighted action.
func (p *Popover) Selected() *Action {
	if p.Sel < 0 || p.Sel >= len(p.Actions) {
		return nil
	}
	return &p.Actions[p.Sel]
}

// Move shifts the highlight.
func (p *Popover) Move(delta int) {
	if len(p.Actions) == 0 {
		return
	}
	p.Sel = (p.Sel + delta + len(p.Actions)) % len(p.Actions)
}

// Box renders the popover as a dialog-sized block.
func (p *Popover) Box(w, h int) string {
	var lines []string
	for i, a := range p.Actions {
		marker := "  "
		if i == p.Sel {
			marker = "▸ "
		}
		line := marker + a.Label
		if a.Key != "" {
			line += " (" + a.Key + ")"
		}
		if a.Confirm != "" {
			line += "  ⚠ confirm"
		}
		lines = append(lines, line)
	}
	d := &Dialog{Title: p.Title, Body: strings.Join(lines, "\n"), Buttons: []string{"run", "close"}}
	return d.Box(w, h)
}
