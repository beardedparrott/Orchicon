package work

// detail_paint_test.go — THE DETAIL PANE IS FULLY PAINTED, on every row, in every channel.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// detailPaintBody is a description carrying every construct that put a reset mid-row: a SHORT
// heading (the case that produced the hole.
const detailPaintBody = `# Short heading

A paragraph that is comfortably longer than the pane so that it wraps, and then a final short line.

## DELIVERED — DO NOT TOUCH

- A bullet whose first line is long enough to wrap, with a chip (` + "`internal/scheduler/reconciler.go`" + `) on
  the continuation line and a second chip (` + "`internal/workitem/validate.go:380`" + `) after it.

` + "```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```" + `

acceptance criteria:
The suite stays green.
`

// workItemWith paints a work item whose body is the fixture above.
func workItemWith(body string) *apiv1.WorkItem {
	return &apiv1.WorkItem{
		Id: "wi-1", Title: "Retire standalone dispatch: enforce workflow-first execution",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1",
		Description: body, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED,
		Priority: 1, SortOrder: 1,
	}
}

// openedDetail renders the Work screen with the detail pane OPEN on the item the plane holds.
func openedDetail(t *testing.T, p *fakePlane) *Model {
	t.Helper()
	m := newModel(t, p)
	m.SetSize(200, 60)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	if cmd := press(t, m, "enter"); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	return m
}

// TestDetailPaneLeavesNoUnpaintedCell is the regression: on a SOLID theme every cell of the detail
// pane carries a background.
func TestDetailPaneLeavesNoUnpaintedCell(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	// A few families rather than one, because the complaint was explicitly "it's not just slate" — and the
	// fix must hold for a light palette and a near-black one alike.
	for _, name := range []string{"slate", "ember", "amber", "lumen", "gruvbox-light"} {
		t.Run(name, func(t *testing.T) {
			prev := theme.Active().Name
			if !theme.Use(name) {
				t.Fatalf("the %q palette is missing", name)
			}
			t.Cleanup(func() { theme.Use(prev) })
			if theme.Active().Transparent {
				t.Skipf("%q is a transparent palette: leaving cells unpainted is the feature there", name)
			}

			p := newPlane()
			p.seedProject("proj-1", "Orchicon")
			p.addItem(workItemWith(detailPaintBody))
			m := openedDetail(t, p)

			// The pane's body must have rendered at all, or "no unpainted cell" is vacuous.
			if !strings.Contains(stripSGR(m.View()), "Short heading") {
				t.Fatalf("the detail body did not render, so this test proves nothing:\n%s", m.View())
			}

			for li, line := range strings.Split(m.View(), "\n") {
				full := []rune(stripSGR(line))
				if len(full) < 105 {
					continue // the left (list) pane; this test is about the detail pane
				}
				cells := cellBackgrounds(line)
				if len(cells) > 100 {
					cells = cells[100:]
				}
				for ci, c := range cells {
					// A CELL MUST CARRY A CONCRETE COLOUR.
					if !strings.HasPrefix(c, "#") {
						t.Errorf("detail row %d cell %d has no pane background (%s) — it renders in the "+
							"terminal's own colour instead of the pane's, which is the operator's \"weird "+
							"color in the whitespace\". Row: %q", li, ci, describeCell(c), string(full))
						break
					}
				}
			}
		})
	}
}

// TestDetailPaneKeepsGenuineHighlights is the other half of the requirement: "unless of course it's
// something special like a highlight." The empty-cells assertion above must NOT be satisfied.
func TestDetailPaneKeepsGenuineHighlights(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	prev := theme.Active().Name
	if !theme.Use("ember") {
		t.Fatal("the ember palette is missing")
	}
	t.Cleanup(func() { theme.Use(prev) })

	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(workItemWith(detailPaintBody))
	m := openedDetail(t, p)

	pane := detailPaneRaw(m.View())

	// The raised surface is what a chip and a code block are drawn ON.
	raised := sgrBackground(theme.Active().SurfaceAlt)
	if raised == "" {
		t.Fatalf("could not derive an SGR background from the raised surface %q", theme.Active().SurfaceAlt)
	}
	if !strings.Contains(pane, raised) {
		t.Fatalf("the detail pane paints no cell with the raised surface (%s, i.e. %q), so the inline-code "+
			"chip and the code block lost their fill — the repair must fix the WHITESPACE, not erase the "+
			"highlights. Pane:\n%s", theme.Active().SurfaceAlt, raised, stripSGR(pane))
	}
}

