// Package activitye2e is the SHARED disposable plane for the activity-line end-to-end
// capstone (work item end-to-end-proof-the-activity-line-through-the-real-tui-frame-and-gui-pane).
//
// WHY A FIXTURE PLANE AT ALL. A genuinely model-driven turn cannot be driven in this container:
// exec.LookPath("opencode") is empty and the serve-host fallback probe misses too, so
// internal/opencode/chatsession.go fails every ChatStream with "host opencode serve unavailable".
// The repo already sanctions exactly this outcome — TestE2EOpenCodeLiveWorkerCallsTheProjectServer
// skips and records ModelAccess "unavailable" — so this harness substitutes ONE layer (the
// provider behind ChatStream) and serves EVERY REAL layer above it: the real Connect/HTTP
// transport, the real internal/tui/chat controller state machine, the real App.onChatWake -> pane
// -> frame pipeline in a real pty, and the real React route/DOM in a real browser.
//
// ONE PLANE, TWO CLIENTS. The TUI takes any plane URL (writeOrchConfig) and the SPA proxies
// /orchicon.api.v1 to 127.0.0.1:8080 (frontend/vite.config.ts), so a single process here serves
// the SAME conversation, the SAME ledger and the SAME server clock to both clients — which is
// what makes the cross-client claim (the same word + the same counts) a literal observation
// rather than an inference.
//
// THE LEDGER IS THE ONE SOURCE OF TRUTH. The scripted turn appends its tool calls to p.calls and
// serves them on the SAME ListMessages page the production client polls (chat/pageToolCalls ->
// App.onTranscript -> chatStore), each row carrying a real issued_at_unix_ms. So the client's
// counter and the server's own SummarizeCalls over p.calls are two independent reads of one
// ledger — the reconciliation the feature's honesty check demands.
package activitye2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/toolclass"
)

const (
	// ConvID is the one conversation both clients open.
	ConvID = "conv-activity-e2e"
	// ProjID is the project the conversation belongs to (so the launch prompt stays quiet).
	ProjID = "proj-activity-e2e"
	// Title is the rail/sidebar label both clients show.
	Title = "activity-e2e"
	// AssistantMsgID is the acked reply id the turn is persisted under.
	AssistantMsgID = "a1"
	// Prompt is the message the TUI types. It asks for work of MIXED CLASSES — a read, a grep, a
	// bash and a modify — which is what makes every bucket of the counter legible in one turn.
	Prompt = "E2EPROMPT inspect files and run the tests"

	// StampOnPeriod is the heartbeat's server_time_unix_ms, chosen EXACTLY on a
	// chat.VerbPeriodMS (4000 ms) boundary so that a client sampling within the next four seconds
	// derives the SAME word from it (VerbAt(stamp + delta) == VerbAt(stamp) for delta < 4000 ms).
	// That is what lets the cross-client comparison be EXACT rather than "some entry of the list":
	// both clients draw VerbAt(StampOnPeriod), and the harness asserts the equality rather than
	// accepting any rotation member.
	StampOnPeriod int64 = 1_700_000_000_000

	// HeartbeatCadence is how often the live stream re-sends the heartbeat. It is well inside
	// the 25 s warn band, so a HEALTHY turn never escalates — which is what makes the escalation
	// leg an observation of silence rather than of the band being mis-set.
	HeartbeatCadence = time.Second
)

// Phase is the harness control surface: the state the plane is serving.
type Phase string

const (
	// PhaseFlight is a healthy turn: heartbeat on cadence, tool calls landing, content streaming.
	PhaseFlight Phase = "flight"
	// PhaseStarted is a live turn that has streamed its first content but counted NOTHING YET, and
	// holds there until the harness flips it to PhaseFlight. It exists so "the turn just started and
	// the line carries no counter" is a STEADY state the harness can stand in, rather than a ~4s
	// window it has to win a race against.
	PhaseStarted Phase = "started"
	// PhaseNoTools is a live turn that issues ZERO tool calls (the "no counters, exactly as
	// before the change" state).
	PhaseNoTools Phase = "no-tools"
	// PhaseStalled is a turn whose stream has gone SILENT: no heartbeat, no chunks. The client's
	// own watchdog must escalate it to the 25 s / 35 s bands with the counters gone.
	PhaseStalled Phase = "stalled"
	// PhaseDown is a plane that answers every RPC with an error — the unreachable case.
	PhaseDown Phase = "down"
)

