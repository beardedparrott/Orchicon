package chat

// consent_unscopable_test.go — an ask NO session grant could cover must not offer one.
//
// The server sends no directory for such an ask: its blocking target IS a volume root (`ls /`), or the
// action resolved no path at all. A grant for an empty directory is refused by the store, so the row used
// to promise a scope the system could not keep — the operator would have clicked it, been asked again, and
// had no way to see why. The row is now DISABLED and the card says which reason applies.

import (
	"strings"
	"testing"
)

func unscopableAsk() PermissionAsk {
	return PermissionAsk{
		ID: "a-root", Kind: AskTool, Tool: "bash",
		Target:  "ls /",
		Summary: "run a shell command: ls /",
		// No Directory: the server found no scope a grant could name.
	}
}

// TestAnAskWithNoGrantableScopeDisablesTheSessionRow is the fix: the row is shown DISABLED with the reason,
// so it cannot be clicked into a decision that does nothing.
func TestAnAskWithNoGrantableScopeDisablesTheSessionRow(t *testing.T) {
	ask := unscopableAsk()
	if !ask.RowDisabled(1) {
		t.Fatal("the session row was OFFERED for an ask with no directory a grant could cover — a grant would silence nothing")
	}
	if _, ok := ask.DecisionForRow(1); ok {
		t.Fatal("a disabled session row must not resolve to a decision, by click or by key")
	}
	// The other two rows are untouched: allowing ONE call and denying must both still work.
	if _, ok := ask.DecisionForRow(0); !ok {
		t.Fatal("allow-once must stay available")
	}
	if _, ok := ask.DecisionForRow(2); !ok {
		t.Fatal("deny must stay available")
	}

	text := ConsentCardText(ConsentItem(ask, 1000), 130)
	if !strings.Contains(text, ConsentSessionNoDir) {
		t.Fatalf("the card must show the session row as unavailable:\n%s", text)
	}
	if !strings.Contains(text, "no directory here for a session grant to cover") {
		t.Fatalf("the card must say WHY the row cannot be taken — a disabled row with no reason reads as a broken control:\n%s", text)
	}
	if !strings.Contains(text, "(lenient)") && !strings.Contains(text, "allow it once") {
		// The notice tells the operator what they CAN do instead.
		t.Logf("notice wording: %s", text)
	}
}

// The arrow keys must skip it, so a disabled row can never be reached and confirmed by accident.
func TestTheUnscopableSessionRowIsUnreachableByKeys(t *testing.T) {
	st := &ConsentState{Ask: unscopableAsk()}
	st.MoveSel(1) // down from "Allow once"
	if st.Sel != 2 {
		t.Fatalf("MoveSel landed on row %d, want the deny row (2) — a disabled row must be skipped", st.Sel)
	}
	if st.SelectedDisabled() {
		t.Fatal("the cursor came to rest on a disabled row")
	}
	st.MoveSel(1) // wraps back round
	if st.Sel != 0 {
		t.Fatalf("MoveSel wrapped to %d, want the allow-once row (0)", st.Sel)
	}
}

// The CONTROL: an ask WITH a directory still offers the session row, so a fix that disabled it everywhere
// cannot pass.
func TestAnAskWithAScopeStillOffersTheSessionRow(t *testing.T) {
	ask := PermissionAsk{
		ID: "a-tmp", Kind: AskTool, Tool: "bash",
		Target: "cd /tmp && ls", Summary: "run a shell command: cd /tmp && ls",
		Directory: "/tmp",
	}
	if ask.RowDisabled(1) {
		t.Fatal("a session row with a real directory must stay available")
	}
	if dec, ok := ask.DecisionForRow(1); !ok || dec != DecisionAllowSession {
		t.Fatalf("row 1 must resolve to allow-session, got %q ok=%v", dec, ok)
	}
	if label := ask.SessionLabel(); !strings.Contains(label, "/tmp") {
		t.Fatalf("the session row must name its scope, got %q", label)
	}
	text := ConsentCardText(ConsentItem(ask, 1000), 130)
	if strings.Contains(text, "no directory here for a session grant to cover") {
		t.Fatalf("a scoped ask must not claim it cannot be silenced:\n%s", text)
	}
}

// The deny-list reason keeps its own wording — the two reasons are different facts and the card states the
// one that applies.
func TestTheDenyListReasonIsStillStated(t *testing.T) {
	ask := PermissionAsk{
		ID: "a-denied", Kind: AskTool, Tool: "write", Target: "/etc/hosts",
		Directory: "/etc", DeniedBy: "/etc/**",
	}
	if !ask.RowDisabled(1) {
		t.Fatal("a deny-listed target must not offer a session grant")
	}
	text := ConsentCardText(ConsentItem(ask, 1000), 130)
	if !strings.Contains(text, "denied by /etc/**") {
		t.Fatalf("the deny rule must be named:\n%s", text)
	}
	if strings.Contains(text, "no directory here for a session grant to cover") {
		t.Fatalf("a deny-listed ask must give the DENY reason, not the no-scope one:\n%s", text)
	}
}
