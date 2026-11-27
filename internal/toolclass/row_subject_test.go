package toolclass_test

// row_subject_test.go — the ONE-LINE tool-row subject, driven from the CROSS-CLIENT fixture.
//
// The same bytes are read by the GUI's Vitest suite (frontend/src/lib/tool-row-subject.test.ts), so a
// change to either implementation that is not mirrored in the other fails HERE. The agy cases are the
// reason the file exists: agy's argument keys (`CommandLine`, `TargetFile`, `Url`, `query`) are not
// opencode's (`command`, `file_path`), so a renderer that knew only one vocabulary drew a bare tool name
// with no subject for every antigravity call.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/toolclass"
)

type rowSubjectFixture struct {
	Cases []struct {
		Name  string `json:"name"`
		Tool  string `json:"tool"`
		Input string `json:"input"`
		Want  string `json:"want"`
	} `json:"cases"`
}

func TestRowSubjectAgainstSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/row_subject_fixture.json")
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var fx rowSubjectFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse shared fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("the shared fixture has no cases — the Go/TS parity contract for tool rows would be vacuous")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got := toolclass.RowSubject(c.Tool, c.Input); got != c.Want {
				t.Errorf("RowSubject(%q, %q) = %q, want %q", c.Tool, c.Input, got, c.Want)
			}
		})
	}
}

// TestRowSubjectCoversEveryAgToolName is the completeness half: EVERY agy tool name the platform
// canonicalises must produce a subject when its recorded argument shape is present. A name that fell
// through to "" is a row an operator reads as "the model called something, unclear what".
func TestRowSubjectCoversEveryAgToolName(t *testing.T) {
	cases := []struct{ agyName, canonical, input, want string }{
		{"run_command", "bash", `{"CommandLine":"go build ./..."}`, "go build ./..."},
		{"write_file", "write", `{"TargetFile":"/p/a.md"}`, "/p/a.md"},
		{"write_to_file", "write", `{"TargetFile":"/p/b.md"}`, "/p/b.md"},
		{"replace_file_content", "edit", `{"TargetFile":"/p/c.go"}`, "/p/c.go"},
		{"multi_replace_file_content", "edit", `{"TargetFile":"/p/d.go"}`, "/p/d.go"},
		{"view_file", "read", `{"AbsolutePath":"/p/e.go"}`, "/p/e.go"},
		{"read_file", "read", `{"AbsolutePath":"/p/f.go"}`, "/p/f.go"},
		{"grep_search", "grep", `{"query":"Ask"}`, "Ask"},
		{"list_dir", "ls", `{"DirectoryPath":"/p/internal"}`, "/p/internal"},
		{"search_web", "websearch", `{"query":"agy"}`, "agy"},
		{"read_url_content", "webfetch", `{"Url":"https://x.example.com/y"}`, "https://x.example.com/y"},
	}
	for _, c := range cases {
		t.Run(c.agyName, func(t *testing.T) {
			got := toolclass.RowSubject(c.canonical, c.input)
			if got != c.want {
				t.Fatalf("RowSubject(%q, …) = %q, want %q (agy tool %q)", c.canonical, got, c.want, c.agyName)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatalf("agy tool %q renders NO subject — the row says nothing about the call", c.agyName)
			}
		})
	}
}

// TestRowSubjectIsTotal pins the never-panics / never-invents contract on the shapes a part can carry
// when an adapter misbehaves: a truncated JSON, a JSON null, an array, a number.
func TestRowSubjectIsTotal(t *testing.T) {
	for _, in := range []string{
		"", "null", "  ", "{", "[]", "[1,2,3]", "42", "true", `{"unclosed":`,
	} {
		if got := toolclass.RowSubject("bash", in); got != "" {
			t.Errorf("RowSubject(\"bash\", %q) = %q, want \"\" — a malformed input must not fabricate a subject", in, got)
		}
	}
	// An unknown tool name with no scalar value is also empty, not a dump.
	if got := toolclass.RowSubject("some_future_tool", `{"nested":{"a":1}}`); got != "" {
		t.Errorf("an all-object input rendered %q; only scalar values are row subjects", got)
	}
}

// TestIsWebTool pins the label set the renderers use to say what kind of subject follows.
func TestIsWebTool(t *testing.T) {
	for _, n := range []string{"webfetch", "web_fetch", "read_url_content", "websearch", "web_search", "search_web", "WebFetch"} {
		if !toolclass.IsWebTool(n) {
			t.Errorf("IsWebTool(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"bash", "read", "write", "", "grep"} {
		if toolclass.IsWebTool(n) {
			t.Errorf("IsWebTool(%q) = true, want false", n)
		}
	}
}

// TestCollapseWhitespaceKeepsOneRow pins the geometry guarantee: a heredoc command must not break the
// transcript's row count.
func TestCollapseWhitespaceKeepsOneRow(t *testing.T) {
	got := toolclass.CollapseWhitespace("cat > f <<'EOF'\nline one\nline two\nEOF")
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("CollapseWhitespace left a newline: %q", got)
	}
	if got != "cat > f <<'EOF' line one line two EOF" {
		t.Fatalf("CollapseWhitespace = %q", got)
	}
}