// scriptCalls is the fixture turn's work list: a read, a grep, a bash and a modify, issued one at a
// time. MIXED on purpose — the counter's three buckets are all legible in ONE turn, which is what
// makes the painted string a real test of the summarizer's fixed ordering and its per-class counts
// rather than of a single bucket.
var scriptCalls = []string{"read", "batch_grep", "bash", "write"}

// Plane is the fixture Ask service plus its control surface.
type Plane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	mu       sync.Mutex
	phase    Phase
	stamp    int64
	running  bool
	prompt   string
	streamed string
	calls    []toolclass.Call
	toolRows []*v1.ToolCall
	// done is closed by EndTurn: the scripted turn's heartbeat loop returns, the stream closes,
	// and the client's line must clear (the "turn ends" observation).
	done    chan struct{}
	endOnce sync.Once
	// scripted guards the ONE-TURN-ONE-SCRIPT rule: only the first stream to attach to a turn runs
	// the work. A re-dial (WatchTurnStream) must RE-ATTACH, not re-run the model — the real server's
	// collector owns the turn and replays it to a late watcher, which is why the client may
	// legitimately hold two streams for one turn without the ledger growing twice.
	scripted bool
	// gen is a GENERATION COUNTER, bumped by Reset. A stream captures it at attach and re-checks it
	// before every call it makes. Without it a stream that attached BEFORE a Reset would resume its
	// script AFTER the reset cleared `scripted` (the sleep between calls spans the reset), and the
	// ledger would grow to TWICE the script: two interleaved runs, one turn. That is a harness
	// artefact, never a client bug, and it must be impossible rather than unlikely — this is what
	// makes the count reconciliation (AC4) a statement about ONE scripted turn.
	gen int
	// settleBeforeCalls is how long the turn streams CONTENT before it issues its first tool call,
	// so "turn just started, no counters yet" is an observable state rather than a race.
	settleBeforeCalls time.Duration
	// callGap is how long the turn waits BETWEEN calls. It is deliberately LONGER than the client's
	// 1s transcript poll, so each intermediate count ("1 read", then "2 reads") is a state the
	// operator — and this harness — can actually stand in, rather than a burst that only ever
	// renders its final shape.
	callGap time.Duration
	// stalledAt is when the plane entered PhaseStalled: it fixes turn_last_activity_at in the
	// past so the GUI's own silence age grows from a real stamp rather than from a guess.
	stalledAt time.Time
}

// New returns a plane in the healthy flight phase.
func New() *Plane {
	return &Plane{
		phase:             PhaseFlight,
		stamp:             StampOnPeriod,
		prompt:            Prompt,
		done:              make(chan struct{}),
		settleBeforeCalls: 4 * time.Second,
		callGap:           2500 * time.Millisecond,
	}
}

// Phase reports the current control phase.
func (p *Plane) Phase() Phase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.phase
}

// SetPhase moves the plane to a new phase. Entering PhaseStalled fixes the staleness stamp.
func (p *Plane) SetPhase(ph Phase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ph == PhaseStalled && p.phase != PhaseStalled {
		p.stalledAt = time.Now()
	}
	if ph != PhaseStalled {
		p.stalledAt = time.Time{}
	}
	p.phase = ph
}

// Stamp reports the server clock the heartbeat carries.
func (p *Plane) Stamp() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stamp
}

// SetStamp sets the server clock the next heartbeat will carry.
func (p *Plane) SetStamp(t int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stamp = t
}

// Calls returns the ledger rows the client is counting — the SAME slice ListMessages serves.
func (p *Plane) Calls() []toolclass.Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]toolclass.Call(nil), p.calls...)
}

