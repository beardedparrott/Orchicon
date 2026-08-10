package askorchicon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// --- Fakes for the Task 1 session transport turn loop ---------------------

// fakeBusSub implements opencode.BusSub backed by a caller-fed channel so a
// test can replay serve bus events (text, tool_use, session.idle, ...).
type fakeBusSub struct {
	events chan opencode.BusEvent
	done   chan struct{}
	once   sync.Once
}

func newFakeBusSub() *fakeBusSub {
	return &fakeBusSub{events: make(chan opencode.BusEvent, 64), done: make(chan struct{})}
}

func (f *fakeBusSub) Events() <-chan opencode.BusEvent { return f.events }
func (f *fakeBusSub) Done() <-chan struct{}            { return f.done }
func (f *fakeBusSub) Close()                           { f.once.Do(func() { close(f.done) }) }

// feed pushes an event onto the subscription (test helper).
func (f *fakeBusSub) feed(evt opencode.BusEvent) { f.events <- evt }

// sentMessage captures one SendMessage call.
type sentMessage struct {
	sessionID string
	system    string
	modelRef  string
	text      string
}

// fakeSessionClient is a sessionTurnClient that records every call and lets
// tests drive the SSE event stream and inject failures.
type fakeSessionClient struct {
	mu           sync.Mutex
	nextID       int
	created      []string
	createTitles []string
	createErr    error
	sendCalls    []sentMessage
	sendErrs     []error // consumed in order; nil when exhausted
	sendCall     int
	// sendGate, when non-nil, makes SendMessage block until the channel is
	// closed. Lets a test hold the send in flight so it can replay events
	// that must be observed while sent == false (the stale-idle guard).
	sendGate     chan struct{}
	aborted      []string
	replies      []string
	sub          *fakeBusSub
	subscribeErr error
}

func (f *fakeSessionClient) Subscribe(ctx context.Context) (opencode.BusSub, error) {
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	if f.sub == nil {
		f.sub = newFakeBusSub()
	}
	return f.sub, nil
}

func (f *fakeSessionClient) CreateSession(ctx context.Context, title string) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("ses_%d", f.nextID)
	f.created = append(f.created, id)
	f.createTitles = append(f.createTitles, title)
	return id, nil
}

func (f *fakeSessionClient) SendMessage(ctx context.Context, sessionID, system, modelRef, text string) error {
	f.mu.Lock()
	f.sendCalls = append(f.sendCalls, sentMessage{sessionID, system, modelRef, text})
	if f.sendCall < len(f.sendErrs) {
		err := f.sendErrs[f.sendCall]
		f.sendCall++
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	// Block on the gate AFTER recording the call so a test can observe the
	// in-flight send and replay events while sent == false.
	if f.sendGate != nil {
		select {
		case <-f.sendGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *fakeSessionClient) Abort(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, sessionID)
	return nil
}

func (f *fakeSessionClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, permissionID)
	return nil
}

// busText builds a completed text-part bus event for a session.
func busText(sessionID, text string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type": "text", "text": text,
				"time": map[string]any{"start": 1.0, "end": 2.0},
			},
		},
	}
}

func busTool(sessionID string) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type":  "tool",
				"tool":  "list_projects",
				"state": map[string]any{"status": "completed", "output": "[]"},
			},
		},
	}
}

func busIdle(sessionID string) opencode.BusEvent {
	return opencode.BusEvent{Type: "session.idle", Properties: map[string]any{"sessionID": sessionID}}
}

func busSessionError(sessionID, message string) opencode.BusEvent {
	return opencode.BusEvent{Type: "session.error", Properties: map[string]any{
		"sessionID": sessionID,
		"error":     map[string]any{"name": "APIError", "message": message},
	}}
}

func busPermissionAsked(sessionID, permID string) opencode.BusEvent {
	return opencode.BusEvent{Type: "permission.asked", Properties: map[string]any{
		"sessionID": sessionID, "id": permID,
	}}
}

// collectEvents is a streamCallback that accumulates text and relays tool
// calls, mirroring what ChatStream's callback does.
type collectEvents struct {
	text []string
	tool int
	err  error
}

func (c *collectEvents) cb(evt opencodeEvent) error {
	switch evt.Type {
	case "text":
		if t, _ := evt.Part["text"].(string); t != "" {
			c.text = append(c.text, t)
		}
	case "tool_use":
		c.tool++
	}
	return c.err
}

