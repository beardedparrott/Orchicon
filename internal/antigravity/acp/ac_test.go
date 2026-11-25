package acp

// ac_test.go — THE ACCEPTANCE-CRITERIA TESTS, hand-driven rather than fixture-replayed so their
// timing is deterministic.
//
//   AC2  a blocking permission handler does not stop session/update delivery
//   AC3  fail closed: a handler error and a cancelled context both answer with the reject option
//   AC4  an unknown sessionUpdate kind yields Unknown and the session continues
//
// plus the transport robustness a bridge needs: request/response correlation, malformed lines,
// process exit, unknown agent requests, and the Windows-safety guard.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sessionUpdateMsg builds one session/update notification line.
func sessionUpdateMsg(sessionID, kind string, fields map[string]any) string {
	update := map[string]any{"sessionUpdate": kind}
	for k, v := range fields {
		update[k] = v
	}
	doc := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params":  map[string]any{"sessionId": sessionID, "update": update},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// resultMsg builds one JSON-RPC response line.
func resultMsg(id any, result any) string {
	doc := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// requestMsg builds one agent→client request line.
func requestMsg(id any, method string, params any) string {
	doc := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// permissionRequest builds a session/request_permission whose offered options are the recorded
// pair: allow (allow_once) and deny (reject_once).
func permissionRequest(id any, sessionID, toolName string) string {
	return requestMsg(id, methodRequestPermission, map[string]any{
		"sessionId": sessionID,
		"toolCall": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call_1",
			"name":          toolName,
			"kind":          "execute",
			"status":        "pending",
			"title":         toolName,
		},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
		},
	})
}

// startSessionOn starts a client over a fake process, performs the handshake, and opens a
// session — the three steps every hand-driven test needs before it can exercise a turn.
func startSessionOn(t *testing.T, handler Handler, sid string) (*Client, *fakeProc, *Session, *eventSink) {
	t.Helper()
	ctx := ctxT(t)
	c, fp := newTestClient(t, handler)

	// Both handshake calls are answered IN LOCKSTEP with the client's writes (answerNext), never
	// ahead of them: a response emitted before the request is on the wire is dropped as an unknown
	// id and the call would then time out. See answerNext for why that matters.
	initErr := make(chan error, 1)
	go func() {
		_, err := c.Initialize(ctx)
		initErr <- err
	}()
	answerNext(t, fp, methodInitialize, map[string]any{
		"protocolVersion": 1,
		"agentCapabilities": map[string]any{
			"loadSession":        true,
			"promptCapabilities": map[string]any{"embeddedContext": true},
		},
		"agentInfo": map[string]any{"name": "Antigravity ACP Server", "version": "1.5.13"},
	})
	if err := <-initErr; err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	type sessOut struct {
		sess *Session
		err  error
	}
	sessCh := make(chan sessOut, 1)
	go func() {
		s, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/x"})
		sessCh <- sessOut{sess: s, err: err}
	}()
	answerNext(t, fp, methodSessionNew, map[string]any{
		"sessionId": sid,
		"models":    map[string]any{"availableModels": []any{}, "currentModelId": "gemini-4-neon"},
	})
	out := <-sessCh
	sess, err := out.sess, out.err
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sess.ID() != sid {
		t.Fatalf("session id = %q, want %q", sess.ID(), sid)
	}
	// Discard the handshake's own writes: from here on, the next write a test reads is the one it
	// caused, not `initialize` or `session/new`.
	fp.drainWrites()
	return c, fp, sess, newEventSink(sess)
}

// awaitWrittenResult waits for the client's next document and returns it decoded, skipping
// requests (the client's own calls) so a test can assert on an agent ANSWER specifically.
func awaitWrittenResult(t *testing.T, fp *fakeProc, d time.Duration) c2sDoc {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("acp test: no agent answer within %s", d)
		}
		raw, err := fp.awaitWrite(remaining)
		if err != nil {
			t.Fatalf("acp test: %v", err)
		}
		var doc c2sDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("acp test: client wrote invalid JSON: %v", err)
		}
		if doc.Method != "" {
			continue
		}
		return doc
	}
}

// decodeOutcome reads a permission answer.
func decodeOutcome(t *testing.T, doc c2sDoc) permissionOutcome {
	t.Helper()
	var o permissionOutcome
	if err := json.Unmarshal(doc.Result, &o); err != nil {
		t.Fatalf("acp test: permission answer is not an outcome: %v (%s)", err, doc.Result)
	}
	return o
}


// promptIn starts a Prompt on its own goroutine so a test can drive the wire while it runs.
type promptOutcome struct {
	res *PromptResult
	err error
}