// SummarizeNow renders the server's OWN read of the ledger over the shared rolling window. It is
// the independent half of the count reconciliation: the client's counter must equal this.
func (p *Plane) SummarizeNow() string {
	return toolclass.SummarizeCalls(p.Calls(), time.Now(), toolclass.DefaultWindow)
}

// Prompt returns the operator message the transcript carries.
func (p *Plane) Prompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompt
}

// Streamed returns the assistant text the turn has produced so far — the "content has arrived"
// half of the regression this feature fixes.
func (p *Plane) Streamed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamed
}

func (p *Plane) appendStreamed(s string) {
	p.mu.Lock()
	p.streamed += s
	p.mu.Unlock()
}

// trace appends a diagnostic line to $ORCH_ACTIVITY_E2E_TRACE (a file path) when set. It exists so
// a harness run can be inspected after the fact without racing the process's stderr.
func trace(format string, args ...any) {
	path := os.Getenv("ORCH_ACTIVITY_E2E_TRACE")
	if path == "" || path == "1" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

// issueCall records one scripted tool call: the ledger row (for the counter) and its wire shape
// (for the transcript page), both stamped NOW so the rolling window really contains the work.
// The client sees the SAME row the server counts — there is no second source of truth.
func (p *Plane) issueCall(name string) {
	at := time.Now().UnixMilli()
	p.mu.Lock()
	defer p.mu.Unlock()
	if os.Getenv("ORCH_ACTIVITY_E2E_TRACE") != "" {
		trace("issueCall %s (now %d rows)", name, len(p.calls)+1)
	}
	p.calls = append(p.calls, toolclass.Call{ToolName: name, AtMs: at})
	p.toolRows = append(p.toolRows, &v1.ToolCall{
		Id:             fmt.Sprintf("tc-%d", len(p.toolRows)+1),
		Type:           "function",
		FunctionName:   name,
		Arguments:      "{}",
		IssuedAtUnixMs: at,
	})
}

// ---------------------------------------------------------------------------
// The Ask service
// ---------------------------------------------------------------------------

func (p *Plane) down() error {
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("activitye2e: plane down (harness control)"))
}

// conversation is the rail/sidebar row. turn_in_flight + turn_last_activity_at are the two fields
// the GUI's turnInFlight / silence age read, and the TUI's turnInFlight reads the former.
func (p *Plane) conversation() *v1.Conversation {
	p.mu.Lock()
	defer p.mu.Unlock()
	row := &v1.Conversation{
		Id:                        ConvID,
		Title:                     Title,
		MessageCount:              2,
		ProjectId:                 ProjID,
		TurnInFlight:              p.running && p.phase != PhaseDown,
		PendingAssistantMessageId: AssistantMsgID,
	}
	if p.phase == PhaseStalled {
		// STALE ON PURPOSE: the turn is still "in flight" (so the line may draw) but nothing has
		// arrived for a while — exactly the state the watchdog bands exist to name.
		row.TurnProgressing = false
		row.TurnLastActivityAt = timestamppb.New(p.stalledAt)
	} else {
		row.TurnProgressing = true
		row.TurnLastActivityAt = timestamppb.New(time.Now())
	}
	return row
}

func (p *Plane) ListConversations(context.Context, *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	if p.Phase() == PhaseDown {
		return nil, p.down()
	}
	return connect.NewResponse(&v1.ListConversationsResponse{
		Conversations: []*v1.Conversation{p.conversation()},
	}), nil
}

func (p *Plane) GetConversation(_ context.Context, req *connect.Request[v1.GetConversationRequest]) (*connect.Response[v1.GetConversationResponse], error) {
	if p.Phase() == PhaseDown {
		return nil, p.down()
	}
	row := p.conversation()
	if id := req.Msg.GetId(); id != "" {
		row.Id = id
	}
	return connect.NewResponse(&v1.GetConversationResponse{Conversation: row}), nil
}

