package tui

// execution_session_test.go — the durable-transcript fetch must read the TAIL of an execution, not
// its opening.
//
// The operator: "I don't think it is showing the entire execution. In fact, I don't think any of the
// executions in the TUI are showing the entire execution."
//
// The fetch asked for Limit: 200 with NO before_seq, which the server answers with the FIRST 200
// parts in sequence order (db.ListExecutionSessionParts: `ORDER BY seq` when before_seq is unset).
// The execution the report named has 17,574 parts, so the pane showed its opening 200 and nothing
// else — no late tool calls, no final answer, no follow-up reply. The GUI asks for the tail instead
// (beforeSeq = max int64, limit 10000, then reverses to chronological), and so does this now.
//
// These tests assert the REQUEST, because that is where the defect was: the response handling was
// never wrong, it was simply being handed the wrong end of the transcript.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// stubSession records the GetExecutionSession request and answers with a newest-first tail, as the
// real server does when before_seq is set.
type stubSession struct {
	apiv1connect.UnimplementedExecutionServiceHandler

	gotBeforeSeq int64
	gotLimit     int32
	calls        int
	// parts are returned in the order the SERVER would send for a DESC tail query: newest first.
	parts []*apiv1.ExecutionSessionPart
}

func (s *stubSession) GetExecutionSession(_ context.Context, req *connect.Request[apiv1.GetExecutionSessionRequest]) (*connect.Response[apiv1.GetExecutionSessionResponse], error) {
	s.calls++
	s.gotBeforeSeq = req.Msg.GetBeforeSeq()
	s.gotLimit = req.Msg.GetLimit()
	return connect.NewResponse(&apiv1.GetExecutionSessionResponse{Parts: s.parts}), nil
}

func newSessionApp(t *testing.T) (*App, *stubSession) {
	t.Helper()
	stub := &stubSession{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewExecutionServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	return m, stub
}

// The request asks for the TAIL: a before_seq that means "from the end", and a limit generous
// enough to hold a long execution's tail.
func TestExecutionSessionFetchesTheTailNotTheHead(t *testing.T) {
	m, stub := newSessionApp(t)

	cmd := m.OpenExecutionSession("exec-1")
	if cmd == nil {
		t.Fatal("OpenExecutionSession produced no command")
	}
	cmd()

	if stub.gotBeforeSeq == 0 {
		t.Fatal("before_seq was not set — the server treats that as \"from the START\", which is the truncated transcript the operator reported")
	}
	// Max int64: every part is below it, so the DESC query starts at the newest.
	if stub.gotBeforeSeq != sessionTailFromSeq {
		t.Errorf("before_seq = %d, want the end sentinel %d", stub.gotBeforeSeq, sessionTailFromSeq)
	}
	if stub.gotLimit != sessionPartLimit {
		t.Errorf("limit = %d, want %d", stub.gotLimit, sessionPartLimit)
	}
	// The two clients must agree about how much of a transcript is "the transcript".
	if sessionPartLimit < 1000 {
		t.Errorf("limit = %d — a real execution has far more parts than this (the reported one has 17,574)", sessionPartLimit)
	}
}

// The tail comes back newest-first and is put back into reading order, so the transcript reads
// top-to-bottom instead of backwards.
func TestExecutionSessionReversesTheTailIntoChronologicalOrder(t *testing.T) {
	m, stub := newSessionApp(t)
	stub.parts = []*apiv1.ExecutionSessionPart{
		{Seq: 30, Kind: "text", Payload: []byte(`{"text":"third"}`)},
		{Seq: 20, Kind: "text", Payload: []byte(`{"text":"second"}`)},
		{Seq: 10, Kind: "text", Payload: []byte(`{"text":"first"}`)},
	}

	msg := m.OpenExecutionSession("exec-1")()
	sm, ok := msg.(execSessionMsg)
	if !ok {
		t.Fatalf("got %T, want execSessionMsg", msg)
	}
	if sm.err != nil {
		t.Fatalf("fetch: %v", sm.err)
	}
	seqs := make([]int64, 0, len(sm.parts))
	for _, p := range sm.parts {
		seqs = append(seqs, p.GetSeq())
	}
	want := []int64{10, 20, 30}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("part order = %v, want chronological %v", seqs, want)
		}
	}
}

// An empty execution id is not a request, and an empty session is not an error.
func TestExecutionSessionHandlesEmptyInputs(t *testing.T) {
	m, stub := newSessionApp(t)
	if cmd := m.OpenExecutionSession(""); cmd != nil {
		t.Error("an empty execution id produced a command")
	}
	if stub.calls != 0 {
		t.Errorf("an empty execution id made %d request(s)", stub.calls)
	}

	// No parts: a successful, empty message — the pane shows an empty transcript, not a failure.
	stub.parts = nil
	msg := m.OpenExecutionSession("exec-1")()
	sm := msg.(execSessionMsg)
	if sm.err != nil || len(sm.parts) != 0 {
		t.Errorf("empty transcript = (%v, %d parts), want no error and no parts", sm.err, len(sm.parts))
	}
}