func promptIn(sess *Session, blocks ...ContentBlock) <-chan promptOutcome {
	out := make(chan promptOutcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	go func() {
		defer cancel()
		res, err := sess.Prompt(ctx, blocks)
		out <- promptOutcome{res: res, err: err}
	}()
	return out
}

// answerPrompt waits for the client's `session/prompt` request and answers it, using the id the
// client actually allocated. It skips other client writes (a permission answer can precede it),
// which is why tests must not assume the prompt id is a fixed number.
func answerPrompt(t *testing.T, fp *fakeProc, result map[string]any) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("acp test: no session/prompt request within 10s")
		}
		raw, err := fp.awaitWrite(remaining)
		if err != nil {
			t.Fatalf("acp test: %v", err)
		}
		var doc c2sDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("acp test: client wrote invalid JSON: %v", err)
		}
		if doc.Method != methodSessionPrompt {
			continue
		}
		fp.emit(resultMsg(json.RawMessage(doc.ID), result))
		return string(doc.ID)
	}
}

// awaitPrompt resolves a promptIn channel and fails the test on error.
func awaitPrompt(t *testing.T, out <-chan promptOutcome, d time.Duration) *PromptResult {
	t.Helper()
	select {
	case o := <-out:
		if o.err != nil {
			t.Fatalf("Prompt: %v", o.err)
		}
		return o.res
	case <-time.After(d):
		t.Fatal("Prompt did not return")
		return nil
	}
}

// ---------------------------------------------------------------------------------------------
// AC2 — updates keep being delivered while a permission handler blocks.
// ---------------------------------------------------------------------------------------------

// TestPermissionBlocksButUpdatesFlow proves the reader is independent of the handler: the agent
// asks for permission, the handler blocks forever-ish, and three streamed message chunks must
// still arrive. Then the handler is released and the answer must be written.
func TestPermissionBlocksButUpdatesFlow(t *testing.T) {
	const sid = "sess-blocks"
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	handler := &gateHandler{gate: gate, entered: entered}

	_, fp, sess, sink := startSessionOn(t, handler, sid)

	// Mid-turn: a permission request, then three chunks that must not be held hostage by it.
	fp.emit(permissionRequest(100, sid, "run_command"))
	for i := 0; i < 3; i++ {
		fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{
			"content": map[string]any{"type": "text", "text": fmt.Sprintf("chunk-%d", i)},
		}))
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the permission handler was never entered")
	}

	// All three chunks must arrive WHILE the handler is still blocked.
	sink.waitForCount(t, KindAgentMessageChunk, 3, 5*time.Second)
	if got := sink.kinds(); len(got) < 3 {
		t.Fatalf("only %d updates while blocked, want 3 (%v)", len(got), got)
	}
	select {
	case <-gate:
		t.Fatal("gate closed without being released")
	default:
	}

	// Release the handler with an explicit allow and confirm the answer is written.
	close(gate)
	doc := awaitWrittenResult(t, fp, 5*time.Second)
	outcome := decodeOutcome(t, doc)
	if outcome.Outcome.Outcome != "selected" || outcome.Outcome.OptionID != "allow" {
		t.Fatalf("permission answer = %+v, want selected/allow", outcome.Outcome)
	}

	// The turn still terminates normally after all of that.
	out := promptIn(sess, TextBlock("go"))
	answerPrompt(t, fp, map[string]any{
		"stopReason": StopReasonEndTurn,
		"usage":      map[string]any{"inputTokens": 1, "outputTokens": 2, "totalTokens": 3},
	})
	if res := awaitPrompt(t, out, 10*time.Second); res.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want end_turn", res.StopReason)
	}
}

// gateHandler blocks on a gate, reporting when it was entered.
type gateHandler struct {
	gate    chan struct{}
	entered chan struct{}
}

func (h *gateHandler) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionDecision, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return PermissionDecision{}, ctx.Err()
	case <-h.gate:
		return PermissionDecision{OptionID: "allow"}, nil
	}
}

// ---------------------------------------------------------------------------------------------
// AC3 — FAIL CLOSED.
// ---------------------------------------------------------------------------------------------

// TestFailClosed_HandlerError: a handler returning an error must answer the reject optionId the
// fixture offers (deny), never an allow.
func TestFailClosed_HandlerError(t *testing.T) {
	const sid = "sess-failclosed-error"
	_, fp, _, _ := startSessionOn(t, errorHandler{}, sid)

	fp.emit(permissionRequest(200, sid, "run_command"))
	doc := awaitWrittenResult(t, fp, 5*time.Second)
	outcome := decodeOutcome(t, doc)
	if outcome.Outcome.Outcome != "selected" {
		t.Fatalf("permission answer = %+v, want a selected outcome", outcome.Outcome)
	}
	if outcome.Outcome.OptionID != "deny" {
		t.Fatalf("permission answer optionId = %q, want the reject option \"deny\"", outcome.Outcome.OptionID)
	}
	if doc.ID == nil || string(doc.ID) != "200" {
		t.Errorf("answer id = %s, want the request id 200", doc.ID)
	}
}