// ListMessages serves the durable transcript NEWEST-FIRST, which is the contract the server's
// db.ListMessages holds and both clients reverse (chat.conversationItems, askOrchicon.ts). The
// assistant row carries BOTH the streamed text and the turn's accumulated tool rows, which is the
// shape a real persisted turn has.
func (p *Plane) ListMessages(_ context.Context, req *connect.Request[v1.ListMessagesRequest]) (*connect.Response[v1.ListMessagesResponse], error) {
	if p.Phase() == PhaseDown {
		return nil, p.down()
	}
	p.mu.Lock()
	base := time.Now().Add(-2 * time.Second)
	user := &v1.ChatMessage{
		Id:             "u1",
		ConversationId: ConvID,
		Role:           "user",
		Content:        p.prompt,
		CreatedAt:      timestamppb.New(base),
	}
	assistant := &v1.ChatMessage{
		Id:             AssistantMsgID,
		ConversationId: ConvID,
		Role:           "assistant",
		Content:        p.streamed,
		ToolCalls:      append([]*v1.ToolCall(nil), p.toolRows...),
		CreatedAt:      timestamppb.New(base.Add(time.Second)),
	}
	p.mu.Unlock()
	return connect.NewResponse(&v1.ListMessagesResponse{
		Messages: []*v1.ChatMessage{assistant, user},
	}), nil
}

// ChatStream is the send path: it acks the turn and then runs the scripted turn on this stream.
func (p *Plane) ChatStream(ctx context.Context, _ *connect.Request[v1.ChatStreamRequest], st *connect.ServerStream[v1.ChatStreamResponse]) error {
	if p.Phase() == PhaseDown {
		return p.down()
	}
	return p.streamTurn(ctx, st)
}

// WatchTurnStream is the re-dial arm: the SAME scripted turn, delivered to a watcher that attached
// mid-turn. Both arms must feed the client's stamp for the rotation to keep drawing.
func (p *Plane) WatchTurnStream(ctx context.Context, _ *connect.Request[v1.WatchTurnStreamRequest], st *connect.ServerStream[v1.ChatStreamResponse]) error {
	if p.Phase() == PhaseDown {
		return p.down()
	}
	return p.streamTurn(ctx, st)
}

