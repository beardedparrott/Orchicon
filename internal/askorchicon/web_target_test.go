package askorchicon

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE WEB-TOOL TARGET IS NOT A PATH.
//
// The claude adapter raises a consent card for WebFetch/WebSearch whose Targets are the URL and the
// query, so the operator can see WHAT is being approved. Before this guard those strings fell into
// the path layer: absAskTarget joined the relative target onto the scope dir, so a fetch of
// https://antigravity.google/docs resolved to <project>/https:/antigravity.google/docs. That
// fabricated path is what the card printed as the grant directory, it lies INSIDE the project (so a
// project-boundary rule waves it through), and it is the key a "never ask again in this directory"
// grant is filed under — a directory no later fetch can ever match.
//
// The URL must still reach the CARD. Only the path machinery ignores it.

const webProbeURL = "https://antigravity.google/docs"

func TestWebToolTargetIsNotResolvedAsAPath(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"WebFetch", "WebSearch", "webfetch"} {
		a := askAction{Tool: tool, Targets: []string{webProbeURL}}

		if got := absAskTarget(a, dir); got != filepath.Clean(dir) {
			t.Errorf("%s: absAskTarget = %q, want the scope dir %q — a URL must not be joined onto it",
				tool, got, filepath.Clean(dir))
		}
		if got := resolveAskKey(a, dir); got != filepath.Clean(dir) {
			t.Errorf("%s: resolveAskKey = %q, want %q — a grant must not be keyed on a fabricated URL path",
				tool, got, filepath.Clean(dir))
		}
		for _, d := range decisionTargets(a, dir) {
			if strings.Contains(d.abstarget, "antigravity") || strings.Contains(d.key, "antigravity") {
				t.Errorf("%s: decisionTargets invented the path %q (key %q) from a URL",
					tool, d.abstarget, d.key)
			}
			if strings.HasPrefix(d.abstarget, dir) && d.abstarget != filepath.Clean(dir) {
				t.Errorf("%s: decisionTargets claims %q is inside the project %q", tool, d.abstarget, dir)
			}
		}
	}
}

// The URL must survive to the CARD — that is the whole point of raising it.
func TestWebToolURLStillReachesTheCard(t *testing.T) {
	a := askAction{Tool: "WebFetch", Targets: []string{webProbeURL}}
	if got := askSummary(a); !strings.Contains(got, webProbeURL) {
		t.Errorf("askSummary = %q, want it to name the URL %q so the operator sees what they approve", got, webProbeURL)
	}
	if len(a.Targets) != 1 || a.Targets[0] != webProbeURL {
		t.Errorf("Targets = %v, want the URL preserved for the card", a.Targets)
	}
}

// A file tool is unaffected: its target is still judged as a path.
func TestFileToolTargetIsStillResolvedAsAPath(t *testing.T) {
	dir := t.TempDir()
	a := askAction{Tool: "Write", Targets: []string{"internal/foo.go"}}
	want := filepath.Join(filepath.Clean(dir), "internal/foo.go")
	if got := absAskTarget(a, dir); got != want {
		t.Errorf("Write: absAskTarget = %q, want %q — file targets must still resolve", got, want)
	}
}

// A "resolves to no path" ask must still ASK (never silently proceed). A web ask carries its
// subject in Targets, not as a path, so it is deliberately unresolved — the card is the outcome.
func TestWebAskWithoutAPathStillAsks(t *testing.T) {
	if askActionResolved(askAction{Tool: "WebFetch", Targets: []string{webProbeURL}}) {
		t.Error("a web ask must not report itself resolved on a URL 'path'")
	}
	// And an ask that genuinely carries nothing must not be resolved either.
	if askActionResolved(askAction{Tool: "WebFetch"}) {
		t.Error("an empty web ask must not report itself resolved")
	}
	// A real path ask is still resolved.
	if !askActionResolved(askAction{Tool: "Write", Targets: []string{"a.go"}}) {
		t.Error("a file ask with a path must report itself resolved")
	}
}

func TestIsNonPathTarget(t *testing.T) {
	for _, s := range []string{"https://x.test/a", "http://x.test", "//x.test/a", "  https://x.test  "} {
		if !isNonPathTarget(s) {
			t.Errorf("isNonPathTarget(%q) = false, want true", s)
		}
	}
	// `a://b` is deliberately NOT in this list: it contains "://", so it is treated as a
	// non-path. That is the safe direction — a target wrongly called a non-path is never turned
	// into a fabricated filesystem path, whereas the reverse is the bug this guards.
	for _, s := range []string{"", "  ", "a.go", "/abs/a.go", "internal/x"} {
		if isNonPathTarget(s) {
			t.Errorf("isNonPathTarget(%q) = true, want false", s)
		}
	}
}

func TestIsWebTool(t *testing.T) {
	for _, s := range []string{"WebFetch", "websearch", " web_fetch ", "Web_Search"} {
		if !isWebTool(s) {
			t.Errorf("isWebTool(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"Write", "Bash", "", "mcp__x__fetch"} {
		if isWebTool(s) {
			t.Errorf("isWebTool(%q) = true, want false", s)
		}
	}
}
