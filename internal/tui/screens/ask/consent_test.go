package ask

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// stubHost is the shell surface the card needs, recorded rather than performed.
type stubHost struct {
	dec        chat.ConsentDecision
	choice     string
	resolved   int
	sent       []string
	grants     []chat.SessionGrant
	grantAvail bool
	store      chat.PermissionStore
	storeOK    bool
	revoked    []string
}

func (s *stubHost) ConsentResolve(_ string, dec chat.ConsentDecision, choice string) tea.Cmd {
	s.dec = dec
	s.choice = choice
	s.resolved++
	return nil
}

func (s *stubHost) ConsentSend(text string) tea.Cmd {
	s.sent = append(s.sent, text)
	return nil
}

func (s *stubHost) ConsentGrants(string) ([]chat.SessionGrant, bool) { return s.grants, s.grantAvail }
func (s *stubHost) ConsentStore() (chat.PermissionStore, bool)       { return s.store, s.storeOK }
func (s *stubHost) ConsentRevoke(_, dir string) error                { s.revoked = append(s.revoked, dir); return nil }

type stubStore struct {
	rules    []chat.PolicyRule
	upserted []chat.PolicyRule
	deleted  []string
}

func (s *stubStore) Rules() ([]chat.PolicyRule, error) { return s.rules, nil }
func (s *stubStore) UpsertRule(r chat.PolicyRule) error {
	s.upserted = append(s.upserted, r)
	s.rules = append(s.rules, r)
	return nil
}
func (s *stubStore) DeleteRule(effect, tool, pattern string) error {
	s.deleted = append(s.deleted, effect+"|"+tool+"|"+pattern)
	return nil
}
func (s *stubStore) SessionGrants(string) ([]chat.SessionGrant, error) { return nil, nil }
func (s *stubStore) RevokeGrant(string, string) error                  { return nil }

func newTestModel(t *testing.T) (*Model, *stubHost) {
	t.Helper()
	m := New(&client.Clients{}, subs.NewRegistry())
	m.SetSize(120, 40)
	h := &stubHost{}
	m.SetShell(h)
	return m, h
}

// TestConsentCardIsAdoptedAndClaimsKeys pins that a pending ask in the
// transcript (a) becomes the screen's card and (b) makes the screen own the
// keyboard.
func TestConsentCardIsAdoptedAndClaimsKeys(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a1", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("a pending card must claim the keys")
	}
	if got := m.ConsentPendingForTest(); got != "a1" {
		t.Fatalf("pending ask = %q, want a1", got)
	}
	if m.FormOpen() {
		t.Fatal("a card is not a form — FormOpen must stay false so Tab is not captured as a field move")
	}
}

// TestConsentEscapeDeniesAndReleasesTheClaim pins the recorded rule: Escape
// DENIES (it does not silently dismiss), and the claim is released so the turn
// stays usable.
func TestConsentEscapeDeniesAndReleasesTheClaim(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a2", Kind: chat.AskTool, Tool: "bash", Target: "make ci", Directory: "/p"})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.resolved != 1 || h.dec != chat.DecisionDeny {
		t.Fatalf("esc must deny, got resolved=%d dec=%q", h.resolved, h.dec)
	}
	if m.ClaimsKeys() {
		t.Fatal("the claim must be released when the card resolves")
	}
	if !items[0].Consent.Pending() == false {
		t.Fatal("the item must be marked resolved so the transcript records the refusal")
	}
}

// TestConsentEnterCommitsTheHighlightedAction pins the three actions by arrow
// key + Enter.
func TestConsentEnterCommitsTheHighlightedAction(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a3", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	// Row 0 is Allow once.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h.dec != chat.DecisionAllowOnce {
		t.Fatalf("enter on the first row must allow once, got %q", h.dec)
	}

	m2, h2 := newTestModel(t)
	it2 := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a4", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"})}
	m2.RenderTranscript(it2, chat.Conversation{}, false)
	m2.Update(tea.KeyMsg{Type: tea.KeyDown})
	m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h2.dec != chat.DecisionAllowSession {
		t.Fatalf("enter on the second row must allow for the session, got %q", h2.dec)
	}
}

