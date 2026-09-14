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
	title, fields := m.RenderTranscript(nil)
	if !strings.Contains(title, "Conversation") || len(fields) == 0 {
		t.Fatalf("pre-meta RenderTranscript = (%q, %v), want a readable fallback", title, fields)
	}

	m.metaTitle = "Conversation: hello"
	m.metaFields = []screenkit.Field{
		{Key: "id", Value: "c1"},
		{Key: "model", Value: "orchicon/deepseek/deepseek-flash"},
	}
	title, fields = m.RenderTranscript([]chat.ChatItem{{Kind: chat.KindText, Text: "hi"}})
	if title != "Conversation: hello" {
		t.Errorf("title = %q, want the cached one", title)
	}
	if len(fields) != 2 || fields[1].Key != "model" {
		t.Errorf("fields = %v, want the cached rich header", fields)
	}
}