// runTurn runs runOpenCodeTurn against a fake client. It returns after the
// turn loop reaches a terminal state, then the caller inspects the fake.
func runTurn(t *testing.T, client *fakeSessionClient, sessionID string, feed func(*fakeBusSub)) (string, string, error) {
	t.Helper()
	s := &Service{log: slog.Default()}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "2s")

	col := &collectEvents{}
	done := make(chan struct{})
	var resMsgID, resSid string
	var resErr error
	go func() {
		defer close(done)
		sub, serr := client.Subscribe(context.Background())
		if serr != nil {
			resErr = serr
			return
		}
		defer sub.Close()
		if feed != nil {
			go feed(sub.(*fakeBusSub))
		}
		resMsgID, resSid, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", sessionID, "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", col.cb)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOpenCodeTurn did not return within 10s")
	}
	return resMsgID, resSid, resErr
}

// --- First message: create + seed + persist ----------------------------------

// TestRunOpenCodeTurnFirstMessageCreatesAndSeedsSession verifies the first
// message in a conversation (no persisted session id) creates a fresh
// session, uses the seeded system prompt (DB history included), and returns
// the created session id.
func TestRunOpenCodeTurnFirstMessageCreatesAndSeedsSession(t *testing.T) {
	client := &fakeSessionClient{}
	feed := func(sub *fakeBusSub) {
		sub.feed(busText("ses_1", "Hello there"))
		sub.feed(busIdle("ses_1"))
	}
	msgID, sid, err := runTurn(t, client, "", feed)
	if err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if msgID == "" {
		t.Error("expected a message id")
	}
	if sid != "ses_1" {
		t.Errorf("returned session id = %q, want ses_1", sid)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 || client.created[0] != "ses_1" {
		t.Errorf("created = %v, want [ses_1]", client.created)
	}
	if len(client.createTitles) != 1 || client.createTitles[0] != "ask-orchicon:conv_1" {
		t.Errorf("create titles = %v, want [ask-orchicon:conv_1]", client.createTitles)
	}
	if len(client.sendCalls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(client.sendCalls))
	}
	if got := client.sendCalls[0]; got.sessionID != "ses_1" || got.system != "SEED_SYSTEM" {
		t.Errorf("send = %+v, want session ses_1 with SEED_SYSTEM", got)
	}
}

// TestRunOpenCodeTurnFollowUpReusesSameSession verifies a follow-up message
// (persisted session id) reuses the SAME session — no CreateSession, the
// reuse (no-history) system prompt, and the persisted id returned.
func TestRunOpenCodeTurnFollowUpReusesSameSession(t *testing.T) {
	client := &fakeSessionClient{}
	feed := func(sub *fakeBusSub) {
		sub.feed(busText("ses_live", "Picking up"))
		sub.feed(busIdle("ses_live"))
	}
	msgID, sid, err := runTurn(t, client, "ses_live", feed)
	if err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if msgID == "" {
		t.Error("expected a message id")
	}
	if sid != "ses_live" {
		t.Errorf("returned session id = %q, want ses_live", sid)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 0 {
		t.Errorf("created = %v, want none (reuse)", client.created)
	}
	if len(client.sendCalls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(client.sendCalls))
	}
	if got := client.sendCalls[0]; got.sessionID != "ses_live" || got.system != "REUSE_SYSTEM" {
		t.Errorf("send = %+v, want session ses_live with REUSE_SYSTEM", got)
	}
}

// --- Stale idle before send --------------------------------------------------

