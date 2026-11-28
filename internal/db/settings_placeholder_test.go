package db

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// THE BUG THIS PINS. The tenant-settings upsert is ONE positional statement: column i of the INSERT list
// takes VALUES expression i, and that expression's $N takes bind argument N. Nothing in the type system
// enforces either pairing, and both have broken:
//
//   - columns placed mid-list while their placeholders sat at the end shifted every value after them
//     ("column execution_retention_timeout_seconds is of type integer but expression is of type text");
//   - a placeholder used ONLY by the ON CONFLICT clause ($24, MaxConcurrentRunsSet) was added to VALUES on
//     the false belief that the sequence must be contiguous ("INSERT has more expressions than target
//     columns").
//
// So the check is not "the numbers are 1,2,3,…" — VALUES legitimately skips SET-only arguments. It is:
// the lists are the same length, and each column is fed by the bind argument that NAMES that column.

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

var sqlComment = regexp.MustCompile(`--[^\n]*`)

// norm reduces a column name or a Go bind expression to comparable letters:
// "session_access_token_ttl_seconds" and "in.SessionAccessTokenTtlSeconds" both become
// "sessionaccesstokenttlseconds".
func norm(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "in.")
	return strings.ToLower(strings.NewReplacer("_", "", ".", "").Replace(s))
}

func TestTenantSettingsInsertPlaceholdersAlign(t *testing.T) {
	src := settingsUpsertText(t)

	fi := strings.Index(src, "func UpdateTenantSettings(")
	if fi < 0 {
		t.Fatal("could not find UpdateTenantSettings")
	}
	body := src[fi:]
	qi := strings.Index(body, "const q = `INSERT INTO tenant_settings (")
	if qi < 0 {
		t.Fatal("could not find the tenant_settings INSERT statement")
	}
	stmt := body[qi:]
	colOpen := strings.Index(stmt, "(")
	valsIdx := strings.Index(stmt, ") VALUES (")
	valsStart := valsIdx + len(") VALUES (")
	valsEnd := strings.Index(stmt[valsStart:], ")\n\tON CONFLICT")
	if valsIdx < 0 || valsEnd < 0 {
		t.Fatal("could not delimit the column and VALUES lists")
	}
	cols := splitTopLevel(strings.Join(strings.Fields(sqlComment.ReplaceAllString(stmt[colOpen+1:valsIdx], "")), " "))
	vals := splitTopLevel(strings.Join(strings.Fields(stmt[valsStart:valsStart+valsEnd]), " "))

	// The bind arguments: tx.Query(ctx, q, <args>).
	ai := strings.Index(body, "tx.Query(ctx, q,")
	if ai < 0 {
		t.Fatal("could not find the upsert's bind arguments")
	}
	argText := body[ai+len("tx.Query(ctx, q,"):]
	argText = argText[:strings.Index(argText, "\n\t)")]
	args := splitTopLevel(strings.Join(strings.Fields(argText), " "))

	if len(cols) != len(vals) {
		t.Fatalf("INSERT names %d columns but VALUES has %d expressions", len(cols), len(vals))
	}

	// Every bind argument whose name matches a column must feed THAT column. A shift moves an argument
	// onto a neighbouring column, which this catches by name.
	colByNorm := map[string]string{}
	for _, c := range cols {
		colByNorm[norm(c)] = c
	}
	re := regexp.MustCompile(`\$(\d+)`)
	checked := 0
	for i, v := range vals {
		m := re.FindStringSubmatch(v)
		if m == nil {
			continue // now() and similar
		}
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > len(args) {
			t.Errorf("column %q uses $%d but there are only %d bind arguments", cols[i], n, len(args))
			continue
		}
		arg := args[n-1]
		if want, ok := colByNorm[norm(arg)]; ok {
			checked++
			if want != cols[i] {
				t.Errorf("column %q is fed $%d = %s, which belongs to column %q", cols[i], n, arg, want)
			}
		}
	}
	// Guard the guard: most columns have a same-named argument, so a near-zero count means the parse broke.
	if checked < 30 {
		t.Fatalf("only %d column/argument pairs matched by name — the parse is broken, not the statement", checked)
	}

	colSeen := map[string]bool{}
	for _, c := range cols {
		if colSeen[c] {
			t.Errorf("column %q appears twice in the INSERT list", c)
		}
		colSeen[c] = true
	}
}