// TestFailClosed_ContextCancelledWhileWaiting: a context cancelled while the handler is waiting
// must ALSO fail closed on the reject optionId — this is the dangerous case, because the handler
// never got to answer at all.
func TestFailClosed_ContextCancelledWhileWaiting(t *testing.T) {
	const sid = "sess-failclosed-cancel"
	c, fp, _, _ := startSessionOn(t, &blockingHandler{gate: make(chan struct{})}, sid)

	fp.emit(permissionRequest(300, sid, "run_command"))

	// Cancel the context the handler is waiting on WITHOUT closing the process, so the answer can
	// still be observed on the wire. The cancellation is a plain one — not the turn-teardown cause
	// a session/cancel carries — so this is precisely the "the decision went away" case, and it
	// must fail CLOSED on the reject option rather than answering `cancelled`.
	waitFor(t, 5*time.Second, func() bool {
		c.conn.permMu.Lock()
		defer c.conn.permMu.Unlock()
		return len(c.conn.perms[sid]) > 0
	})
	c.conn.serveCancel(context.Canceled)

	doc := awaitWrittenResult(t, fp, 10*time.Second)
	outcome := decodeOutcome(t, doc)
	if outcome.Outcome.Outcome != "selected" || outcome.Outcome.OptionID != "deny" {
		t.Fatalf("permission answer after the waiting context was cancelled = %+v, want selected/deny", outcome.Outcome)
	}
}

// errorHandler always fails.
type errorHandler struct{}

func (errorHandler) RequestPermission(context.Context, PermissionRequest) (PermissionDecision, error) {
	return PermissionDecision{}, errors.New("acp test: operator unavailable")
}

// panickingHandler always panics.
type panickingHandler struct{}

func (panickingHandler) RequestPermission(context.Context, PermissionRequest) (PermissionDecision, error) {
	panic("acp test: handler exploded")
}

// chainedHandler answers from a fixed decision with a fixed error.
type chainedHandler struct {
	decision PermissionDecision
	err      error
}

func (h chainedHandler) RequestPermission(context.Context, PermissionRequest) (PermissionDecision, error) {
	return h.decision, h.err
}

// TestFailClosed_OtherPaths: every other abnormal handler outcome also answers the reject option.
func TestFailClosed_OtherPaths(t *testing.T) {
	cases := []struct {
		name    string
		handler Handler
	}{
		{"panicking handler", panickingHandler{}},
		{"nil handler", nil},
		{"zero decision", chainedHandler{}},
		{"option agy did not offer", chainedHandler{decision: PermissionDecision{OptionID: "allow-everything"}}},
		{"empty option id", chainedHandler{decision: PermissionDecision{OptionID: ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := "sess-failclosed-" + strings.ReplaceAll(tc.name, " ", "-")
			_, fp, _, _ := startSessionOn(t, tc.handler, sid)
			fp.emit(permissionRequest(400, sid, "run_command"))
			doc := awaitWrittenResult(t, fp, 5*time.Second)
			outcome := decodeOutcome(t, doc)
			if outcome.Outcome.Outcome != "selected" || outcome.Outcome.OptionID != "deny" {
				t.Fatalf("permission answer = %+v, want selected/deny (fail closed)", outcome.Outcome)
			}
		})
	}
}

// TestFailClosed_NoRejectOptionOffered: when agy offers no reject option at all, the client must
// still not allow — it answers `cancelled`.
func TestFailClosed_NoRejectOptionOffered(t *testing.T) {
	const sid = "sess-failclosed-noreject"
	_, fp, _, _ := startSessionOn(t, errorHandler{}, sid)

	onlyAllow := requestMsg(500, methodRequestPermission, map[string]any{
		"sessionId": sid,
		"toolCall":  map[string]any{"toolCallId": "call_9", "name": "run_command"},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
		},
	})
	fp.emit(onlyAllow)
	doc := awaitWrittenResult(t, fp, 5*time.Second)
	outcome := decodeOutcome(t, doc)
	if outcome.Outcome.Outcome != "cancelled" {
		t.Fatalf("permission answer = %+v, want cancelled when no reject option exists", outcome.Outcome)
	}
	if outcome.Outcome.OptionID != "" {
		t.Fatalf("permission answer selected optionId %q when no reject option existed", outcome.Outcome.OptionID)
	}
}

