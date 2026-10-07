package chat

// rail_paging_test.go — the rail must be able to SEE every conversation.
//
// The defect this pins: LoadConversations asked for one page of 100 and threw away
// `next_page_token`, so the rail was a fixed "newest 100" window over an unlimited table.
// On a tenant holding 184 conversations (132 of them newer test rows) the operator's own
// history fell past rank 100 and the rail could never ask for it again — which presents as
// "all my conversations disappeared", because nothing on screen says a filter is in play.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// pagingAsk serves a conversation list in pages, exactly as the server does: a full page
// carries the token of the next, and the last page carries none.
type pagingAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	total    int
	requests int
	tokens   []string // the token each request carried ("" for the first)
	failAt   int      // 1-based request number to fail on; 0 = never
}

func (s *pagingAsk) ListConversations(_ context.Context, req *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	s.requests++
	s.tokens = append(s.tokens, req.Msg.GetPageToken())
	if s.failAt != 0 && s.requests == s.failAt {
		return nil, errors.New("plane fell over")
	}
	start := 0
	if tok := req.Msg.GetPageToken(); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("stub: bad token %q", tok)
		}
		start = n
	}
	// The server CLAMPS anything over 100 down to its own default, so a client asking for
	// more than 100 gets a HALF page and a token — the stub models that, because a rail that
	// asked for 500 would otherwise look correct here and page twice as often in production.
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 100 {
		size = 50
	}
	end := start + size
	if end > s.total {
		end = s.total
	}
	out := &apiv1.ListConversationsResponse{
		Categories: []*apiv1.Category{{Id: "cat-1", Name: "Triaged"}},
	}
	for i := start; i < end; i++ {
		out.Conversations = append(out.Conversations, &apiv1.Conversation{
			Id:    fmt.Sprintf("c%03d", i),
			Title: fmt.Sprintf("conversation %d", i),
		})
	}
	if end < s.total {
		out.NextPageToken = strconv.Itoa(end)
	}
	return connect.NewResponse(out), nil
}

// The rail reaches EVERY conversation, not just the first page.
func TestLoadConversationsPagesToExhaustion(t *testing.T) {
	h := &pagingAsk{total: 250}
	cl, _ := newTestServer(t, h)
	c := NewController(cl)

	msg, ok := c.LoadConversations()().(ConversationsMsg)
	if !ok {
		t.Fatal("LoadConversations did not return a ConversationsMsg")
	}
	if msg.Err != "" {
		t.Fatalf("LoadConversations errored: %s", msg.Err)
	}
	if len(msg.Convs) != 250 {
		t.Fatalf("rail holds %d conversations, want all 250 — a truncated rail is the defect (rank>100 unreachable)", len(msg.Convs))
	}
	// The order is the server's (newest first) and paging must not disturb it: page 2 has to
	// continue where page 1 stopped, not restart or interleave.
	for i, c := range msg.Convs {
		if want := fmt.Sprintf("c%03d", i); c.ID != want {
			t.Fatalf("row %d is %s, want %s — pages are out of order", i, c.ID, want)
		}
	}
	if h.requests != 3 {
		t.Errorf("requests = %d, want 3 (100 + 100 + 50) — the loop must follow the token until it is empty", h.requests)
	}
	if h.tokens[0] != "" || h.tokens[1] != "100" || h.tokens[2] != "200" {
		t.Errorf("page tokens = %v, want [\"\" 100 200]", h.tokens)
	}
	// The grouping set rides the FIRST page and must survive the whole loop.
	if len(msg.Categories) != 1 || msg.Categories[0].GetId() != "cat-1" {
		t.Errorf("categories = %v, want the first page's set carried through", msg.Categories)
	}
}

// A single page that reports no token stops the loop: a tenant with fewer conversations than
// one page must not pay a second request, and an empty tenant must not loop.
func TestLoadConversationsStopsOnAShortPage(t *testing.T) {
	h := &pagingAsk{total: 7}
	cl, _ := newTestServer(t, h)
	c := NewController(cl)

	msg := c.LoadConversations()().(ConversationsMsg)
	if len(msg.Convs) != 7 {
		t.Fatalf("rail holds %d conversations, want 7", len(msg.Convs))
	}
	if h.requests != 1 {
		t.Errorf("requests = %d, want 1 — a page with no token ends the load", h.requests)
	}
}

// A failure mid-paging is reported, not silently truncated to the pages that succeeded. A
// half-list that looks complete is worse than the rail's explicit error + retry.
func TestLoadConversationsReportsAFailureMidPaging(t *testing.T) {
	h := &pagingAsk{total: 250, failAt: 2}
	cl, _ := newTestServer(t, h)
	c := NewController(cl)

	msg := c.LoadConversations()().(ConversationsMsg)
	if msg.Err == "" {
		t.Fatal("a failed page was swallowed; the rail would show a truncated list as if it were complete")
	}
	if len(msg.Convs) != 0 {
		t.Errorf("convs = %d, want 0 — a partial list must not be presented as the rail", len(msg.Convs))
	}
}
