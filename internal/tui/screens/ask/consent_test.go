package ask

import (
	"strings"
	"testing"
	"time"

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
	// grantLoaded mirrors the server FETCH having happened; the header distinguishes "not asked yet" from "no
	// grants", so a stub that only sets grantAvail would exercise a state production cannot reach.
	grantLoaded bool
	store       chat.PermissionStore
	storeOK     bool
	revoked     []string
	// repaints counts the RepaintTranscript calls the screen made. repaint() returns nil for a host that
	// does not implement the hook, so WITHOUT this a test cannot tell "the row asked for a repaint" from
	// "the row forgot to" — and forgetting is the difference between a paste that shows and one that
	// silently does nothing.
	repaints int
}

// RepaintTranscript mirrors the production host: the ask screen asks it to redraw the transcript, which is
// where the card (and its input row) is drawn.
func (s *stubHost) RepaintTranscript() tea.Cmd {
	s.repaints++
	return func() tea.Msg { return nil }
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

func (s *stubHost) ConsentGrantsLoaded(string) bool            { return s.grantLoaded }
func (s *stubHost) ConsentStore() (chat.PermissionStore, bool) { return s.store, s.storeOK }
func (s *stubHost) ConsentRevoke(_, dir string) error          { s.revoked = append(s.revoked, dir); return nil }

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
		ID: "a1", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)}
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
		ID: "a2", Kind: chat.AskTool, Tool: "bash", Target: "make ci", Directory: "/p"}, 1000)}
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
		ID: "a3", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)}
	m.RenderTranscript(items, chat.Conversation{}, false)
	// Row 0 is Allow once.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if h.dec != chat.DecisionAllowOnce {
		t.Fatalf("enter on the first row must allow once, got %q", h.dec)
	}

	m2, h2 := newTestModel(t)
	it2 := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a4", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)}
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
		ID: "a5", Kind: chat.AskTool, Tool: "write", Target: "/etc/hosts", Directory: "/etc", DeniedBy: "/etc/**"}, 1000)}
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

// TestConsentCtrlGRecordsNoDecision — the operator's report as a test:
//
//	"I hit ctrl+g to gain focus to the composer in the TUI so I could do a fullsend test and
//	 it registered it as a deny."
//
// This replaces TestConsentCtrlGDeniesRatherThanSilentlyReleasing, which asserted the OPPOSITE
// and is why the bug survived: the old rationale was that the claim is a latch, ctrl+g has to
// be able to leave it, and a recorded deny beats a silent dismissal ("decision 5"). Every part
// of that is true EXCEPT the conclusion. ctrl+g is the advertised way to type (it leads every
// page's hint line), it is pressed by operators who want to TYPE — to answer this card, or to
// reach /fullsend while it is up — and it is not an act of refusal. Recording deny made the
// transcript report a refusal the operator never judged, and the refusal reaches the MODEL,
// which then chooses a different approach on the strength of an answer nobody gave.
//
// So: no decision, the card stays pending, and the card can still be decided the normal ways.
func TestConsentCtrlGRecordsNoDecision(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a6", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)}
	m.RenderTranscript(items, chat.Conversation{}, false)

	m.DropKeyClaim()

	if h.dec != "" || h.resolved != 0 {
		t.Fatalf("ctrl+g must record NO decision, got dec=%q resolved=%d", h.dec, h.resolved)
	}
	if items[0].Consent == nil || !items[0].Consent.Pending() {
		t.Fatal("the card must still be PENDING after the focus chord — nothing was decided and nothing was dropped")
	}
	// THE CLAIM IS RELEASED, which is what makes the chord USEFUL: the composer is the DEFAULT
	// focus, so the claim is the only thing making the card own the keys — releasing it is how
	// the operator types (to answer, or to reach /fullsend while the card is up).
	//
	// Letting a focused composer outrank the claim instead (which I tried first) broke the card
	// outright, because the default focus already IS the composer: every claimed key went to the
	// composer and the card could not be answered at all. Releasing the claim for THIS ask is the
	// narrow version, and the shell test proves typing works after the chord.
	if m.ClaimsKeys() {
		t.Fatal("the claim must be released so the composer can take the keys")
	}
	// AND A NEW ASK RE-ARMS IT BY CONSTRUCTION — deferring by ID rather than a bool is what
	// makes that true with no flag to forget to clear.
	items2 := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a7", Kind: chat.AskTool, Tool: "write", Target: "/p/y.go", Directory: "/p"}, 2000)}
	m.RenderTranscript(items2, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("a NEW ask must claim the keys again — a deferred card must not disarm the next one")
	}
	// AND THE CARD IS STILL DECIDABLE — esc still denies, deliberately and explicitly.
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.dec != chat.DecisionDeny {
		t.Fatalf("esc must still deny explicilty, got %q", h.dec)
	}
}