// TestFailClosedNoAllowAnywhere is the summary guard: across every fail-closed scenario the client
// must never write an allow optionId it was not explicitly told to write.
func TestFailClosedNoAllowAnywhere(t *testing.T) {
	sid := "sess-failclosed-sweep"
	_, fp, _, _ := startSessionOn(t, errorHandler{}, sid)
	for i := 0; i < 3; i++ {
		fp.emit(permissionRequest(600+i, sid, "run_command"))
	}
	for i := 0; i < 3; i++ {
		doc := awaitWrittenResult(t, fp, 5*time.Second)
		if outcome := decodeOutcome(t, doc); outcome.Outcome.OptionID == "allow" {
			t.Fatalf("client answered allow for request %s", doc.ID)
		}
	}
	if answeredWith(t, fp, "allow") {
		t.Fatal("an allow answer was written during a fail-closed sweep")
	}
}

// ---------------------------------------------------------------------------------------------
// AC4 — an unknown sessionUpdate kind yields Unknown and the session continues.
// ---------------------------------------------------------------------------------------------

// TestUnknownUpdateKindYieldsUnknownAndContinues injects a kind agy does not have yet between two
// ordinary updates and asserts the session neither dies nor reorders.
func TestUnknownUpdateKindYieldsUnknownAndContinues(t *testing.T) {
	const sid = "sess-unknown-kind"
	_, fp, sess, sink := startSessionOn(t, allowAllHandler{}, sid)

	fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{
		"content": map[string]any{"type": "text", "text": "before"},
	}))
	fp.emit(sessionUpdateMsg(sid, "future_thing_from_agy_2", map[string]any{"x": 1, "nested": map[string]any{"y": true}}))
	fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{
		"content": map[string]any{"type": "text", "text": "after"},
	}))

	sink.waitForCount(t, "future_thing_from_agy_2", 1, 5*time.Second)
	out := promptIn(sess, TextBlock("go"))
	answerPrompt(t, fp, map[string]any{
		"stopReason": StopReasonEndTurn,
		"usage":      map[string]any{"totalTokens": 10},
	})
	res := awaitPrompt(t, out, 10*time.Second)
	if res.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want end_turn (an unknown update must not end the turn)", res.StopReason)
	}

	sink.waitStable(t, 3, 5*time.Second)
	mustKindSequence(t, sink.kinds(), KindAgentMessageChunk, "future_thing_from_agy_2", KindAgentMessageChunk)

	u, ok := sink.findKind("future_thing_from_agy_2")
	if !ok {
		t.Fatal("the unknown update never reached the session")
	}
	unknown, ok := u.(Unknown)
	if !ok {
		t.Fatalf("unknown kind decoded as %T, want Unknown", u)
	}
	if unknown.UpdateKind != "future_thing_from_agy_2" {
		t.Errorf("Unknown.UpdateKind = %q, want future_thing_from_agy_2", unknown.UpdateKind)
	}
	if unknown.Err != nil {
		t.Errorf("Unknown.Err = %v, want nil for a well-formed unknown kind", unknown.Err)
	}
	if !strings.Contains(string(unknown.Raw()), "future_thing_from_agy_2") {
		t.Errorf("Unknown.Raw = %s, want the raw update object", unknown.Raw())
	}

	// The messages either side of it are intact: the unknown did not swallow them.
	var texts []string
	for _, e := range sink.all() {
		if m, ok := e.(AgentMessageChunk); ok {
			texts = append(texts, m.Content.Text)
		}
	}
	if strings.Join(texts, ",") != "before,after" {
		t.Errorf("message texts = %v, want [before after]", texts)
	}
}

// TestUndecodableKnownUpdateIsUnknownNotFatal: a KNOWN kind whose payload is the wrong shape also
// degrades to Unknown instead of failing the reader.
func TestUndecodableKnownUpdateIsUnknownNotFatal(t *testing.T) {
	const sid = "sess-bad-payload"
	_, fp, sess, sink := startSessionOn(t, allowAllHandler{}, sid)

	// content is an array where every other update puts an object.
	fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{"content": []int{1, 2, 3}}))
	fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{
		"content": map[string]any{"type": "text", "text": "recovered"},
	}))

	out := promptIn(sess, TextBlock("go"))
	answerPrompt(t, fp, map[string]any{"stopReason": StopReasonEndTurn, "usage": map[string]any{"totalTokens": 1}})
	awaitPrompt(t, out, 10*time.Second)
	sink.waitStable(t, 2, 5*time.Second)

	first := sink.all()[0]
	unknown, ok := first.(Unknown)
	if !ok {
		t.Fatalf("a malformed known update decoded as %T, want Unknown", first)
	}
	if unknown.UpdateKind != KindAgentMessageChunk {
		t.Errorf("Unknown.UpdateKind = %q, want %q", unknown.UpdateKind, KindAgentMessageChunk)
	}
	if unknown.Err == nil {
		t.Error("Unknown.Err is nil; a decode failure must be reported")
	}
	if second, ok := sink.all()[1].(AgentMessageChunk); !ok || second.Content.Text != "recovered" {
		t.Errorf("the update after a malformed one = %+v, want recovered", sink.all()[1])
	}
}

