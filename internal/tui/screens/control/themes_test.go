package control

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// The operator's "there is no themes under settings": Control must expose a
// Themes pane listing the TUI's palette set, marking the active one, and
// offering an action that applies the selection.
func TestThemesPaneListsPalettes(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	theme.Use("dark")

	m := New(nil, nil)
	items, _, err := m.fetchThemes(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchThemes: %v", err)
	}
	// Every palette is listed, plus the two section headings (DARK / LIGHT).
	var palettes int
	for _, it := range items {
		if it.ID != "" {
			palettes++
		}
	}
	if palettes != len(theme.Names()) {
		t.Fatalf("themes pane lists %d palettes, want %d", palettes, len(theme.Names()))
	}
	// The list is separated into dark and light sections, in that order.
	var heads []string
	for _, it := range items {
		if it.ID == "" {
			heads = append(heads, it.Title)
		}
	}
	if strings.Join(heads, ",") != "DARK,LIGHT" {
		t.Fatalf("expected DARK then LIGHT headings, got %v", heads)
	}
	var active int
	for _, it := range items {
		if it.Meta == "active" {
			active++
			if it.ID != "dark" {
				t.Fatalf("active marker on %q, want dark", it.ID)
			}
		}
	}
	if active != 1 {
		t.Fatalf("%d rows marked active, want exactly 1", active)
	}
}

// Selecting a non-active palette offers an apply action; the active one offers
// nothing (there is nothing to do).
func TestThemesPaneOffersApply(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	theme.Use("dark")

	m := New(nil, nil)
	if !m.SelectSource("themes") {
		t.Fatal("themes must be a registered source")
	}
	items, _, _ := m.fetchThemes(context.Background(), "")
	m.LoadItems("themes", items, "")
	if !m.SelectItem("themes", "dark") {
		t.Fatal("could not select the active theme row")
	}
	if acts := m.actionsForSelection(); len(acts) != 0 {
		t.Fatalf("the ACTIVE theme must offer no action, got %d", len(acts))
	}
	if !m.SelectItem("themes", "gruvbox-dark") {
		t.Fatal("could not select gruvbox-dark")
	}
	acts := m.actionsForSelection()
	if len(acts) != 1 {
		t.Fatalf("a selectable theme must offer exactly 1 action, got %d", len(acts))
	}
	if acts[0].Label != "apply" {
		t.Fatalf("action label = %q, want apply", acts[0].Label)
	}
	// Applying switches the live palette.
	acts[0].Apply()
	if theme.Active().Name != "gruvbox-dark" {
		t.Fatalf("active theme = %q after apply, want gruvbox-dark", theme.Active().Name)
	}
}

// The pane's detail describes the theme and names the apply key.
func TestThemesDetailDescribesPalette(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	theme.Use("dark")

	m := New(nil, nil)
	title, fields, body, err := m.detail(context.Background(), "themes", "light")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !strings.Contains(title, "light") {
		t.Fatalf("detail title = %q, want it to name the theme", title)
	}
	var joined strings.Builder
	for _, f := range fields {
		joined.WriteString(f.Key + "=" + f.Value + " ")
	}
	if !strings.Contains(joined.String(), "state=available") {
		t.Fatalf("fields = %q, want state=available for a non-active theme", joined.String())
	}
	if !strings.Contains(body, "contrast") {
		t.Fatalf("detail body must explain the terminal-contrast rationale: %q", body)
	}
	_ = tea.Quit
}
