package ask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// fakeAsk answers just enough of the Ask service for the detail round trip.
type fakeAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	msgs []*apiv1.ChatMessage
}

func (f *fakeAsk) GetConversation(_ context.Context, req *connect.Request[apiv1.GetConversationRequest]) (*connect.Response[apiv1.GetConversationResponse], error) {
	return connect.NewResponse(&apiv1.GetConversationResponse{
		Conversation: &apiv1.Conversation{
			Id:       req.Msg.GetId(),
			Title:    "hello",
			ModelRef: "orchicon/deepseek/deepseek-flash",
		},
	}), nil
}

// ListMessages is implemented ON PURPOSE. Left unimplemented, the old
// body-building code would have hit an error and produced "" anyway — so the test
// below would pass even with the duplicate renderer still present. Serving the rows
// makes it a real negative control: re-adding the body makes the test fail.
func (f *fakeAsk) ListMessages(_ context.Context, _ *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	return connect.NewResponse(&apiv1.ListMessagesResponse{Messages: f.msgs}), nil
}

func newAskModelWithPlane(t *testing.T, f *fakeAsk) *Model {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry())
}

// The transcript BODY has exactly ONE renderer: the shell.
//
// This screen used to build a second, competing copy from ListMessages — and
// because the server returns that page NEWEST-FIRST (db.ListMessages orders
// created_at DESC), that copy was INVERTED: the model's reply printed ABOVE the
// operator's message, and a long reply pushed that message out of the visible
// area, which is why the operator kept reporting "my initial user message is
// still not showing up". The sibling conversationItems had been fixed for exactly
// this ordering; this path had not. The duplicate is deleted rather than fixed, so
// the pane can only ever be painted by the live, scroll-preserving renderer.
func TestConversationDetailLeavesTheBodyToTheShell(t *testing.T) {
	// The fake still OFFERS messages — the point is that this screen no longer
	// asks for them.
	f := &fakeAsk{msgs: []*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant", Content: "the answer is 42"},
		{Id: "m1", Role: "user", Content: "hello"},
	}}
	m := newAskModelWithPlane(t, f)

	title, fields, body, err := m.detail(context.Background(), "conversations", "c1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if body != "" {
		t.Errorf("the screen rendered its own transcript body (%q) — the shell owns it, and a second copy is what inverted the order:\n%s", body, body)
	}
	if !strings.Contains(title, "hello") {
		t.Errorf("title = %q, want the conversation title", title)
	}
	// The RICH header survives (the shell repaints using it).
	keys := map[string]string{}
	for _, fl := range fields {
		keys[fl.Key] = fl.Value
	}
	for _, want := range []string{"id", "title", "model", "messages", "mode"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("header lost the %q field: %v", want, fields)
		}
	}
}

// RenderTranscript serves that cached header, so the shell's repaint does not
// downgrade the pane to a two-field summary.
func TestRenderTranscriptServesTheCachedHeader(t *testing.T) {
	m := New(nil, nil)

	// Before the meta lands: honest, not blank.
	title, fields := m.RenderTranscript(nil, chat.Conversation{}, false)
	if !strings.Contains(title, "Conversation") || len(fields) == 0 {
		t.Fatalf("pre-meta RenderTranscript = (%q, %v), want a readable fallback", title, fields)
	}

	m.metaTitle = "Conversation: hello"
	m.metaFields = []screenkit.Field{
		{Key: "id", Value: "c1"},
		{Key: "model", Value: "orchicon/deepseek/deepseek-flash"},
	}
	title, fields = m.RenderTranscript([]chat.ChatItem{{Kind: chat.KindText, Text: "hi"}}, chat.Conversation{}, false)
	if title != "Conversation: hello" {
		t.Errorf("title = %q, want the cached one", title)
	}
	if len(fields) != 2 || fields[1].Key != "model" {
		t.Errorf("fields = %v, want the cached rich header", fields)
	}
}

// The cached header must not go STALE.
//
// detail() caches what its one-shot GetConversation returned at the instant the
// pane opened. For a brand-new conversation that is before the title is assigned
// and before the first message is written, and nothing re-reads it — so the pane
// reported "title —" and "messages 0" for a conversation that plainly had both,
// while the conversations rail showed the real title right beside it. The shell's
// live list row (reloaded after every send) must win.
func TestRenderTranscriptOverlaysTheLiveConversation(t *testing.T) {
	m := New(nil, nil)
	// The stale cache: what a too-early GetConversation returns.
	m.metaTitle = "Conversation: older"
	m.metaFields = []screenkit.Field{
		{Key: "id", Value: "c1"},
		{Key: "title", Value: "older"},
		{Key: "messages", Value: "0"},
		{Key: "model", Value: "orchicon/deepseek/deepseek-flash"},
	}

	title, fields := m.RenderTranscript(nil, chat.Conversation{ID: "c1", Title: "test", MessageN: 2}, true)
	if title != "Conversation: test" {
		t.Errorf("title = %q, want the live title", title)
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if got["messages"] != "2" {
		t.Errorf("messages = %q, want the live count 2 (the stale cache said 0)", got["messages"])
	}
	if got["title"] != "test" {
		t.Errorf("title field = %q, want the live title", got["title"])
	}
	// Fields the live row does NOT carry survive the overlay.
	if got["model"] != "orchicon/deepseek/deepseek-flash" {
		t.Errorf("model = %q, want the cached model preserved", got["model"])
	}

	// A live row with no title yet must leave the cached title alone rather than
	// blanking the pane.
	if t2, _ := m.RenderTranscript(nil, chat.Conversation{ID: "c1", MessageN: 3}, true); t2 != "Conversation: older" {
		t.Errorf("title = %q, want the cached title when the live row has none", t2)
	}

	// With no live row, the cached header is served untouched.
	if _, f := m.RenderTranscript(nil, chat.Conversation{}, false); len(f) != 4 {
		t.Errorf("fields = %v, want the 4 cached fields when there is no live row", f)
	}
}