// TestSyntheticThoughtChunkAndPlan covers the two variants agy never emits, so the client's
// handling of them cannot silently rot.
func TestSyntheticThoughtChunkAndPlan(t *testing.T) {
	raw := json.RawMessage(`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}`)
	u := decodeUpdate("s", raw)
	thought, ok := u.(AgentThoughtChunk)
	if !ok {
		t.Fatalf("agent_thought_chunk decoded as %T", u)
	}
	if thought.Content.Text != "thinking" {
		t.Errorf("thought text = %q, want thinking", thought.Content.Text)
	}

	planRaw := json.RawMessage(`{"sessionUpdate":"plan","entries":[
		{"content":"read the file","priority":"high","status":"completed"},
		{"content":"edit it","priority":"medium","status":"pending"}]}`)
	pu := decodeUpdate("s", planRaw)
	plan, ok := pu.(Plan)
	if !ok {
		t.Fatalf("plan decoded as %T", pu)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("plan entries = %d, want 2", len(plan.Entries))
	}
	if plan.Entries[0].Status != StatusCompleted || plan.Entries[1].Content != "edit it" {
		t.Errorf("plan entries = %+v", plan.Entries)
	}
}

// ---------------------------------------------------------------------------------------------
// Transport robustness.
// ---------------------------------------------------------------------------------------------

// TestUnknownAgentRequestAnswered32601: an agent request this client does not implement gets a
// JSON-RPC method-not-found, so agy is never left waiting.
func TestUnknownAgentRequestAnswered32601(t *testing.T) {
	const sid = "sess-unknown-request"
	_, fp, _, _ := startSessionOn(t, allowAllHandler{}, sid)

	fp.emit(requestMsg(700, "fs/read_text_file", map[string]any{"sessionId": sid, "path": "/etc/passwd"}))
	doc := awaitWrittenResult(t, fp, 5*time.Second)
	if doc.ID == nil || string(doc.ID) != "700" {
		t.Fatalf("error answer id = %s, want 700", doc.ID)
	}
	if doc.Error == nil {
		t.Fatalf("answer to an unknown request = %s, want a JSON-RPC error", doc.Result)
	}
	if doc.Error.Code != CodeMethodNotFound {
		t.Errorf("error code = %d, want %d (method not found)", doc.Error.Code, CodeMethodNotFound)
	}
	if !strings.Contains(doc.Error.Message, "fs/read_text_file") {
		t.Errorf("error message = %q, want it to name the method", doc.Error.Message)
	}
}

// TestMalformedLineDoesNotKillReader: a non-JSON line, an empty line and a JSON-RPC-less object
// must all be skipped without ending the session.
func TestMalformedLineDoesNotKillReader(t *testing.T) {
	const sid = "sess-malformed"
	_, fp, sess, sink := startSessionOn(t, allowAllHandler{}, sid)

	fp.emit("this is not json at all")
	fp.emit(`{"jsonrpc":"2.0","id":` /* truncated */)
	fp.emit("")
	fp.emit(`{"unexpected":"shape"}`)
	fp.emit(sessionUpdateMsg(sid, KindAgentMessageChunk, map[string]any{
		"content": map[string]any{"type": "text", "text": "still alive"},
	}))
	out := promptIn(sess, TextBlock("go"))
	answerPrompt(t, fp, map[string]any{
		"stopReason": StopReasonEndTurn,
		"usage":      map[string]any{"totalTokens": 7},
	})
	res := awaitPrompt(t, out, 10*time.Second)
	if res.StopReason != StopReasonEndTurn || res.Usage.TotalTokens != 7 {
		t.Errorf("result = %+v, want end_turn/7", res)
	}
	sink.waitForCount(t, KindAgentMessageChunk, 1, 5*time.Second)
	if got := sink.firstOfKind(t, KindAgentMessageChunk, time.Second).(AgentMessageChunk); got.Content.Text != "still alive" {
		t.Errorf("message after malformed lines = %q, want \"still alive\"", got.Content.Text)
	}
}

