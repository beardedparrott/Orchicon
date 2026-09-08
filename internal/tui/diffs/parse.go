package diffs

import (
	"regexp"
	"strings"
)

// hunkRE matches a unified-diff hunk header:
//
//	@@ -a[,b] +c[,d] @@
//
// Mirrors sideBySide.ts HUNK_RE exactly (optional ,count defaults to 1).
var hunkRE = regexp.MustCompile(`^@@ -(\d+)(,(\d+))? \+(\d+)(,(\d+))? @@\s*$`)

// hunkHeader is a parsed unified-diff hunk header.
type hunkHeader struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
}

func parseHunkHeader(line string) (hunkHeader, bool) {
	m := hunkRE.FindStringSubmatch(line)
	if m == nil {
		return hunkHeader{}, false
	}
	oldCount := 1
	if m[3] != "" {
		oldCount = atoi(m[3])
	}
	newCount := 1
	if m[6] != "" {
		newCount = atoi(m[6])
	}
	return hunkHeader{
		OldStart: atoi(m[1]),
		OldCount: oldCount,
		NewStart: atoi(m[4]),
		NewCount: newCount,
	}, true
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// ParseUnifiedDiff splits a server-computed unified diff into side-by-side
// rows. It walks the hunk headers to track the running old/new line numbers
// and emits one row per context/add/delete line. Row kinds map 1:1 to the
// diff operation (" ", "+", "-"). Hunk-boundary rows are skipped (no
// visible line) but the line-number state is preserved.
//
// This is a faithful port of sideBySide.ts parseUnifiedDiff.
func ParseUnifiedDiff(unified string) []Row {
	if unified == "" {
		return []Row{}
	}
	lines := strings.Split(unified, "\n")
	// Skip the trailing empty element that a trailing "\n" produces.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	rows := []Row{}
	oldLine := 0
	newLine := 0
	inHunk := false

	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if h, ok := parseHunkHeader(line); ok {
				oldLine = h.OldStart
				newLine = h.NewStart
				inHunk = true
			}
			continue
		}
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			// File headers (a/ b/ or /dev/null) — not diff content.
			continue
		}
		if !inHunk {
			continue
		}

		marker := line[0]
		body := line[1:]
		// A "\ No newline at end of file" marker — the leading backslash IS
		// the line's first char (marker == '\\'), so after slicing it off
		// body starts with " No newline at end of file". Annotate the
		// preceding row. Mirrors the GUI exactly (⟪no newline⟫ glyph).
		if marker == '\\' && strings.HasPrefix(body, " No newline at end of file") {
			if len(rows) > 0 {
				prev := &rows[len(rows)-1]
				if prev.Sign == "+" {
					prev.NewText += "\u27EA"
				} else {
					prev.OldText += "\u27EA"
				}
			}
			continue
		}

		if marker == '+' {
			rows = append(rows, Row{
				LineNoNew: newLine,
				HasNew:    true,
				Sign:      "+",
				NewText:   body,
				Kind:      KindAdd,
			})
			newLine++
		} else if marker == '-' {
			rows = append(rows, Row{
				LineNoOld: oldLine,
				HasOld:    true,
				Sign:      "-",
				OldText:   body,
				Kind:      KindDel,
			})
			oldLine++
		} else {
			// Context line (" ").
			rows = append(rows, Row{
				LineNoOld: oldLine,
				LineNoNew: newLine,
				HasOld:    true,
				HasNew:    true,
				Sign:      " ",
				OldText:   body,
				NewText:   body,
				Kind:      KindCtx,
			})
			oldLine++
			newLine++
		}
	}

	applyWordEmphasis(rows)
	return rows
}

// applyWordEmphasis pairs a "-" row with the following "+" row (the common
// replace shape) and computes cheap token-level emphasis on both sides.
// Mirrors sideBySide.ts applyWordEmphasis.
func applyWordEmphasis(rows []Row) {
	for i := 0; i < len(rows); i++ {
		r := &rows[i]
		if r.Kind != KindDel {
			continue
		}
		// Find the immediately-following add at the same new-line slot (the
		// replace pair). Skip any run of dels and require an adjacent add.
		j := i + 1
		for j < len(rows) && rows[j].Kind == KindDel {
			j++
		}
		if j < len(rows) && rows[j].Kind == KindAdd {
			oldSpans, newSpans := EmphasizeTokens(r.OldText, rows[j].NewText)
			r.OldSpans = oldSpans
			rows[j].NewSpans = newSpans
		}
	}
}
