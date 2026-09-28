package askorchicon

import (
	"strings"
	"testing"
)

// The permission card's text is what an operator decides on, so these pin the
// two things the reported screenshot got wrong:
//
//  1. it dumped Go's own formatting of a nested argument —
//     "batch_write writes=[map[mode:edit new:type chatBus struct {…]",
//     truncated mid-struct at 80 chars — instead of saying what would happen;
//  2. it described the call by its tool name ("batch_write"), which tells an
//     operator nothing, rather than by its intent ("modify <path>").

// The exact shape from the report: a batch write whose `writes` value is a
// []map[string]any, carrying file contents. It must never reach the card as Go
// syntax.
func TestAskSummaryNeverDumpsGoSyntaxForNestedArgs(t *testing.T) {
	a := askAction{
		Tool: "batch_write",
		Input: map[string]any{
			"writes": []any{
				map[string]any{
					"mode":    "edit",
					"new":     "type chatBus struct {\n\tevents chan scheduler.SessionEvent\n}",
					"old":     "type chatBus struct{}",
					"path":    "/p/a.go",
					"dry_run": false,
				},
			},
		},
	}

	got := askSummary(a)

	// The defect: Go's debug formatting of the value, mid-struct.
	for _, leaked := range []string{"map[", "interface {}", "struct {", "chan ", "[{", "0x"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("askSummary leaked Go internals (%q) onto the card: %q", leaked, got)
		}
	}
	// It still says what will happen, and which tool does it.
	if !strings.Contains(got, "modify") {
		t.Fatalf("askSummary = %q — the card must state the INTENT, not just the tool", got)
	}
	if !strings.Contains(got, "batch_write") {
		t.Fatalf("askSummary = %q — the tool name must stay on the card", got)
	}
	// The args are summarised by shape, so the operator can see how much work
	// this is without reading a content dump.
	if !strings.Contains(got, "[1 item]") {
		t.Fatalf("askSummary = %q — a nested list should be summarised by size", got)
	}
}

// scalarArg is the guard itself, so it is asserted directly.
func TestScalarArgSummarisesContainersAndNeverFormatsThem(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "abc", "abc"},
		{"bool", true, "true"},
		{"number", float64(3), "3"},
		{"null", nil, "null"},
		{"list of maps", []any{map[string]any{"a": 1}}, "[1 item]"},
		{"two lists", []any{1, 2}, "[2 items]"},
		{"map", map[string]any{"a": 1, "b": 2}, "[2 fields]"},
		// An unknown type is an ellipsis, NOT fmt's %v: this is the branch that
		// used to be the leak.
		{"unknown type", struct{ X int }{1}, "…"},
		{"pointer", &struct{ X int }{1}, "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scalarArg(tc.in); got != tc.want {
				t.Fatalf("scalarArg(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The intent verb is the whole point of the change: the card must read as an
// ACTION. Naming is deliberately broad because the tool vocabulary is open
// (opencode, MCP, and the native suite each name their tools their own way).
func TestToolIntentVerbNamesTheAction(t *testing.T) {
	cases := map[string]string{
		"write":                 "modify",
		"edit":                  "modify",
		"batch_write":           "modify",
		"orchicon_write":        "modify",
		"bash":                  "run a shell command",
		"shell":                 "run a shell command",
		"read":                  "read",
		"read_file":             "read",
		"grep":                  "read",
		"delete":                "delete",
		"orchicon_unknown_tool": "use the tool",
	}
	for tool, want := range cases {
		if got := toolIntentVerb(tool); got != want {
			t.Errorf("toolIntentVerb(%q) = %q, want %q", tool, got, want)
		}
	}
}

// A bash ask reads as the command itself, with no redundant tool suffix: the
// verb already says it is a shell command.
func TestAskSummaryBashStatesTheCommand(t *testing.T) {
	got := askSummary(askAction{Tool: "bash", Command: "go test ./internal/askorchicon/"})
	if got != "run a shell command: go test ./internal/askorchicon/" {
		t.Fatalf("askSummary = %q", got)
	}
	// A long command is bounded rather than pushing the actions off the screen.
	long := askSummary(askAction{Tool: "bash", Command: strings.Repeat("x", 300)})
	if len(long) > 120 || !strings.HasSuffix(long, "…") {
		t.Fatalf("a long command must be truncated, got %d chars: %q", len(long), long)
	}
}

// The diagnostic for an unresolvable ask belongs in the log, not in the copy an
// operator reads. This is a regression guard for exactly that: the note used to
// be appended to the card's text.
func TestAskSummaryCarriesNoDeveloperDiagnostics(t *testing.T) {
	// An unresolvable ask: a tool name, no target, no command, no args.
	got := askSummary(askAction{Tool: "orchicon_unknown_tool"})
	for _, diagnostic := range []string{
		"no path or command in the ask detail",
		"asking rather than proceeding",
	} {
		if strings.Contains(got, diagnostic) {
			t.Fatalf("askSummary = %q — a developer diagnostic must not be the operator's copy", got)
		}
	}
	// It still names the tool, so the card is not empty of information.
	if !strings.Contains(got, "orchicon_unknown_tool") {
		t.Fatalf("askSummary = %q — the tool name must survive", got)
	}
}