// TestAgentRequestIDIsEchoedVerbatim: agy identifies ITS OWN requests with ids this client never
// issued — a STRING here, where every id the client sends is a number. The answer must echo that
// id verbatim or agy cannot correlate the decision with the tool call it is waiting on, and the
// turn stalls.
//
// NOTE: correlation the OTHER way (agy answering a numeric client id with a string) is not
// exercised, because no fixture records it: `deliver` keys pending calls by the exact id the
// client wrote, so an asymmetric answer would not match and the call would run to its deadline.
// Adding a fabricated recording to "prove" it would be inventing protocol behaviour.
func TestAgentRequestIDIsEchoedVerbatim(t *testing.T) {
	const sid = "sess-ids"
	_, fp, _, _ := startSessionOn(t, allowAllHandler{}, sid)

	fp.emit(permissionRequest("1", sid, "run_command"))

	doc := awaitWrittenResult(t, fp, 5*time.Second)
	if string(doc.ID) != `"1"` {
		t.Fatalf("permission answer id = %s, want the string \"1\" echoed verbatim", doc.ID)
	}
	if doc.Result == nil {
		t.Errorf("permission answer has no result: %s", doc.ID)
	}
}

// TestConcurrentCallsCorrelate: two calls in flight get their own answers, in either order.
func TestConcurrentCallsCorrelate(t *testing.T) {
	const sid = "sess-concurrent"
	_, fp, sess, _ := startSessionOn(t, allowAllHandler{}, sid)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := make([]string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = sess.SetModel(ctxT(t), "model-a")
	}()
	go func() {
		defer wg.Done()
		errs[1] = sess.SetMode(ctxT(t), "mode-a")
	}()

	// Read both requests, record their ids, then answer in REVERSE order.
	for i := 0; i < 2; i++ {
		raw, err := fp.awaitWrite(5 * time.Second)
		if err != nil {
			t.Fatalf("await request %d: %v", i, err)
		}
		var doc c2sDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		ids[i] = string(doc.ID)
		switch doc.Method {
		case methodSessionSetModel:
			errs[0] = errors.Join(errs[0], nil)
		case methodSessionSetMode:
		default:
			t.Fatalf("unexpected method %q", doc.Method)
		}
	}
	fp.emit(resultMsg(json.RawMessage(ids[1]), map[string]any{}))
	fp.emit(resultMsg(json.RawMessage(ids[0]), map[string]any{}))

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent calls did not both complete")
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

// TestCallContextCancel: a call whose context ends returns that error and leaves the client
// usable.
func TestCallContextCancel(t *testing.T) {
	const sid = "sess-ctxcancel"
	_, fp, sess, _ := startSessionOn(t, allowAllHandler{}, sid)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Consume the request, then let the context die before answering.
		_, _ = fp.awaitWrite(5 * time.Second)
		cancel()
	}()
	err := sess.SetModel(ctx, "model-a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SetModel with a cancelled context = %v, want context.Canceled", err)
	}

	// Still usable: a later call completes.
	fp.emit("")
	go func() {
		raw, err := fp.awaitWrite(5 * time.Second)
		if err != nil {
			return
		}
		var doc c2sDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return
		}
		fp.emit(resultMsg(json.RawMessage(doc.ID), map[string]any{}))
	}()
	if err := sess.SetMode(ctxT(t), "mode-a"); err != nil {
		t.Fatalf("SetMode after a cancelled call: %v", err)
	}
}

// TestProcessExitFailsPendingCalls: when the child dies mid-call, the pending call fails with an
// error carrying the exit code AND the stderr tail — a bare "closed" is unactionable.
func TestProcessExitFailsPendingCalls(t *testing.T) {
	const sid = "sess-exit"
	c, fp, sess, sink := startSessionOn(t, allowAllHandler{}, sid)

	fp.emitStderr("agy: fatal: language server failed to start")
	fp.emitStderr("agy: hint: run `agy` and sign in")

	errCh := make(chan error, 1)
	go func() {
		errCh <- sess.SetModel(ctxT(t), "model-a")
	}()
	// Let the request reach the wire, then kill the process.
	if _, err := fp.awaitWrite(5 * time.Second); err != nil {
		t.Fatalf("await SetModel write: %v", err)
	}
	fp.shutdown(9, errors.New("exit status 9"))

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("SetModel succeeded against a dead process")
		}
		if !errors.Is(err, ErrClosed) {
			t.Errorf("error = %v, want it to match ErrClosed", err)
		}
		var closed *ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("error = %T, want *ClosedError", err)
		}
		if closed.ExitCode != 9 {
			t.Errorf("ClosedError.ExitCode = %d, want 9", closed.ExitCode)
		}
		if len(closed.Stderr) == 0 {
			t.Error("ClosedError.Stderr is empty; the child's diagnostic must be carried")
		} else if !strings.Contains(strings.Join(closed.Stderr, "\n"), "language server failed to start") {
			t.Errorf("ClosedError.Stderr = %v, want the fatal line", closed.Stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pending call did not fail when the process exited")
	}

	// The session's events end, so a consumer ranging over them terminates.
	select {
	case <-sink.closedCh():
	case <-time.After(10 * time.Second):
		t.Fatal("the session's Events() channel did not close after the process exited")
	}
	if !c.Exited() {
		t.Error("Exited() = false after the process exited")
	}
	if c.ExitErr() == nil {
		t.Error("ExitErr() = nil after the process exited")
	}
}

