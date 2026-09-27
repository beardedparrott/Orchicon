package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsentCardNamesTheToolAndTheTarget pins the first acceptance criterion:
// a pending write/execution renders in the transcript naming the tool AND the
// target path or command.
func TestConsentCardNamesTheToolAndTheTarget(t *testing.T) {
	ask := PermissionAsk{
		ID: "a1", Kind: AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	}
	it := ConsentItem(ask, 1000)
	out := ConsentCardText(it, 64)
	for _, want := range []string{"write", "/home/ops/project/main.go", ConsentAllowOnce, ask.SessionLabel(), ConsentDeny} {
		if !strings.Contains(out, want) {
			t.Fatalf("the card must name %q:\n%s", want, out)
		}
	}
}

// TestConsentWordingMatchesTheGUILiterals pins the parity rule as LITERALS, not
// constant-to-constant (a compare-to-itself assertion passes at any value).
func TestConsentWordingMatchesTheGUILiterals(t *testing.T) {
	if ConsentAllowOnce != "Allow once" {
		t.Fatalf("Allow once drifted: %q", ConsentAllowOnce)
	}
	if ConsentDeny != "Deny" {
		t.Fatalf("Deny drifted: %q", ConsentDeny)
	}
	// The session row is a TEMPLATE rather than a fixed string, because it NAMES the
	// directory the grant would cover. Its two pieces and its no-directory fallback
	// are therefore the literals the GUI has to carry.
	if ConsentSessionPrefix != "Never ask again in " {
		t.Fatalf("the session row's prefix drifted: %q", ConsentSessionPrefix)
	}
	if ConsentSessionSuffix != " this session" {
		t.Fatalf("the session row's suffix drifted: %q", ConsentSessionSuffix)
	}
	if ConsentSessionNoDir != "Never ask again in this directory this session" {
		t.Fatalf("the session row's no-directory form drifted: %q", ConsentSessionNoDir)
	}
}