// TestRunOpenCodeTurnIgnoresIdleBeforeSend verifies the sent guard: a
// session.idle replayed on the bus while OUR message is still being sent
// (sent == false) must NOT complete the turn — the turn ends only on the
// idle that follows our accepted message.
func TestRunOpenCodeTurnIgnoresIdleBeforeSend(t *testing.T) {
	client := &fakeSessionClient{sendGate: make(chan struct{})}
	s := &Service{log: slog.Default()}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "2s")

	col := &collectEvents{}
	done := make(chan struct{})
	var resMsgID, resSid string
	var resErr error
	go func() {
		defer close(done)
		sub, serr := client.Subscribe(context.Background())
		if serr != nil {
			resErr = serr
			return
		}
		defer sub.Close()
		resMsgID, resSid, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", "", "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", col.cb)
	}()

	// Give the turn loop time to subscribe and block on the send gate.
	deadline := time.After(5 * time.Second)
	for {
		client.mu.Lock()
		n := len(client.sendCalls)
		client.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("send did not start before deadline")
		}
	}

	fsub := client.sub
	// Stale idle + text from a PRIOR turn replayed before our send is
	// accepted. The idle must be ignored (sent == false). The stale text is
	// legitimate telemetry and is relayed; the STALE IDLE must not end the
	// turn.
	fsub.feed(busText("ses_1", "stale text"))
	fsub.feed(busIdle("ses_1"))
	// Wait until the stale events have been consumed by the drain loop
	// (stale text processed ⇒ the idle that follows it in the channel was
	// also consumed while sent == false). Only then release the send gate,
	// so the stale idle cannot race a later sent == true.
	for len(col.text) < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("stale text was never processed before deadline")
		}
	}
	// Release the send; then the real turn produces text + the real idle.
	// Feed the idle twice with a gap: the drain loop's select may consume
	// the first fresh idle before it processes the sendCh result (sent still
	// false → correctly ignored); the second idle is consumed after sent has
	// flipped, so the turn terminates deterministically.
	close(client.sendGate)
	fsub.feed(busText("ses_1", "fresh reply"))
	fsub.feed(busIdle("ses_1"))
	time.Sleep(100 * time.Millisecond)
	fsub.feed(busIdle("ses_1"))

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOpenCodeTurn did not return within 10s")
	}
	if resErr != nil {
		t.Fatalf("turn error: %v", resErr)
	}
	if resMsgID == "" {
		t.Error("expected a message id")
	}
	if resSid != "ses_1" {
		t.Errorf("returned session id = %q, want ses_1", resSid)
	}
	// The stale idle must not have ended the turn early with only stale
	// text. Telemetry text is relayed as it arrives (it is the conversation's
	// own output), but the turn must continue to the post-accept idle and
	// collect the FRESH reply — if the stale idle had completed the turn we
	// would only see the stale text.
	if len(col.text) != 2 || col.text[0] != "stale text" || col.text[1] != "fresh reply" {
		t.Errorf("collected text = %v, want [stale text fresh reply]", col.text)
	}
}

// --- Timeout abort -----------------------------------------------------------

// TestRunOpenCodeTurnTimeoutAborts verifies that when the model never
// finishes (no idle within ORCHICON_ASK_TIMEOUT) the turn aborts the
// session (keeping it for the next message) and returns a timeout error.
func TestRunOpenCodeTurnTimeoutAborts(t *testing.T) {
	client := &fakeSessionClient{}
	s := &Service{log: slog.Default()}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "100ms")

	done := make(chan struct{})
	var resErr error
	go func() {
		defer close(done)
		sub, _ := client.Subscribe(context.Background())
		defer sub.Close()
		_, _, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", "ses_live", "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", func(evt opencodeEvent) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOpenCodeTurn did not return within 10s")
	}
	if resErr == nil || !strings.Contains(resErr.Error(), "timed out") {
		t.Errorf("error = %v, want timeout", resErr)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.aborted) != 1 || client.aborted[0] != "ses_live" {
		t.Errorf("aborted = %v, want [ses_live]", client.aborted)
	}
}

// --- Session error -----------------------------------------------------------