// TestConsentDisabledSessionRowCannotBeChosen pins that a denied target's
// session row is skipped by the arrows AND that Enter can never commit it.
func TestConsentDisabledSessionRowCannotBeChosen(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a5", Kind: chat.AskTool, Tool: "write", Target: "/etc/hosts", Directory: "/etc", DeniedBy: "/etc/**"})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if items[0].Consent.Sel != 2 {
		t.Fatalf("down must skip the disabled session row, landed on %d", items[0].Consent.Sel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h.dec == chat.DecisionAllowSession {
		t.Fatal("a session grant must never be sent for a file-denied target")
	}
}

// TestConsentCtrlGDeniesRatherThanSilentlyReleasing pins decision 5: the focus
// chord must always be able to leave the latched claim, and leaving it is a
// RECORDED deny rather than a silent dismissal.
func TestConsentCtrlGDeniesRatherThanSilentlyReleasing(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a6", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.DropKeyClaim()
	if h.dec != chat.DecisionDeny || h.resolved != 1 {
		t.Fatalf("ctrl+g while pending must record a deny, got dec=%q resolved=%d", h.dec, h.resolved)
	}
	if m.ClaimsKeys() {
		t.Fatal("the claim must be released after the focus chord")
	}
}

// TestConsentReconcileReleasesAStaleCard pins decision 7: an ask that the
// transcript no longer carries (turn ended / superseded / aborted) drops the
// card and its claim, so the composer becomes typable again.
func TestConsentReconcileReleasesAStaleCard(t *testing.T) {
	m, _ := newTestModel(t)
	m.RenderTranscript([]chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a7", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go"})}, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("precondition: the card must be pending")
	}
	m.RenderTranscript(nil, chat.Conversation{}, false)
	if m.ClaimsKeys() {
		t.Fatal("a card whose ask has left the transcript must release the claim")
	}
}

// TestQuestionCardOtherSendsTheTypedText pins the clarifying-question card: the
// free-text Other row is typed into and its text is submitted as the choice.
func TestQuestionCardOtherSendsTheTypedText(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "q1", Kind: chat.AskQuestion, Question: "Which file?",
		Options: []string{"main.go", "util.go"}, AllowOther: true})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	// options: main.go(0), util.go(1), Other(2)
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !items[0].Consent.OtherMode {
		t.Fatal("enter on Other must open the free-text row")
	}
	for _, r := range "seed.sql" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h.dec != chat.DecisionAnswer || h.choice != "seed.sql" {
		t.Fatalf("the typed answer must be submitted, got dec=%q choice=%q", h.dec, h.choice)
	}
}

// TestQuestionCardOptionSendsTheChoice pins that selecting a canned option is
// sent as the choice (the shell then sends it as the next user message).
func TestQuestionCardOptionSendsTheChoice(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "q2", Kind: chat.AskQuestion, Question: "Which file?",
		Options: []string{"main.go", "util.go"}, AllowOther: true})}
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h.dec != chat.DecisionAnswer || h.choice != "main.go" {
		t.Fatalf("the option must be submitted as the answer, got dec=%q choice=%q", h.dec, h.choice)
	}
}

// TestGrantsRollUpIsVisible pins criterion 4's visibility half: the granted
// directories are countable in the header and listable, and revocable.
func TestGrantsRollUpIsVisible(t *testing.T) {
	m, h := newTestModel(t)
	h.grants = []chat.SessionGrant{{Directory: "/p", Tool: "write", Count: 3}, {Directory: "/q", Tool: "bash", Count: 1}}
	h.grantAvail = true
	if got := m.grantsFieldValue(); !strings.Contains(got, "2") || !strings.Contains(got, "/grants") {
		t.Fatalf("the grants field must count them and point at /grants, got %q", got)
	}
	m.openGrants()
	if m.ov == nil || m.ov.tbl == nil {
		t.Fatal("/grants must open a list")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open overlay must claim the keys")
	}
	if rows := m.ov.tbl.Rows; len(rows) != 2 {
		t.Fatalf("the grant list must show both grants, got %d rows", len(rows))
	}
	// Enter revokes the selected directory.
	m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(h.revoked) != 1 {
		t.Fatalf("enter must revoke the selected grant, got %v", h.revoked)
	}
	m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ov != nil {
		t.Fatal("esc must close the overlay")
	}
}

// TestGrantsRollUpSaysUnavailableRatherThanFabricating pins decision 9's honest
// fallback.
func TestGrantsRollUpSaysUnavailableRatherThanFabricating(t *testing.T) {
	m, h := newTestModel(t)
	h.grantAvail = false
	if got := m.grantsFieldValue(); !strings.Contains(got, "unavailable") {
		t.Fatalf("an unanswerable plane must be stated, got %q", got)
	}
}

// TestPermissionsListIsListableRemovableAndAddable pins criterion 5's TUI half:
// the persistent list is shown, a row is removable, and a rule is addable — all
// through the store (the file), never a local copy.
func TestPermissionsListIsListableRemovableAndAddable(t *testing.T) {
	m, h := newTestModel(t)
	st := &stubStore{rules: []chat.PolicyRule{{Effect: "deny", Tool: "write", Pattern: "/etc/**"}}}
	h.store = st
	h.storeOK = true
	m.openPermissions()
	if m.ov == nil || len(m.ov.tbl.Rows) != 1 {
		t.Fatal("the list must be loaded from the store")
	}
	// Enter removes the selected rule.
	m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(st.deleted) != 1 || st.deleted[0] != "deny|write|/etc/**" {
		t.Fatalf("enter must delete the selected rule through the store, got %v", st.deleted)
	}
	// `a` opens the add form; ctrl+s writes through the store.
	m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.ov == nil || m.ov.kind != ovPolicyAdd || m.ov.form == nil {
		t.Fatal("a must open the add form")
	}
	if !m.FormOpen() {
		t.Fatal("the add form must report FormOpen")
	}
	m.ov.form.Values["effect"] = "allow"
	m.ov.form.Values["tool"] = "write"
	m.ov.form.Values["pattern"] = "/tmp/**"
	m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	if len(st.upserted) != 1 || st.upserted[0].Pattern != "/tmp/**" {
		t.Fatalf("ctrl+s must upsert through the store, got %+v", st.upserted)
	}
}

// TestPermissionsSaysUnavailableWithoutAStore pins the honest fallback for the
// persistent list too (the sibling owns storage; the TUI must not invent one).
func TestPermissionsSaysUnavailableWithoutAStore(t *testing.T) {
	m, _ := newTestModel(t)
	m.openPermissions()
	if m.ov == nil || !strings.Contains(m.ov.err, "unavailable") {
		t.Fatalf("no store must read as unavailable, got %q", m.ov.err)
	}
}
