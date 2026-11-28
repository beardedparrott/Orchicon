package db

import (
	"strings"
	"testing"
)

// THE BUG THIS PINS. scanTenantSettings is shared by THREE read lists — the GetTenantSettings SELECT, its
// create-default INSERT … RETURNING, and the upsert's RETURNING — and each must name exactly the columns the
// scan has destinations for, in the same order. Two columns were added to the scan but dropped from all three
// lists, so EVERY settings read failed with
//
//	db: scan tenant settings: number of field descriptions must equal number of destinations, got 72 and 74
//
// and every consumer of tenant settings (the Settings screen, the scheduler, the reaper, auth TTLs) saw an
// error or fell back to defaults. The DB round-trip test would catch it, but it skips without a DSN; this one
// runs everywhere.

// readList returns the top-level select expressions between start and end markers, searched from `from`.
func readList(t *testing.T, src, from, start, end string) []string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("could not find %q", from)
	}
	body := src[i:]
	s := strings.Index(body, start)
	if s < 0 {
		t.Fatalf("could not find %q after %q", start, from)
	}
	body = body[s+len(start):]
	e := strings.Index(body, end)
	if e < 0 {
		t.Fatalf("could not find %q after %q", end, start)
	}
	return splitTopLevel(strings.Join(strings.Fields(body[:e]), " "))
}

func TestTenantSettingsReadListsMatchTheScan(t *testing.T) {
	src := settingsUpsertText(t)

	// Count the scan's destinations.
	si := strings.Index(src, "func scanTenantSettings(")
	if si < 0 {
		t.Fatal("could not find scanTenantSettings")
	}
	scanBody := src[si:]
	scanBody = scanBody[:strings.Index(scanBody, "); err != nil")]
	dests := strings.Count(scanBody, "&r.")

	lists := map[string][]string{
		"GetTenantSettings SELECT":       readList(t, src, "func GetTenantSettings(", "const q = `SELECT", "FROM tenant_settings"),
		"create-default RETURNING":       readList(t, src, "const insertQ = `", "RETURNING", "`"),
		"UpdateTenantSettings RETURNING": readList(t, src, "func UpdateTenantSettings(", "RETURNING", "`"),
	}
	var ref []string
	for name, cols := range lists {
		if len(cols) != dests {
			t.Errorf("%s names %d columns but scanTenantSettings has %d destinations", name, len(cols), dests)
		}
		if ref == nil {
			ref = cols
			continue
		}
		if strings.Join(cols, ",") != strings.Join(ref, ",") {
			t.Errorf("%s differs from the other read lists — all three feed the same scan and must be identical", name)
		}
	}
	// The typed context-editing knobs must be read back, not only written.
	for _, c := range []string{"max_tool_output_bytes", "context_editing_keep_tool_calls"} {
		for name, cols := range lists {
			found := false
			for _, x := range cols {
				if x == c {
					found = true
				}
			}
			if !found {
				t.Errorf("%s does not read %s", name, c)
			}
		}
	}
}
