package tui

// slash_alias_visibility_test.go — a slash command's ALIAS must be visible and findable.
//
// The operator: "/quit is an actual slash command that shows up in the list. /exit works but
// doesn't get displayed in the slash command list as a valid slash command. We should fix
// that just for consistency."
//
// Both halves were broken, and they were broken in DIFFERENT places — which is why the two
// surfaces disagreed:
//
//   * /help DID render aliases (helpLines → aliasSuffix).
//   * the "/" PALETTE did not: refreshPalette skipped every alias row outright AND matched
//     the filter against the PRIMARY name only (`strings.Contains(c.Name, query)`), so typing
//     "/exit" listed "no matching commands" while submitting "/exit" worked perfectly. A
//     command you can run but cannot see is the inconsistency.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// paletteFor opens the palette seeded from the composer text, the way typing "/" does.
//
// The size is set the way the shell tests do: the palette clamps its content width, so a 0x0
// fixture renders a row narrower than any real terminal would and would assert against an
// artifact rather than against what the operator sees.
func paletteFor(t *testing.T, value string) *App {
	t.Helper()
	m := NewApp(&client.Clients{}, &config.Profile{URL: "http://x", Token: "t"}, "v9.9.9")
	m.width, m.height = 120, 40
	m.dock.Focus()
	m.dock.SetValue(value)
	m.openPalette()
	return m
}

func paletteText(m *App) string {
	var b strings.Builder
	for _, c := range m.palette.filter {
		b.WriteString(c.Name + " ")
	}
	return b.String()
}

// TYPING THE ALIAS MUST FIND THE COMMAND. "/exit" resolves and runs, so the palette must not
// answer "no matching commands" for it.
func TestPaletteFilterFindsAliases(t *testing.T) {
	m := paletteFor(t, "/exit")
	if len(m.palette.filter) == 0 {
		t.Fatal(`typing "/exit" listed no commands, although "/exit" is a valid command that runs`)
	}
	found := false
	for _, c := range m.palette.filter {
		if c.Name == "/quit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the filter for %q surfaced %q, want the command it aliases", "/exit", paletteText(m))
	}
	// The primary name still filters as before.
	m2 := paletteFor(t, "/quit")
	if len(m2.palette.filter) == 0 {
		t.Fatal(`typing "/quit" listed no commands`)
	}
	// And a bare "/" still lists everything.
	m3 := paletteFor(t, "/")
	if len(m3.palette.filter) < 2 {
		t.Fatalf("a bare slash listed %d command(s), want the whole set", len(m3.palette.filter))
	}
}

// THE ALIAS MUST BE DISPLAYED. A row that runs for "/exit" must say so, or the operator has
// no way to learn the spelling exists.
func TestPaletteRowShowsTheAlias(t *testing.T) {
	// Filter to the command so its row is the VISIBLE one: the palette viewport shows only a
	// few rows and /quit is not near the top of the full list.
	m := paletteFor(t, "/quit")
	if len(m.palette.filter) != 1 || m.palette.filter[0].Name != "/quit" {
		t.Fatalf("filtering /quit surfaced %q, want just /quit", paletteText(m))
	}
	quit := m.palette.filter[0]
	if len(quit.Aliases) == 0 {
		t.Fatal("fixture: /quit is expected to carry the /exit alias")
	}
	// The rendered row must carry the alias, exactly as /help does.
	view := m.paletteView()
	if !strings.Contains(view, "/exit") {
		t.Errorf("the palette row for /quit does not show its /exit alias:\n%s", view)
	}
	// AND THE ALIAS MUST COME BEFORE THE DESCRIPTION. The content is truncated to the palette
	// width, and a long description is common — appending the alias after it would let a wide
	// row slice the alias off the end, which is the very "doesn't get displayed" this fixes.
	row := ""
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "/quit") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("no row for /quit in the palette:\n%s", view)
	}
	ai, di := strings.Index(row, "/exit"), strings.Index(row, "exit orch")
	if ai < 0 || di < 0 || ai > di {
		t.Errorf("the alias is not placed before the description, so truncation could hide it:\n%s", row)
	}
	// The description must survive intact on this row — the alias must not push it off.
	if !strings.Contains(row, "terminal state restored") {
		t.Errorf("adding the alias truncated the description:\n%s", row)
	}
	// And /help agrees — the two surfaces must not drift.
	help := strings.Join(m.slash.helpLines(), "\n")
	if !strings.Contains(help, "/exit") {
		t.Errorf("/help does not show the alias either:\n%s", help)
	}
}

// One row, not two: an alias is the same command, so listing it separately would be a
// phantom second entry with identical behaviour.
func TestAliasesDoNotDuplicateRows(t *testing.T) {
	m := paletteFor(t, "/")
	seen := map[string]int{}
	for _, c := range m.palette.filter {
		seen[c.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times in the palette", name, n)
		}
	}
	// Specifically: /exit is not a row of its own.
	if _, ok := seen["/exit"]; ok {
		t.Error("/exit is listed as its own row rather than as the alias of /quit")
	}
}

// Selecting the filtered row runs the command the operator meant — the alias resolves to the
// same Run, so accepting "/exit" from the palette must quit.
func TestSelectingByAliasRunsTheCommand(t *testing.T) {
	m := paletteFor(t, "/exit")
	if len(m.palette.filter) == 0 {
		t.Fatal("fixture: the alias filter found nothing")
	}
	sel := m.paletteSelected()
	if sel == nil {
		t.Fatal("no command selected")
	}
	if sel.Run == nil {
		t.Fatal("the selected command has no Run")
	}
	cmd := sel.Run(m, nil)
	if !m.quitting {
		t.Error("running the command selected by its alias did not set quitting")
	}
	if cmd == nil {
		t.Error("running the command selected by its alias issued no cmd")
	}
}