// sgrBackground renders a #rrggbb colour as the SGR background PARAMETER LIST a truecolor renderer
// emits for it ("48;2;R;G;B"), so a test can look for a fill in the escape stream.
func sgrBackground(c lipgloss.TerminalColor) string {
	hex, ok := c.(lipgloss.Color)
	if !ok || len(hex) != 7 || hex[0] != '#' {
		return ""
	}
	v, err := strconv.ParseUint(string(hex[1:]), 16, 32)
	if err != nil {
		return ""
	}
	return "48;2;" + strconv.Itoa(int(v>>16&0xff)) + ";" + strconv.Itoa(int(v>>8&0xff)) + ";" + strconv.Itoa(int(v&0xff))
}

// --- helpers -----------------------------------------------------------------------------------

// cellBackgrounds walks a rendered row and returns, per printed cell, the background in effect: a
// "#rrggbb" string, "CLEAR" for an explicit reset to the terminal default.
func cellBackgrounds(s string) []string {
	var out []string
	var bg string
	clear := false
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				var params []int
				for _, p := range strings.Split(s[i+2:j], ";") {
					if p == "" {
						params = append(params, 0)
						continue
					}
					v, _ := strconv.Atoi(p)
					params = append(params, v)
				}
				for k := 0; k < len(params); k++ {
					switch {
					case params[k] == 0 || params[k] == 49:
						bg, clear = "", true
					case params[k] == 48 && k+1 < len(params) && params[k+1] == 2 && k+4 < len(params):
						bg, clear = rgbHex(params[k+2], params[k+3], params[k+4]), false
						k += 4
					case params[k] == 48 && k+1 < len(params) && params[k+1] == 5 && k+2 < len(params):
						n := params[k+2]
						bg, clear = rgbHex(n, n, n), false
						k += 2
					}
				}
			}
			i = j + 1
			continue
		}
		switch {
		case bg != "":
			out = append(out, bg)
		case clear:
			out = append(out, "CLEAR")
		default:
			out = append(out, "")
		}
		sz := 1
		for sz < 4 && i+sz < len(s) && s[i+sz]&0xC0 == 0x80 {
			sz++
		}
		i += sz
	}
	return out
}

func rgbHex(r, g, b int) string {
	const hex = "0123456789abcdef"
	return string([]byte{'#', hex[(r>>4)&0xf], hex[r&0xf], hex[(g>>4)&0xf], hex[g&0xf], hex[(b>>4)&0xf], hex[b&0xf]})
}

// stripSGR removes escape sequences so a row can be measured and searched as text.
func stripSGR(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// detailPaneRaw returns the RAW (escape-carrying) text of the detail pane — everything from the
// second pane's left border.
func detailPaneRaw(view string) string {
	var b strings.Builder
	for _, line := range strings.Split(view, "\n") {
		if len([]rune(stripSGR(line))) < 100 {
			continue
		}
		b.WriteString(afterCells(line, 100))
		b.WriteByte('\n')
	}
	return b.String()
}

// afterCells returns s with its first n PRINTED cells removed, leaving the escapes around them
// intact.
func afterCells(s string, n int) string {
	cells := 0
	i := 0
	for i < len(s) && cells < n {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		sz := 1
		for sz < 4 && i+sz < len(s) && s[i+sz]&0xC0 == 0x80 {
			sz++
		}
		i += sz
		cells++
	}
	return s[i:]
}

// describeCell names a cell's state for a failure message: a colour, an explicit reset, or nothing at all.
func describeCell(c string) string {
	switch {
	case strings.HasPrefix(c, "#"):
		return c
	case c == "CLEAR":
		return "reset to the terminal default"
	case c == "":
		return "no background set at all"
	default:
		return c
	}
}