// streamTurn is the scripted turn, shared by both stream arms.
//
// THE ORDER IS THE OBSERVATION. Content arrives FIRST, then the counters land one at a time, then
// more content — so a frame sampled after the counters land has BOTH a partially rendered reply
// AND a live activity line. That coincidence is the regression this feature exists to fix (before
// it, the line vanished the instant content arrived).
func (p *Plane) streamTurn(ctx context.Context, st *connect.ServerStream[v1.ChatStreamResponse]) error {
	p.mu.Lock()
	p.running = true
	runWork := !p.scripted
	if runWork {
		p.scripted = true
	}
	// myGen is the generation this stream belongs to. Reset bumps p.gen, so a stream that attached
	// before a reset recognises itself as stale at every call it is about to make.
	myGen := p.gen
	p.mu.Unlock()
	trace("streamTurn attach runWork=%v gen=%d", runWork, myGen)
	// stale reports whether a Reset has superseded this stream. It is checked immediately before
	// every ledger write, so a stale stream can NEVER grow the ledger of the turn that replaced it.
	stale := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.gen != myGen
	}

	send := func(r *v1.ChatStreamResponse) error { return st.Send(r) }
	if err := send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_TurnStarted{
		TurnStarted: &v1.TurnStarted{AssistantMessageId: AssistantMsgID}}}); err != nil {
		return nil
	}
	if err := send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_Heartbeat{
		Heartbeat: &v1.Heartbeat{ServerTimeUnixMs: p.Stamp()}}}); err != nil {
		return nil
	}

	// CONTENT FIRST. The reply is partially rendered from here on, which is the state in which the
	// old line had already disappeared.
	first := "E2EWITNESSCONTENT Reading the repo layout and the files this change touches."
	if p.Phase() == PhaseNoTools {
		first = "E2EWITNESSCONTENT Answering without touching any files or running anything."
	}
	p.appendStreamed(first)
	if err := send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_TextChunk{
		TextChunk: &v1.TextChunk{Content: first}}}); err != nil {
		return nil
	}

	// THEN THE WORK, one call at a time: the counters appear and GROW. The settle beat before the
	// first call is what makes "the turn just started and has counted nothing" a state the harness
	// can actually stand in.
	if p.Phase() != PhaseNoTools && runWork {
		// THE SETTLE BEAT, and in PhaseStarted it is a STANDING GATE: the harness can observe the
		// bare line for as long as it likes, then flip the phase and watch the counters land.
		select {
		case <-ctx.Done():
			return nil
		case <-p.done:
			return nil
		case <-time.After(p.settleBeforeCalls):
		}
		for p.Phase() == PhaseStarted {
			select {
			case <-ctx.Done():
				return nil
			case <-p.done:
				return nil
			case <-time.After(200 * time.Millisecond):
			}
		}
		for _, name := range scriptCalls {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(p.callGap):
			}
			if stale() {
				trace("streamTurn gen=%d SUPERSEDED before issuing %s", myGen, name)
				return nil
			}
			p.issueCall(name)
		}
	} else if runWork {
		// THE ZERO-TOOL-CALL TURN still takes real time and still streams a closing line, so the
		// transcript it leaves has the SAME shape as the with-tools one. That is what makes the
		// row-budget comparison (AC6) a comparison of the counter's cost and nothing else.
		select {
		case <-ctx.Done():
			return nil
		case <-p.done:
			return nil
		case <-time.After(p.settleBeforeCalls):
		}
	}
	if runWork {
		final := " E2EWITNESSFINAL Now running the focused tests."
		p.appendStreamed(final)
		if err := send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_TextChunk{
			TextChunk: &v1.TextChunk{Content: final}}}); err != nil {
			return nil
		}
	}

	// THE HEARTBEAT LOOP. It keeps the healthy turn healthy (a live stream can never reach the
	// 25s band) and, when the harness flips the phase to stalled, STOPS — which is how a real
	// silence is produced for the escalation legs rather than simulated in the client.
	t := time.NewTicker(HeartbeatCadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.done:
			// A CLEAN END: the done signal, then the stream closes. A real turn's completion is what
			// makes the client clear its line and hand the transcript body its row back.
			_ = send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_Done{
				Done: &v1.DoneSignal{}}})
			return nil
		case <-t.C:
			if p.Phase() == PhaseStalled {
				continue
			}
			if p.Phase() == PhaseDown {
				// THE PLANE WENT UNREACHABLE. A real server that dies mid-turn does not politely go
				// quiet on an open stream — the stream terminates, and the client's OWN failure path
				// (route `fail()` -> reconnecting) is what tells the operator. Returning the error is
				// that termination: the browser's async iterator THROWS, which is the only way the GUI
				// reaches its "connection interrupted" banner. (A bare CloseClientConnections is not
				// enough THROUGH THE DEV PROXY: it severs plane<->vite while leaving browser<->vite open,
				// so the client sees silence, not a drop.)
				return p.down()
			}
			if err := send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_Heartbeat{
				Heartbeat: &v1.Heartbeat{ServerTimeUnixMs: p.Stamp()}}}); err != nil {
				return nil
			}
		}
	}
}

// Reissue appends a FRESH burst of calls to the running turn, stamped at the current instant. It is
// what the harness calls to put counters back on the row after a long stall leg has aged the earlier
// calls out of the rolling window — an honest "more work landed on this turn", not a rewind.
func (p *Plane) Reissue() {
	p.issueCall("read")
	p.issueCall("bash")
}

// Reset returns the plane to a clean slate: no ledger rows, no streamed text, no turn. It is what
// lets the SAME plane serve the with-tools leg and the zero-tool-call leg without the first leg's
// ledger leaking into the second (a counter the second leg never earned would be exactly the false
// claim this feature's honesty check exists to catch).
func (p *Plane) Reset(ph Phase) {
	p.mu.Lock()
	p.gen++
	trace("RESET to %s gen=%d (dropping %d rows)", ph, p.gen, len(p.calls))
	p.calls = nil
	p.toolRows = nil
	p.streamed = ""
	p.running = false
	p.scripted = false
	p.phase = ph
	p.stalledAt = time.Time{}
	p.mu.Unlock()
}