// closedCh reports when the sink's stream ended.
func (k *eventSink) closedCh() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			k.mu.Lock()
			closed := k.closed
			k.mu.Unlock()
			if closed {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return ch
}

// TestStderrRingBounded: the tail keeps the LAST lines and never grows past the cap.
func TestStderrRingBounded(t *testing.T) {
	ch := make(chan []byte, stderrRingMaxLines+64)
	ring := startStderrDrain(ch)
	for i := 0; i < stderrRingMaxLines+50; i++ {
		ch <- []byte(fmt.Sprintf("line-%d", i))
	}
	close(ch)
	ring.stop()

	tail := ring.Tail()
	if len(tail) != stderrRingMaxLines {
		t.Fatalf("retained %d lines, want %d", len(tail), stderrRingMaxLines)
	}
	if want := fmt.Sprintf("line-%d", stderrRingMaxLines+49); tail[len(tail)-1] != want {
		t.Errorf("last retained line = %q, want %q", tail[len(tail)-1], want)
	}
	// The FIRST %d lines are gone — that bias towards the tail is the whole point.
	if tail[0] != fmt.Sprintf("line-%d", 50) {
		t.Errorf("first retained line = %q, want line-50", tail[0])
	}
}

// TestLaunchOptionsArgv: the flag order is fixed and empty options add nothing.
func TestLaunchOptionsArgv(t *testing.T) {
	cases := []struct {
		name string
		opts LaunchOptions
		want []string
	}{
		{"defaults", LaunchOptions{}, []string{"agy", "acp"}},
		{"everything", LaunchOptions{
			Binary: "agy-dev", DisableTelemetry: true, ProcessPerSession: true,
			AppDataDir: "/tmp/appdata", Sandbox: true,
		}, []string{"agy-dev", "acp", "--disable-telemetry", "--process-per-session", "--app-data-dir", "/tmp/appdata", "--sandbox"}},
		{"telemetry only", LaunchOptions{DisableTelemetry: true}, []string{"agy", "acp", "--disable-telemetry"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.opts.Argv()
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("Argv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConfigRequiresFactory: Start refuses to invent a spawner, so a test can never start a real
// agy by accident.
func TestConfigRequiresFactory(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("Start with no factory succeeded, want an error")
	}
}

// TestNoPOSIXSignalsInPackage is AC5's guard, and it works from the SOURCE rather than from build
// tags: parsing the AST catches a platform-specific import or a syscall signal even on a build
// where it would compile.
func TestNoPOSIXSignalsInPackage(t *testing.T) {
	banned := map[string]string{
		"golang.org/x/sys/unix": "a POSIX-only syscall package",
		"syscall":               "the platform-dependent syscall package",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found")
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if why, bad := banned[p]; bad {
				t.Errorf("%s imports %q (%s); the package must build and behave on Windows too", path, p, why)
			}
			if p == "os/signal" {
				t.Errorf("%s imports os/signal; shutdown must be stdin close + os.Process.Kill", path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "syscall" {
				t.Errorf("%s:%d references syscall.%s", path, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
			if ident.Name == "unix" {
				t.Errorf("%s:%d references unix.%s", path, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestCloseIsIdempotent: Close can be called twice, and it succeeds both times.
func TestCloseIsIdempotent(t *testing.T) {
	c, fp := newTestClient(t, allowAllHandler{})
	ctx := context.Background()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_ = fp
	// A call on a closed client reports ErrClosed rather than hanging. Authenticate ALSO wraps
	// it in *AuthError (every authenticate failure is an auth failure), so both must match.
	err := c.Authenticate(ctx, "oauth-personal")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Authenticate after Close = %v, want an ErrClosed match", err)
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Errorf("Authenticate after Close = %T, want *AuthError wrapping the closed cause", err)
	}
	// A non-auth call reports the bare closed error, matched the same way.
	if _, _, nerr := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/x"}); !errors.Is(nerr, ErrClosed) {
		t.Errorf("NewSession after Close = %v, want an ErrClosed match", nerr)
	}
}

// TestEmbeddedContextBlockShape pins the embedded-context content block: agy advertises
// promptCapabilities.embeddedContext, and the shape is standard ACP.
func TestEmbeddedContextBlockShape(t *testing.T) {
	b, err := json.Marshal(EmbeddedContextBlock("file:///tmp/a.go", "package main", "text/x-go"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"type":"resource","resource":{"uri":"file:///tmp/a.go","text":"package main","mimeType":"text/x-go"}}`
	if !sameJSONValue(json.RawMessage(b), json.RawMessage(want)) {
		t.Errorf("embedded context block = %s, want %s", b, want)
	}
	text, err := json.Marshal(TextBlock("hi"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(text) != `{"type":"text","text":"hi"}` {
		t.Errorf("text block = %s", text)
	}
}

// TestRejectOptionIDPrecedence: the reject resolver prefers a reject KIND over a deny-looking id,
// and reports false when agy offered no reject at all.
func TestRejectOptionIDPrecedence(t *testing.T) {
	opts := []PermissionOption{
		{OptionID: "yes-please", Name: "Yes, allow", Kind: OptionKindAllowOnce},
		{OptionID: "no-thanks", Name: "No", Kind: OptionKindRejectAlways},
	}
	if got, ok := RejectOptionID(opts); !ok || got != "no-thanks" {
		t.Errorf("RejectOptionID = %q,%v, want no-thanks,true", got, ok)
	}
	if got, ok := AllowOptionID(opts); !ok || got != "yes-please" {
		t.Errorf("AllowOptionID = %q,%v, want yes-please,true", got, ok)
	}
	if _, ok := RejectOptionID([]PermissionOption{{OptionID: "allow", Kind: OptionKindAllowOnce}}); ok {
		t.Error("RejectOptionID found a reject in an allow-only set")
	}
	// A deny-looking id without a kind is still a reject.
	if got, ok := RejectOptionID([]PermissionOption{{OptionID: "deny", Name: "Deny"}}); !ok || got != "deny" {
		t.Errorf("RejectOptionID by name = %q,%v, want deny,true", got, ok)
	}
}

// TestPermissionHandlerTimeoutFailsClosed: an explicit timeout also resolves closed.
func TestPermissionHandlerTimeoutFailsClosed(t *testing.T) {
	SetPermissionTimeout(80 * time.Millisecond)
	t.Cleanup(func() { SetPermissionTimeout(0) })

	const sid = "sess-timeout"
	_, fp, _, _ := startSessionOn(t, &blockingHandler{gate: make(chan struct{})}, sid)
	fp.emit(permissionRequest(800, sid, "run_command"))

	doc := awaitWrittenResult(t, fp, 5*time.Second)
	if outcome := decodeOutcome(t, doc); outcome.Outcome.Outcome != "selected" || outcome.Outcome.OptionID != "deny" {
		t.Fatalf("permission answer after timeout = %+v, want selected/deny", outcome.Outcome)
	}
}

// TestProcFactoryReceivesLaunchArgv: Start derives the child's argv from LaunchOptions when the
// spec has none, so the flags cannot silently go missing.
func TestProcFactoryReceivesLaunchArgv(t *testing.T) {
	var got ProcSpec
	fp := newFakeProc()
	c, err := Start(context.Background(), Config{
		Factory: func(_ context.Context, spec ProcSpec) (Proc, error) {
			got = spec
			return fp, nil
		},
		Launch:  LaunchOptions{DisableTelemetry: true, Sandbox: true},
		Handler: allowAllHandler{},
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if strings.Join(got.Argv, " ") != "agy acp --disable-telemetry --sandbox" {
		t.Errorf("factory argv = %v, want the launch flags", got.Argv)
	}
}

// TestStderrTailReachesClosedError: the client exposes the child's stderr tail for a caller that
// wants to log it even when no call fails.
func TestStderrTailReachesClosedError(t *testing.T) {
	c, fp := newTestClient(t, allowAllHandler{})
	fp.emitStderr("warning: telemetry disabled")
	fp.shutdown(3, nil)
	waitFor(t, 5*time.Second, func() bool { return c.Exited() })
	tail := c.Stderr()
	if len(tail) == 0 {
		t.Fatal("Stderr() is empty after the child wrote a line")
	}
	if !strings.Contains(strings.Join(tail, "\n"), "telemetry disabled") {
		t.Errorf("Stderr() = %v, want the child's line", tail)
	}
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
