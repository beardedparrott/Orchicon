package db

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE BUG THIS PINS. The tenant-settings upsert is ONE positional statement: the INSERT column list and
// the VALUES placeholder list must line up index-for-index. Nothing in the type system enforces it, so a
// missing or displaced placeholder silently shifts every value after it — which surfaced to the operator as
//
//	db: update tenant settings: ERROR: column "execution_retention_timeout_seconds"
//	    is of type integer but expression is of type text (SQLSTATE 42804)
//
// because a text value landed in an integer column. The statement had also been shipping with `$24` missing
// entirely, which shifted everything from position 24 onward by one — a latent corruption that only became
// visible once a save path was exercised end to end.
//
// A save touches EVERY column, so this must hold exactly; an approximate check is worthless.

// statements extracts the three positional lists of the upsert from settings.go.
func settingsUpsertText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("settings.go")
	if err != nil {
		t.Fatalf("read settings.go: %v", err)
	}
	return string(raw)
}

// splitTopLevel splits a SQL list on commas that are not inside parentheses.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	cur := strings.Builder{}
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		}
		if ch == ',' && depth == 0 {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(ch)
	}
	if v := strings.TrimSpace(cur.String()); v != "" {
		out = append(out, v)
	}
	return out
}

func TestTenantSettingsInsertPlaceholdersAlign(t *testing.T) {
	src := settingsUpsertText(t)

	// Locate the INSERT column list and its VALUES list.
	startIdx := strings.Index(src, "const q = `INSERT INTO tenant_settings (")
	if startIdx < 0 {
		t.Fatal("could not find the tenant_settings INSERT statement")
	}
	body := src[startIdx:]
	valsIdx := strings.Index(body, ") VALUES (")
	if valsIdx < 0 {
		t.Fatal("could not find the VALUES list")
	}
	// The column list begins after the opening paren of the INSERT's column list.
	colOpen := strings.Index(body, "(")
	colPart := body[colOpen+1 : valsIdx]
	// The VALUES list ends at the closing paren that precedes ON CONFLICT.
	valsStart := valsIdx + len(") VALUES (")
	valsEnd := strings.Index(body[valsStart:], ")\n\tON CONFLICT")
	if valsEnd < 0 {
		t.Fatal("could not find the end of the VALUES list")
	}
	valsPart := body[valsStart : valsStart+valsEnd]

	cols := splitTopLevel(strings.ReplaceAll(colPart, "\n", " "))
	vals := splitTopLevel(strings.ReplaceAll(valsPart, "\n", " "))

	if len(cols) == 0 || len(vals) == 0 {
		t.Fatalf("parsed nothing: %d columns, %d values", len(cols), len(vals))
	}

	// Build the expected placeholder index for each column POSITION, in order.
	//
	// The lists are not required to be the same LENGTH — a column can be fed by an expression such as
	// COALESCE(NULLIF($70,''),'oauth-personal') or now() — so the check is on the ORDER of the placeholder
	// NUMBERS that appear, which must be 1,2,3,… with none skipped and none repeated. A skipped number is
	// exactly the `$24` bug; a displaced one is what produced the type error.
	seen := map[int]bool{}
	last := 0
	re := regexp.MustCompile(`\$(\d+)`)
	var numbers []int
	for _, v := range vals {
		if m := re.FindStringSubmatch(v); m != nil {
			n := 0
			for _, c := range m[1] {
				n = n*10 + int(c-'0')
			}
			numbers = append(numbers, n)
			if seen[n] {
				t.Errorf("placeholder $%d appears twice in VALUES", n)
			}
			seen[n] = true
			if n != last+1 {
				t.Errorf("placeholder sequence jumps from $%d to $%d — every value after a gap shifts by one", last, n)
			}
			last = n
		}
	}

	// Every column must be present in the column list exactly once.
	colSeen := map[string]bool{}
	for _, c := range cols {
		base := c
		if i := strings.Index(base, "("); i > 0 {
			base = strings.TrimSpace(base[:i])
		}
		base = strings.TrimSpace(strings.SplitN(base, " ", 2)[0])
		if base == "" {
			continue
		}
		if colSeen[base] {
			t.Errorf("column %q appears twice in the INSERT list", base)
		}
		colSeen[base] = true
	}
}