// EndTurn marks the turn over: turn_in_flight false, the stream closes on its next beat, so a
// client's line must clear and the transcript body get its row back.
func (p *Plane) EndTurn() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	p.endOnce.Do(func() { close(p.done) })
}

// ---------------------------------------------------------------------------
// Supporting services (so the shell reaches a full, quiet TUI)
// ---------------------------------------------------------------------------

// Projects answers the project list with the folder the harness runs in, so the launch-time
// project prompt stays quiet (the same reason the diff gate's fixture does it).
type Projects struct {
	apiv1connect.UnimplementedProjectServiceHandler
	Dir string
}

func (s *Projects) ListProjects(context.Context, *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	return connect.NewResponse(&v1.ListProjectsResponse{Projects: []*v1.Project{
		{Id: ProjID, Name: Title, ProjectDir: s.Dir},
	}}), nil
}

func (s *Projects) GetProject(_ context.Context, req *connect.Request[v1.GetProjectRequest]) (*connect.Response[v1.GetProjectResponse], error) {
	return connect.NewResponse(&v1.GetProjectResponse{Project: &v1.Project{
		Id: req.Msg.GetId(), Name: Title, ProjectDir: s.Dir,
	}}), nil
}

// Identities answers the tenant resolution probe.
type Identities struct {
	apiv1connect.UnimplementedAuthServiceHandler
}

func (Identities) ListIdentities(context.Context, *connect.Request[v1.ListIdentitiesRequest]) (*connect.Response[v1.ListIdentitiesResponse], error) {
	return connect.NewResponse(&v1.ListIdentitiesResponse{Identities: []*v1.Identity{
		{Id: "id-e2e", TenantId: "tnt-e2e", DisplayName: "e2e"},
	}}), nil
}

// Edits serves an EMPTY file-edit ledger (this feature has no diff pane in play; the service exists
// so the shell's fetch path has a real answer rather than a transport error).
type Edits struct {
	apiv1connect.UnimplementedFileEditServiceHandler
}

func (Edits) GetSessionFileEdits(context.Context, *connect.Request[v1.GetSessionFileEditsRequest]) (*connect.Response[v1.GetSessionFileEditsResponse], error) {
	return connect.NewResponse(&v1.GetSessionFileEditsResponse{}), nil
}

// Sessions answers the GUI's HTTP auth routes (the SPA resolves its session over plain HTTP, not
// Connect — see frontend/src/auth/session.ts: local-login, refresh and /auth/session).
type Sessions struct {
	mu sync.Mutex
	// down makes every auth route fail, so the harness can produce an unauthenticated page too.
	down bool
}

// SetDown flips the auth routes between answering and failing.
func (s *Sessions) SetDown(v bool) {
	s.mu.Lock()
	s.down = v
	s.mu.Unlock()
}

// IsDown reports whether the auth routes are failing.
func (s *Sessions) IsDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

