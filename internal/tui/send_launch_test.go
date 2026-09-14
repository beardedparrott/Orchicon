package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// fakeAskPlane counts the conversation creates the shell issues.
type fakeAskPlane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	created int
}

func (f *fakeAskPlane) CreateConversation(_ context.Context, _ *connect.Request[apiv1.CreateConversationRequest]) (*connect.Response[apiv1.CreateConversationResponse], error) {
	f.created++
	return connect.NewResponse(&apiv1.CreateConversationResponse{
		Conversation: &apiv1.Conversation{Id: "new-conv", Title: "t"},
	}), nil
}

// sendApp builds an App on the Ask launch page wired to a counting plane.
func sendApp(t *testing.T) (*App, *fakeAskPlane) {
	t.Helper()
	f := &fakeAskPlane{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewApp(client.New(client.Options{BaseURL: srv.URL}),
		&config.Profile{Name: "default", URL: srv.URL}, "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(*App)
	m.SwitchTo(TabAsk)
	m.setFocus(focusComposer)
	m.chatConvID = ""
	return m, f
}

// typeInto feeds runes one per key message, KEEPING each Update result.
//
// The result must be kept: App.Update has a value receiver, so a discarded
// return leaves the App as it was and only partially-mutated shared state (the
// textarea's internal slices) survives — which silently scrambles the buffer.
// That is how typing "test" once produced "tset" in a throwaway harness.
func typeInto(t *testing.T, m *App, s string) *App {
	t.Helper()
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(*App)
	}
	return m
}

// runCmd executes a command, expanding a tea.Batch the way bubbletea does, and
// feeds every resulting message back in.
func runCmd(t *testing.T, m *App, cmd tea.Cmd) *App {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			if c == nil {
				continue
			}
			if sub := c(); sub != nil {
				nm, _ := m.Update(sub)
				m = nm.(*App)
			}
		}
	case nil:
	default:
		nm, _ := m.Update(msg)
		m = nm.(*App)
	}
	return m
}

// Enter on the launch page must CREATE a conversation and send the message.
//
// This is the path the operator exercises first, so it gets a real regression
// test: the composer accepts the text, Enter produces the send command, and that
// command reaches CreateConversation. (The multi-step async chain after that —
// transcript load, live stream, rail reconcile — is covered by their own tests.)
func TestLaunchPageEnterCreatesTheConversation(t *testing.T) {
	m, plane := sendApp(t)

	m = typeInto(t, m, "test")
	if got := m.dock.Value(); got != "test" {
		t.Fatalf("composer buffer = %q, want the typed text (a value-receiver Update whose result is dropped scrambles this)", got)
	}

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	if cmd == nil {
		t.Fatal("enter produced NO command — the send path is dead")
	}
	m = runCmd(t, m, cmd)

	if plane.created != 1 {
		t.Fatalf("CreateConversation called %d times, want exactly 1", plane.created)
	}
	if m.chatConvID != "new-conv" {
		t.Fatalf("chatConvID = %q, want the created conversation", m.chatConvID)
	}
	// The operator's message is echoed locally the moment the conversation exists,
	// so the transcript is never briefly empty.
	items := m.chatStore.snapshot("new-conv")
	if len(items) == 0 || items[0].Kind != "user" {
		t.Fatalf("store = %+v, want the operator's message echoed first", items)
	}
	if items[0].Text != "test" {
		t.Fatalf("echoed text = %q, want %q", items[0].Text, "test")
	}
	// And the launch page is left behind.
	if m.welcomeMode() {
		t.Error("still on the launch page after sending — the view must switch to the conversation")
	}
}

// An empty composer must not create anything on Enter (it is a no-op, not a
// blank conversation).
func TestEnterOnAnEmptyComposerDoesNotCreate(t *testing.T) {
	m, plane := sendApp(t)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	m = runCmd(t, m, cmd)
	if plane.created != 0 {
		t.Fatalf("an empty Enter created %d conversations, want 0", plane.created)
	}
}

// The same send must work when the terminal spells Enter as LF.
//
// bubbletea reports CR (0x0D) as KeyEnter and LF (0x0A) as KeyCtrlJ — different
// KeyTypes. Some terminals and PTY configurations send LF, and with only the
// KeyEnter branch those terminals pressed Enter and got NOTHING at all. This
// drives the full shell path (composer -> dispatch -> create) with an LF Enter.
func TestLaunchPageLFEnterCreatesTheConversation(t *testing.T) {
	if tea.KeyEnter == tea.KeyCtrlJ {
		t.Fatal("fixture: KeyEnter and KeyCtrlJ are the same KeyType — vacuous test")
	}
	m, plane := sendApp(t)
	m = typeInto(t, m, "test")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ}) // LF
	m = nm.(*App)
	if cmd == nil {
		t.Fatal("LF-Enter produced NO command — an LF-sending terminal cannot send at all")
	}
	m = runCmd(t, m, cmd)

	if plane.created != 1 {
		t.Fatalf("CreateConversation called %d times via LF-Enter, want exactly 1", plane.created)
	}
	if m.chatConvID != "new-conv" {
		t.Fatalf("chatConvID = %q, want the created conversation", m.chatConvID)
	}
}