// TestConsentReconcileReleasesAStaleCard pins decision 7: an ask that the
// transcript no longer carries (turn ended / superseded / aborted) drops the
// card and its claim, so the composer becomes typable again.
func TestConsentReconcileReleasesAStaleCard(t *testing.T) {
	m, _ := newTestModel(t)
	m.RenderTranscript([]chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a7", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go"}, 1000)}, chat.Conversation{}, false)
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
		Options: []string{"main.go", "util.go"}, AllowOther: true}, 1000)}
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
		Options: []string{"main.go", "util.go"}, AllowOther: true}, 1000)}
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
	// The rows are the SERVER's fields: a directory and when it was granted. It used to also carry a tool and a
	// count accrued locally from the cards this client answered — a second record of grants the plane owns, and
	// the reason a revoke from this list revoked nothing.
	now := time.Now().Unix()
	h.grants = []chat.SessionGrant{{Directory: "/p", GrantedAt: now}, {Directory: "/q", GrantedAt: now}}
	h.grantAvail = true
	h.grantLoaded = true
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
	h.grantLoaded = true
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

// TestConsentScreenOwnsTabWhileClaiming pins the OTHER half of the key-ownership
// hook, against the shell's real tab chord.
//
// THE CLAIM ALONE DOES NOT TAKE TAB: the shell handles the tab chord ABOVE the
// ClaimsKeys gate (router.go, the focus-chord switch), so `ClaimsKeys() == true`
// left tab falling through to tabRingNext — press one moved the keyboard to the
// tab bar, press two rotated the active screen out from under a still-pending
// card (and, because the gate reads m.screens[m.active], the card then lost the
// keyboard entirely). Tab IS a card key: handleConsentKey binds tab/shift+tab to
// row movement. OwnsTab is the shell's own hook for exactly this (see
// execution.Model.OwnsTab).
func TestConsentScreenOwnsTabWhileClaiming(t *testing.T) {
	m, _ := newTestModel(t)
	if m.OwnsTab() {
		t.Fatal("with nothing claimed, tab belongs to the shell")
	}
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "a9", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p"}, 1000)}
	m.RenderTranscript(items, chat.Conversation{}, false)
	if !m.OwnsTab() {
		t.Fatal("a pending card must own tab — the card binds it as row movement")
	}
	// The list overlays claim the keyboard too, so tab must not rotate the ring
	// out of a modal the operator is reading.
	m.openPermissions()
	if !m.OwnsTab() {
		t.Fatal("an open overlay must own tab")
	}
	m.ov = nil
	// Tab now MOVES THE CARD (the behaviour the hook exists to protect).
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if items[0].Consent.Sel != 1 {
		t.Fatalf("tab must move the card's selection, sel=%d", items[0].Consent.Sel)
	}
	// Once resolved, tab is the shell's again.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.OwnsTab() {
		t.Fatal("tab must return to the shell once the card resolves")
	}
}