// JSON writes v as a one-line JSON response with the given status.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Mux builds the whole fixture plane: the Connect services (path/handler pairs from the generated
// constructors, so the wire path is the server's own), the SPA's HTTP auth routes, and the harness
// control surface.
//
// IT IS ALSO THE IN-PROCESS HANDLER THE GO TEST SERVES. The real-pty gate starts this behind an
// httptest server on 127.0.0.1:8080 and the Playwright spec talks to the standalone command — one
// implementation, so the two clients cannot be served by two drifting planes.
func Mux(plane *Plane, sessions *Sessions, dir string) http.Handler {
	mux := http.NewServeMux()

	ap, ah := apiv1connect.NewAskOrchiconServiceHandler(plane)
	mux.Handle(ap, ah)
	pp, ph := apiv1connect.NewProjectServiceHandler(&Projects{Dir: dir})
	mux.Handle(pp, ph)
	ip, ih := apiv1connect.NewAuthServiceHandler(Identities{})
	mux.Handle(ip, ih)
	ep, eh := apiv1connect.NewFileEditServiceHandler(Edits{})
	mux.Handle(ep, eh)

	// The SPA's session routes are PLAIN HTTP, not Connect (frontend/src/auth/session.ts). The
	// keys below are exactly the ones that file reads (SessionInfo, the local-login body).
	sessionBody := map[string]any{
		"authenticated": true,
		"identity_id":   "id-e2e",
		"tenant_id":     "tnt-e2e",
		"username":      "e2e",
		"is_admin":      false,
	}
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, _ *http.Request) {
		if sessions.IsDown() {
			http.Error(w, "plane down", http.StatusServiceUnavailable)
			return
		}
		JSON(w, http.StatusOK, sessionBody)
	})
	tokenBody := map[string]any{
		"access_token": "e2e-access-token",
		"identity_id":  "id-e2e",
		"tenant_id":    "tnt-e2e",
		"expires_in":   3600,
	}
	login := func(w http.ResponseWriter, _ *http.Request) {
		if sessions.IsDown() {
			http.Error(w, "plane down", http.StatusServiceUnavailable)
			return
		}
		// The HttpOnly refresh cookie the real server sets; the SPA exchanges it for a token on
		// a full page load, which is how the browser keeps a live session across a reload.
		http.SetCookie(w, &http.Cookie{Name: "orchicon_refresh", Value: "e2e-refresh", Path: "/", HttpOnly: true})
		JSON(w, http.StatusOK, tokenBody)
	}
	mux.HandleFunc("/auth/local-login", login)
	mux.HandleFunc("/auth/refresh", login)
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		JSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// The harness control surface — the browser half drives phases through these.
	mux.HandleFunc("/__e2e/phase", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("p") {
		case "flight":
			plane.SetPhase(PhaseFlight)
		case "started":
			plane.SetPhase(PhaseStarted)
		case "no-tools":
			plane.SetPhase(PhaseNoTools)
		case "stalled":
			plane.SetPhase(PhaseStalled)
		case "down":
			plane.SetPhase(PhaseDown)
			sessions.SetDown(true)
		case "rpc-down":
			// RPCs fail but the SPA's OWN session routes keep answering, so the page stays mounted.
			// This is how the harness "stops the plane mid-turn" (the work item's own AC5 wording)
			// while the banner remains observable: the open stream and the re-dial both fail, the
			// page does not go blank.
			plane.SetPhase(PhaseDown)
		default:
			http.Error(w, "unknown phase", http.StatusBadRequest)
			return
		}
		JSON(w, http.StatusOK, map[string]any{"ok": true, "phase": plane.Phase()})
	})
	mux.HandleFunc("/__e2e/stamp", func(w http.ResponseWriter, r *http.Request) {
		var v int64
		if _, err := fmt.Sscanf(r.URL.Query().Get("v"), "%d", &v); err != nil {
			http.Error(w, "bad v", http.StatusBadRequest)
			return
		}
		plane.SetStamp(v)
		JSON(w, http.StatusOK, map[string]any{"ok": true, "stamp": v})
	})
	mux.HandleFunc("/__e2e/end", func(w http.ResponseWriter, _ *http.Request) {
		plane.EndTurn()
		JSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/__e2e/reset", func(w http.ResponseWriter, r *http.Request) {
		ph := Phase(r.URL.Query().Get("p"))
		switch ph {
		case PhaseFlight, PhaseStarted, PhaseNoTools, PhaseStalled:
		default:
			ph = PhaseFlight
		}
		plane.Reset(ph)
		JSON(w, http.StatusOK, map[string]any{"ok": true, "phase": string(ph)})
	})
	mux.HandleFunc("/__e2e/state", func(w http.ResponseWriter, _ *http.Request) {
		type callJSON struct {
			ToolName string `json:"tool_name"`
			AtMs     int64  `json:"at_ms"`
		}
		calls := plane.Calls()
		out := make([]callJSON, 0, len(calls))
		for _, c := range calls {
			out = append(out, callJSON{ToolName: c.ToolName, AtMs: c.AtMs})
		}
		JSON(w, http.StatusOK, map[string]any{
			"phase":    string(plane.Phase()),
			"stamp":    plane.Stamp(),
			"calls":    out,
			"counter":  plane.SummarizeNow(),
			"streamed": plane.Streamed(),
		})
	})
	return mux
}