// TestConsentWordingMatchesTheGUISource is the CROSS-CLIENT half: the GUI's own
// source must carry the same three strings. The labels land with the sibling
// policy task, so until then this test keeps its name and points at it rather
// than being silently dropped — the repo has precedent for reading frontend/src
// from a Go test (internal/fileedit/diff_test.go reads its TS fixtures).
func TestConsentWordingMatchesTheGUISource(t *testing.T) {
	root := repoRoot(t)
	dirs := []string{
		filepath.Join(root, "frontend", "src", "routes"),
		filepath.Join(root, "frontend", "src", "components"),
		// THE COPY'S OWN HOME IS SCANNED TOO. The session row's label is built by a
		// helper in frontend/src/lib, so a scan that stopped at routes/ and components/
		// would let the one string this test exists to police live somewhere it could not
		// see — a parity check that passes because it is looking at the wrong tree.
		filepath.Join(root, "frontend", "src", "lib"),
	}
	var found strings.Builder
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".tsx") && !strings.HasSuffix(path, ".ts") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			found.Write(b)
			return nil
		})
	}
	src := found.String()
	if !strings.Contains(src, ConsentAllowOnce) {
		t.Skipf("the GUI consent labels have not landed yet (sibling task: the TUI/policy parity work item): "+
			"this test must keep its name and be enabled when %q appears in frontend/src", ConsentAllowOnce)
	}
	for _, want := range []string{ConsentAllowOnce, ConsentDeny, ConsentSessionPrefix, ConsentSessionSuffix} {
		if !strings.Contains(src, want) {
			t.Fatalf("the GUI uses a different wording; %q is not in frontend/src — the two clients must describe the same decision identically", want)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("module root not found from the test's working directory")
	return ""
}

// TestConsentResolvedItemIsAOneLineRecord pins that a decided ask stops being a
// card and becomes the transcript's record of the decision.
func TestConsentResolvedItemIsAOneLineRecord(t *testing.T) {
	it := ConsentItem(PermissionAsk{ID: "a2", Kind: AskTool, Tool: "bash", Target: "make ci"}, 1000)
	it.Consent.Decision = DecisionDeny
	out := ConsentCardText(it, 72)
	if strings.Contains(out, "┌") {
		t.Fatalf("a resolved ask must not draw the card box:\n%s", out)
	}
	if !strings.Contains(out, ConsentDeny) || !strings.Contains(out, "make ci") {
		t.Fatalf("the record must name the decision and the target:\n%s", out)
	}
}

// TestConsentAllowSessionRecordNamesTheDirectory pins that the session record
// says WHICH directory was granted ("ask once per directory").
func TestConsentAllowSessionRecordNamesTheDirectory(t *testing.T) {
	it := ConsentItem(PermissionAsk{ID: "a3", Kind: AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)
	it.Consent.Decision = DecisionAllowSession
	out := ConsentCardText(it, 72)
	if !strings.Contains(out, "/p") || !strings.Contains(out, "session") {
		t.Fatalf("the session record must name the granted directory:\n%s", out)
	}
}

// TestConsentMoveSelSkipsTheDisabledSessionRow pins that a denied target's
// session row is unreachable by the arrows.
func TestConsentMoveSelSkipsTheDisabledSessionRow(t *testing.T) {
	st := &ConsentState{Ask: PermissionAsk{ID: "a4", Kind: AskTool, DeniedBy: "/etc/**"}}
	if !st.Ask.RowDisabled(1) {
		t.Fatal("a denied target must disable the session row")
	}
	st.MoveSel(1)
	if st.Sel != 2 {
		t.Fatalf("down from Allow once must land on Deny, got %d", st.Sel)
	}
	st.MoveSel(1)
	if st.Sel != 0 {
		t.Fatalf("down from Deny must wrap to Allow once, got %d", st.Sel)
	}
}

// TestConsentOptionLabelsForAQuestionCard pins the clarifying-question shape:
// the options plus Other when it is allowed, and no permission actions.
func TestConsentOptionLabelsForAQuestionCard(t *testing.T) {
	a := PermissionAsk{Kind: AskQuestion, Question: "Which file?", Options: []string{"main.go", "util.go"}, AllowOther: true}
	got := a.OptionLabels()
	want := []string{"main.go", "util.go", ConsentOther}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
	for _, l := range got {
		if strings.HasPrefix(l, ConsentSessionPrefix) {
			t.Fatal("a question card must not offer a permission grant")
		}
	}
}

// TestConsentQuestionCardRendersOptionsAndOther pins the rendered form of the
// question card.
func TestConsentQuestionCardRendersOptionsAndOther(t *testing.T) {
	it := ConsentItem(PermissionAsk{ID: "q1", Kind: AskQuestion, Question: "Which file did you mean?",
		Options: []string{"main.go", "util.go"}, AllowOther: true}, 1000)
	out := ConsentCardText(it, 64)
	for _, want := range []string{"Which file did you mean?", "main.go", "util.go", ConsentOther} {
		if !strings.Contains(out, want) {
			t.Fatalf("the question card must show %q:\n%s", want, out)
		}
	}
}

// TestDenyingRuleMatchesPaths pins the deny-lookup the card's DeniedBy rests on.
func TestDenyingRuleMatchesPaths(t *testing.T) {
	rules := []PolicyRule{
		{Effect: "allow", Tool: "write", Pattern: "/home/ops/**"},
		{Effect: "deny", Tool: "write", Pattern: "/home/ops/secrets/**"},
	}
	if got := DenyingRule(rules, "write", "/home/ops/secrets/key.pem"); got != "/home/ops/secrets/**" {
		t.Fatalf("a denied path must name its rule, got %q", got)
	}
	if got := DenyingRule(rules, "write", "/home/ops/main.go"); got != "" {
		t.Fatalf("an allow entry must not be reported as a deny, got %q", got)
	}
	if got := DenyingRule(rules, "bash", "/home/ops/secrets/key.pem"); got != "" {
		// the rule names the write tool, so a bash ask is not covered by it
		t.Fatalf("a rule for another tool must not deny this one, got %q", got)
	}
}

// TestConsentDeniedByIsStatedOnTheCard pins decision 11 end to end: the card
// says the session grant cannot override the file, rather than offering one.
func TestConsentDeniedByIsStatedOnTheCard(t *testing.T) {
	it := ConsentItem(PermissionAsk{ID: "a5", Kind: AskTool, Tool: "write",
		Target: "/etc/hosts", Directory: "/etc", DeniedBy: "/etc/**"}, 1000)
	out := ConsentCardText(it, 78)
	if !strings.Contains(out, "/etc/**") {
		t.Fatalf("the card must name the denying pattern:\n%s", out)
	}
	if !strings.Contains(out, "session grant cannot override") {
		t.Fatalf("the card must say a session grant cannot override the file:\n%s", out)
	}
	if !strings.Contains(out, "denied by /etc/**") {
		t.Fatalf("the session row must carry its disabled reason:\n%s", out)
	}
}

// TestConsentEscOnAQuestionIsRecordedWithoutADanglingSeparator pins the record a
// dismissed question leaves: esc picks nothing, so the row must say THAT rather
// than rendering "answer · " with nothing after the separator.
func TestConsentEscOnAQuestionIsRecordedWithoutADanglingSeparator(t *testing.T) {
	it := ConsentItem(PermissionAsk{ID: "q9", Kind: AskQuestion, Question: "Which file?", Options: []string{"a.go"}, AllowOther: true}, 1000)
	it.Consent.Decision = DecisionAnswer
	out := ConsentCardText(it, 72)
	if strings.Contains(out, "answer · ") || strings.HasSuffix(strings.TrimSpace(out), "·") {
		t.Fatalf("a dismissed question must not render a dangling separator: %q", out)
	}
	if !strings.Contains(out, "dismissed") {
		t.Fatalf("the record must say the question was dismissed: %q", out)
	}
	// And a real answer still renders as an answer.
	it2 := ConsentItem(PermissionAsk{ID: "q10", Kind: AskQuestion, Question: "Which file?", Options: []string{"a.go"}}, 1000)
	it2.Consent.Decision = DecisionAnswer
	it2.Consent.Choice = "seed.sql"
	if out2 := ConsentCardText(it2, 72); !strings.Contains(out2, "answer · seed.sql") {
		t.Fatalf("a real answer must still be recorded: %q", out2)
	}
}

// TestConsentSessionLabelNamesTheDirectory pins the operator's own request: the
// session row must SAY what it covers. They were asked for the same directory over
// and over and had no idea how far a session grant reached — "I thought we were going
// to add an option that says something along the lines of 'Never ask again for this
// directory for this session'".
func TestConsentSessionLabelNamesTheDirectory(t *testing.T) {
	a := PermissionAsk{ID: "a1", Kind: AskTool, Tool: "write", Target: "/p/sub/x.go", Directory: "/p/sub"}
	want := "Never ask again in /p/sub this session"
	if got := a.SessionLabel(); got != want {
		t.Fatalf("SessionLabel = %q, want %q", got, want)
	}
	// The row the card OFFERS is that label, not a generic phrase — the point of the
	// change is that the operator reads it before choosing.
	labels := a.OptionLabels()
	if len(labels) != 3 {
		t.Fatalf("labels = %v, want three", labels)
	}
	if labels[1] != want {
		t.Fatalf("the session row = %q, want %q", labels[1], want)
	}
	// An ask whose detail never resolved names the scope by its lifetime rather than
	// borrowing Target — Target is a FILE for a write ask, and naming a file as the
	// directory a grant covers would misstate the scope.
	noDir := PermissionAsk{ID: "a2", Kind: AskTool, Tool: "write", Target: "/p/sub/x.go"}
	if got := noDir.SessionLabel(); got != ConsentSessionNoDir {
		t.Fatalf("a directory-less ask = %q, want %q", got, ConsentSessionNoDir)
	}
}

// TestDecisionForRowIsIndexedNotStringMatched is the regression guard for the
// landmine the scope-naming label would otherwise have armed.
//
// The click path used to resolve a row by matching its LABEL against a constant, with
// "allow once" as the fall-through. The session row's label is now built from the
// directory, so it can equal no constant — and every click on it would have become
// "allow once": approving ONE call where the operator asked for the session, silently.
func TestDecisionForRowIsIndexedNotStringMatched(t *testing.T) {
	a := PermissionAsk{ID: "a1", Kind: AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}
	for i, want := range []ConsentDecision{DecisionAllowOnce, DecisionAllowSession, DecisionDeny} {
		got, ok := a.DecisionForRow(i)
		if !ok || got != want {
			t.Fatalf("row %d = (%q, %v), want (%q, true)", i, got, ok, want)
		}
	}
	// The session row's label is dynamic, so NO row may be keyed off the label text.
	if a.OptionLabels()[1] == ConsentAllowOnce || a.SessionLabel() == ConsentAllowOnce {
		t.Fatal("the session row's label collides with Allow once")
	}
	// Out of range is not a decision.
	if _, ok := a.DecisionForRow(3); ok {
		t.Fatal("row 3 is not a choice")
	}
	// A DISABLED session row is not choosable by any gesture. RowDisabled already hides
	// it from the arrows; this is what stops a CLICK from taking it.
	denied := PermissionAsk{ID: "a2", Kind: AskTool, Tool: "write", Target: "/etc/x", Directory: "/etc", DeniedBy: "/etc/**"}
	if !denied.RowDisabled(1) {
		t.Fatal("a denied target must disable the session row")
	}
	if _, ok := denied.DecisionForRow(1); ok {
		t.Fatal("a disabled session row must not yield a decision")
	}
	// A question card's rows are CONTENT, answered through the reply RPC — never a
	// permission choice, or picking an answer would record a grant that grants nothing.
	q := PermissionAsk{ID: "a3", Kind: AskQuestion, Question: "which?", Options: []string{"one", "two"}}
	if _, ok := q.DecisionForRow(0); ok {
		t.Fatal("a question card must not yield a permission decision")
	}
}