// TestAskOverlaysAreVisibleInTheViewsRealFrame is the SURFACE half of criteria 4
// and 5, and it is the assertion the overlay tests were missing: the list
// surfaces must actually REACH THE SCREEN.
//
// They did not. Base.View() already fills the region the shell sizes the screen
// to (app.go baseView normalizes the screen's render to exactly that many rows),
// so the overlay APPENDED after the pane landed past the last row and was dropped
// — `/grants` and `/permissions` opened a list that rendered NOTHING. Asserting
// the overlay's own View (what the other tests do) could not catch that; this
// asserts the screen's real frame, which is what the operator sees.
func TestAskOverlaysAreVisibleInTheViewsRealFrame(t *testing.T) {
	m, h := newTestModel(t)
	h.grantAvail = true
	h.grantLoaded = true
	h.grants = []chat.SessionGrant{
		{Directory: "/home/ops/project", GrantedAt: time.Now().Unix()},
		{Directory: "/srv", GrantedAt: time.Now().Add(-90 * time.Minute).Unix()},
	}
	h.store = &stubStore{rules: []chat.PolicyRule{{Effect: "deny", Tool: "write", Pattern: "/etc/**"}}}
	h.storeOK = true

	m.openGrants()
	v := m.View()
	for _, want := range []string{"Session grants", "/home/ops/project", "enter revokes"} {
		if !strings.Contains(v, want) {
			t.Fatalf("/grants must render %q in the screen's frame, got:\n%s", want, v)
		}
	}
	if got := strings.Count(strings.TrimRight(v, "\n"), "\n") + 1; got != m.h {
		t.Fatalf("the frame must stay %d rows, got %d", m.h, got)
	}

	m.ov = nil
	m.openPermissions()
	v = m.View()
	for _, want := range []string{"Permissions", "deny", "/etc/**", "the file is the source of truth"} {
		if !strings.Contains(v, want) {
			t.Fatalf("/permissions must render %q in the screen's frame, got:\n%s", want, v)
		}
	}
	if got := strings.Count(strings.TrimRight(v, "\n"), "\n") + 1; got != m.h {
		t.Fatalf("the frame must stay %d rows, got %d", m.h, got)
	}
}

// TestAskListOverlayShowsEveryRowNotOne pins the row budget: kit2.Table windows
// its body to Height-3 with a floor of ONE row, and the overlays never set Height,
// so `/permissions` and `/grants` drew a single row no matter how much room the
// pane had ("1-1/2" beside one of two rules).
func TestAskListOverlayShowsEveryRowNotOne(t *testing.T) {
	m, h := newTestModel(t)
	h.grantAvail = true
	h.grantLoaded = true
	h.grants = []chat.SessionGrant{
		{Directory: "/home/ops/project", GrantedAt: time.Now().Unix()},
		{Directory: "/srv", GrantedAt: time.Now().Unix()},
	}
	m.openGrants()
	v := m.View()
	for _, want := range []string{"/home/ops/project", "/srv"} {
		if !strings.Contains(v, want) {
			t.Fatalf("every grant must be on screen, %q missing:\n%s", want, v)
		}
	}
	if strings.Contains(v, "1-1/2") {
		t.Fatalf("the list must not be windowed to one row:\n%s", v)
	}
}

// The grants roll-up must not claim the operator has allowed NOTHING before it has asked. "No grants" and "not
// asked yet" look identical in an empty list, and the header reported the first for both — a claim about the
// operator's permissions that nothing supported, at the exact moment the conversation might be allowed a great
// deal. The same distinction is why a FAILED fetch says so instead of showing an empty list.
func TestGrantsHeaderDoesNotGuessBeforeTheFetch(t *testing.T) {
	m, h := newTestModel(t)

	h.grantAvail = true
	h.grantLoaded = false
	if got := m.grantsFieldValue(); strings.Contains(got, "none") {
		t.Fatalf("the header claimed the operator has allowed nothing before asking: %q", got)
	} else if !strings.Contains(got, "checking") || !strings.Contains(got, "/grants") {
		t.Fatalf("the header must say it is still checking and point at /grants, got %q", got)
	}

	// Asked, and genuinely empty: now "none" is the truth and the header may say it.
	h.grantLoaded = true
	if got := m.grantsFieldValue(); !strings.Contains(got, "none") {
		t.Fatalf("an empty server list must read as none, got %q", got)
	}

	// Asked, and the plane could not answer: neither "none" nor a count.
	h.grantAvail = false
	if got := m.grantsFieldValue(); !strings.Contains(got, "unavailable") {
		t.Fatalf("a failed fetch must say the plane could not answer, got %q", got)
	}
}