// TestRunOpenCodeTurnSessionErrorEndsTurn verifies a session.error bus event
// ends the turn with an error (session kept — no abort) rather than hanging.
func TestRunOpenCodeTurnSessionErrorEndsTurn(t *testing.T) {
	client := &fakeSessionClient{}
	feed := func(sub *fakeBusSub) {
		sub.feed(busSessionError("ses_live", "provider 500"))
	}
	_, _, err := runTurn(t, client, "ses_live", feed)
	if err == nil || !strings.Contains(err.Error(), "provider 500") {
		t.Fatalf("error = %v, want provider 500 message", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.aborted) != 0 {
		t.Errorf("aborted = %v, want none (session kept)", client.aborted)
	}
}

// --- Lost session recreation -------------------------------------------------

// TestRunOpenCodeTurnRecreatesLostSession verifies a follow-up send that
// 404s (serve data dir wiped / session gone) recreates a fresh session,
// re-seeds the DB history (seed system), persists the new id, and retries
// on the fresh session.
func TestRunOpenCodeTurnRecreatesLostSession(t *testing.T) {
	client := &fakeSessionClient{
		sendErrs: []error{opencode.ErrSessionNotFound},
	}
	// Wait until the retry send has been accepted before feeding text +
	// idle, otherwise the idle can arrive while sent == false and be
	// ignored (and the retry needs the sendErr consumed first).
	feed := func(sub *fakeBusSub) {
		deadline := time.After(5 * time.Second)
		for {
			client.mu.Lock()
			n := len(client.sendCalls)
			client.mu.Unlock()
			if n >= 2 {
				break
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-deadline:
				return
			}
		}
		sub.feed(busText("ses_1", "recreated"))
		sub.feed(busIdle("ses_1"))
	}
	_, sid, err := runTurn(t, client, "ses_lost", feed)
	if err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if sid != "ses_1" {
		t.Errorf("returned session id = %q, want recreated ses_1", sid)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 || client.created[0] != "ses_1" {
		t.Errorf("created = %v, want [ses_1]", client.created)
	}
	if len(client.sendCalls) != 2 {
		t.Fatalf("send calls = %d, want 2 (lost then recreated)", len(client.sendCalls))
	}
	if got := client.sendCalls[0]; got.sessionID != "ses_lost" {
		t.Errorf("first send session = %q, want ses_lost", got.sessionID)
	}
	// The retry uses the fresh session with the SEED system (DB history
	// re-injected — the recreated session has no memory of prior turns).
	if got := client.sendCalls[1]; got.sessionID != "ses_1" || got.system != "SEED_SYSTEM" {
		t.Errorf("retry send = %+v, want session ses_1 with SEED_SYSTEM", got)
	}
}

// TestRunOpenCodeTurnCreateFailureReturnsError verifies a CreateSession
// failure on a first message is terminal (error returned, no fallback
// mid-request — the next message retries).
func TestRunOpenCodeTurnCreateFailureReturnsError(t *testing.T) {
	client := &fakeSessionClient{createErr: errors.New("serve down")}
	_, _, err := runTurn(t, client, "", nil)
	if err == nil || !strings.Contains(err.Error(), "create conversation session") {
		t.Fatalf("error = %v, want create conversation session error", err)
	}
}

// --- Relay of permission.asked and tool_use ---------------------------------

// TestRunOpenCodeTurnRelaysPermissionAndTool verifies permission.asked is
// auto-approved (--auto equivalent) and tool_use events are relayed to the
// callback (never re-executed).
func TestRunOpenCodeTurnRelaysPermissionAndTool(t *testing.T) {
	client := &fakeSessionClient{}
	feed := func(sub *fakeBusSub) {
		sub.feed(busPermissionAsked("ses_live", "perm_1"))
		sub.feed(busTool("ses_live"))
		sub.feed(busText("ses_live", "done"))
		sub.feed(busIdle("ses_live"))
	}
	col := &collectEvents{}
	s := &Service{log: slog.Default()}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "2s")
	done := make(chan struct{})
	var resErr error
	go func() {
		defer close(done)
		sub, _ := client.Subscribe(context.Background())
		defer sub.Close()
		go feed(sub.(*fakeBusSub))
		_, _, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", "ses_live", "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", col.cb)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOpenCodeTurn did not return within 10s")
	}
	if resErr != nil {
		t.Fatalf("turn error: %v", resErr)
	}
	// The permission reply is issued on a goroutine (the --auto equivalent),
	// so poll briefly for it rather than racing the feed.
	client.mu.Lock()
	n := len(client.replies)
	client.mu.Unlock()
	if n == 0 {
		deadline := time.After(2 * time.Second)
		for {
			client.mu.Lock()
			n = len(client.replies)
			client.mu.Unlock()
			if n > 0 {
				break
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-deadline:
				t.Fatal("permission reply was never issued")
			}
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.replies) != 1 || client.replies[0] != "perm_1" {
		t.Errorf("replies = %v, want [perm_1]", client.replies)
	}
	if col.tool != 1 {
		t.Errorf("tool relays = %d, want 1", col.tool)
	}
	if len(col.text) != 1 || col.text[0] != "done" {
		t.Errorf("text = %v, want [done]", col.text)
	}
}

// --- Subscribe failure -------------------------------------------------------

// TestRunOpenCodeTurnSubscribeFailureReturnsError verifies a Subscribe
// failure is terminal (error returned). It drives runOpenCodeTurn directly
// (the runTurn helper pre-subscribes, which would fail first and not
// exercise the turn's own subscribe).
func TestRunOpenCodeTurnSubscribeFailureReturnsError(t *testing.T) {
	client := &fakeSessionClient{subscribeErr: errors.New("stream down")}
	s := &Service{log: slog.Default()}
	t.Setenv("ORCHICON_ASK_TIMEOUT", "2s")
	done := make(chan struct{})
	var resErr error
	go func() {
		defer close(done)
		_, _, _, resErr = s.runOpenCodeTurn(context.Background(), client, "tnt_dev",
			"conv_1", "ses_live", "opencode/deepseek-v4-flash-free",
			"SEED_SYSTEM", "REUSE_SYSTEM", "hello", func(evt opencodeEvent) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOpenCodeTurn did not return within 10s")
	}
	if resErr == nil || !strings.Contains(resErr.Error(), "subscribe") {
		t.Fatalf("error = %v, want subscribe error", resErr)
	}
}
