package antigravity

// asksupport_test.go — the ASK harness: a Bridge wired to the fake child, a recorded bus, and the two drivers
// every Ask test needs (raise one permission and answer it; send one turn and wait for idle).
//
// IT IS SEPARATE FROM testsupport_test.go ON PURPOSE. That file's harness is the WORKER harness: it asserts on
// ExecutionCallbacks, the usage recorder and the session store. An Ask conversation has none of those — it has
// a bus. Keeping the two apart is what stops a worker assertion being quietly satisfied by an Ask event.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askHarness is a Bridge plus everything a test observes about its conversations.
type askHarness struct {
	t *testing.T
	b *Bridge

	mu sync.Mutex
	// bus is the most recent conversation's recorded stream. It is a FIELD, not a method, because the tests
	// read it as `h.bus.permissionEvents()` — the shape they were written in.
	bus   *askBusRecord
	asks  []*askBusRecord
	procs []*fakeACP
	// nextDecision is the decision the harness applies to the next card it sees (see queueAnswer).
	nextDecision string
	// ledger records the ASK diff-ledger invocations.
	ledger []ledgerCall
	// execLedger records invocations of the EXECUTION ledger, which an Ask edit must never reach.
	execLedger []ledgerCall
}

// askBusRecord is one conversation's recorded event stream, in arrival order.
type askBusRecord struct {
	mu     sync.Mutex
	events []scheduler.SessionEvent
	convID string
}

func (r *askBusRecord) add(e scheduler.SessionEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *askBusRecord) all() []scheduler.SessionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scheduler.SessionEvent(nil), r.events...)
}

// permissionEvents returns every permission event the bus recorded.
func (r *askBusRecord) permissionEvents() []scheduler.SessionEvent {
	var out []scheduler.SessionEvent
	for _, e := range r.all() {
		if e.Kind == "permission" {
			out = append(out, e)
		}
	}
	return out
}

// toolResults returns every tool_result event the bus recorded.
func (r *askBusRecord) toolResults() []scheduler.SessionEvent {
	var out []scheduler.SessionEvent
	for _, e := range r.all() {
		if e.Kind == "tool_result" {
			out = append(out, e)
		}
	}
	return out
}

// kindsAndTypes renders the recorded sequence as "kind/type" pairs, for a mapping assertion.
func (r *askBusRecord) kindsAndTypes() []string {
	var out []string
	for _, e := range r.all() {
		if e.Type != "" {
			out = append(out, e.Kind+"/"+e.Type)
			continue
		}
		out = append(out, e.Kind)
	}
	return out
}

// has reports whether an event with the kind and a text containing want arrived.
func (r *askBusRecord) has(kind, want string) bool {
	for _, e := range r.all() {
		if e.Kind == kind && containsStr0(e.Text, want) {
			return true
		}
	}
	return false
}

func containsStr0(hay, needle string) bool {
	return needle == "" || strings.Contains(hay, needle)
}

// newAskHarness builds a bridge wired exactly as the server wires it, minus the pool.
func newAskHarness(t *testing.T) *askHarness {
	t.Helper()
	h := &askHarness{t: t, b: New(quiet())}
	h.b.SetAskRoot(t.TempDir())
	h.b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal", ShareTelemetry: false}, "", nil
	})
	h.b.SetBinaryOverride(func() (string, error) { return "/fake/agy", nil })
	h.b.SetDiffLedger(
		func(_ context.Context, execID, tenantID, execDir, toolName string, input map[string]any, _ string) {
			h.mu.Lock()
			h.execLedger = append(h.execLedger, ledgerCall{
				ExecID: execID, TenantID: tenantID, ExecDir: execDir, Tool: toolName, Input: input,
			})
			h.mu.Unlock()
		},
		func(_ context.Context, convID, tenantID, execDir, toolName string, input map[string]any, _ string) {
			h.mu.Lock()
			h.ledger = append(h.ledger, ledgerCall{
				ExecID: convID, TenantID: tenantID, ExecDir: execDir, Tool: toolName, Input: input,
			})
			h.mu.Unlock()
		},
	)
	// ONE CONVERSATION IS OPEN FROM THE START, because every Ask test starts by asking "what did the operator
	// see?" and a bus that was never subscribed answers nothing.
	h.subscribe("conv-test")
	t.Cleanup(h.b.CloseAsk)
	return h
}

// serve installs a script and starts recording every conversation's bus.
func (h *askHarness) serve(sc script) {
	h.b.SetSpawnOverride(func(_ context.Context, spec acp.ProcSpec) (acp.Proc, error) {
		fp := newFakeACP(h.t, sc)
		fp.argv = append([]string(nil), spec.Argv...)
		fp.env = append([]string(nil), spec.Env...)
		fp.cwd = spec.Cwd
		h.mu.Lock()
		h.procs = append(h.procs, fp)
		h.mu.Unlock()
		h.t.Cleanup(fp.shutdown)
		return fp, nil
	})
}

// askDir overrides the conversation's working directory, so a test can point at a real project tree.
func (h *askHarness) askDir(dir string) {
	// The directory is derived from askRoot/<convID> at session creation, so pointing it at a fixed tree means
	// driving the session directly after Subscribe has created it.
	h.b.SetAskRoot(dir)
}

// projectDir wires the project resolver every turn consults.
func (h *askHarness) projectDir(dir string) {
	h.b.SetAskProjectDirResolver(func(context.Context) string { return dir })
}

// subscribe opens (and starts recording) a conversation's bus.
func (h *askHarness) subscribe(convID string) *askBusRecord {
	h.t.Helper()
	bus, err := h.b.Subscribe(context.Background(), convID)
	if err != nil {
		h.t.Fatalf("Subscribe(%q): %v", convID, err)
	}
	rec := &askBusRecord{convID: convID}
	h.mu.Lock()
	h.asks = append(h.asks, rec)
	h.bus = rec
	h.mu.Unlock()
	go func() {
		for {
			select {
			case <-bus.Done():
				return
			case evt, open := <-bus.Events():
				if !open {
					return
				}
				rec.add(evt)
				// A QUEUED ANSWER IS APPLIED HERE, because the card is raised inside the turn's prompt and
				// the test's own goroutine is blocked waiting for that turn.
				if evt.Kind == "permission" {
					go h.answerNextFromQueue(convID, evt.PermissionID)
				}
			}
		}
	}()
	return rec
}

// bus returns the recorder for the most recent subscription.
func (h *askHarness) busRec() *askBusRecord {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.asks) == 0 {
		h.t.Fatal("no ask bus was subscribed")
	}
	return h.asks[len(h.asks)-1]
}

// proc returns the i-th fake child.
func (h *askHarness) procAt(i int) *fakeACP {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.procs) {
		h.t.Fatalf("no agy process at index %d (have %d)", i, len(h.procs))
	}
	return h.procs[i]
}

// procCount reports how many children have been spawned.
func (h *askHarness) procCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.procs)
}

// ledgerCalls returns the recorded ASK-ledger invocations.
func (h *askHarness) ledgerCalls() []ledgerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ledgerCall(nil), h.ledger...)
}

// execLedgerCalls returns the recorded EXECUTION-ledger invocations.
func (h *askHarness) execLedgerCalls() []ledgerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ledgerCall(nil), h.execLedger...)
}

// modeCtx stamps a context with a turn's mode and tenant, as askorchicon's turn path does.
func modeCtx(mode string) context.Context {
	ctx := askmode.WithMode(context.Background(), mode)
	return tenant.WithID(ctx, "tnt_test")
}

// scopeCtx stamps a context with a turn's mode, tenant AND conversation scope.
func scopeCtx(mode, convID, projectID string) context.Context {
	ctx := askmode.WithConversationScope(modeCtx(mode), askmode.ConversationScope{
		ConversationID: convID,
		ProjectID:      projectID,
	})
	return ctx
}

// askDecision is one resolved permission outcome.
type askDecision struct {
	optionID  string
	cancelled bool
	err       error
}

// permission drives ONE permission request through the session's own handler, with the turn's context.
//
// IT CALLS RequestPermission DIRECTLY rather than scripting the fake child to ask, because the case under test
// is the HANDLER's decision — the wire round-trip is asserted separately (TestAskPermission_AnswerOnce…), where
// the fake really does ask.
func (h *askHarness) permission(t *testing.T, ctx context.Context, rawName, canonical string, input map[string]any) askDecision {
	t.Helper()
	done := h.permissionAsync(t, ctx, rawName, canonical, input)
	return h.awaitDecision(t, done)
}

// permissionAsync starts a permission request and returns the channel its answer arrives on.
func (h *askHarness) permissionAsync(t *testing.T, ctx context.Context, rawName, canonical string, input map[string]any) <-chan askDecision {
	t.Helper()
	s := h.session(t, ctx)
	handler := &askPermissionHandler{s: s}
	out := make(chan askDecision, 1)
	req := acp.PermissionRequest{
		SessionID: "sid-test",
		ToolCall: acp.ToolCallInfo{
			ToolCallID: "call_" + rawName,
			Name:       rawName,
			RawInput:   mustJSON(input),
		},
		Options: defaultPermissionOptions(),
	}
	go func() {
		d, err := handler.RequestPermission(ctx, req)
		out <- askDecision{optionID: d.OptionID, cancelled: d.Cancelled, err: err}
	}()
	return out
}

// session builds (without spawning anything) the conversation's session with the turn's facts recorded.
func (h *askHarness) session(t *testing.T, ctx context.Context) *askSession {
	t.Helper()
	s := h.b.ensureAskSession("conv-test")
	s.mu.Lock()
	s.mode = askmode.ModeFromContext(ctx)
	s.tenantID = tenant.FromContext(ctx)
	s.projectID = askmode.ConversationScopeFromContext(ctx).ProjectID
	s.askDir = t.TempDir()
	s.projectDir = s.askDir
	s.sid = "sid-test"
	// A live context, so the teardown path is reachable: CloseAsk cancels it and releases a parked handler.
	if s.liveCtx == nil {
		s.liveCtx, s.liveCancel = context.WithCancel(context.Background())
	}
	s.mu.Unlock()
	return s
}

// answer replies to a parked ask by its permission id.
func (h *askHarness) answer(t *testing.T, permissionID, decision string) {
	t.Helper()
	s := h.b.ensureAskSession("conv-test")
	ask := s.takeAsk(permissionID)
	if ask == nil {
		t.Fatalf("no pending ask %q (the card was never registered)", permissionID)
	}
	select {
	case ask.decision <- decision:
	default:
		t.Fatalf("the ask %q could not be answered — its channel is full", permissionID)
	}
}

// awaitDecision waits for an async permission result.
func (h *askHarness) awaitDecision(t *testing.T, done <-chan askDecision) askDecision {
	t.Helper()
	select {
	case d := <-done:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the permission handler never returned")
		return askDecision{}
	}
}

// transcriptHasRefusal reports whether a tool_result carrying a refusal reached the bus.
//
// IT POLLS. The bus is drained by its own goroutine, so an event emitted microseconds ago may not be in the
// recorder yet — a one-shot check would fail intermittently on a fast machine, which is the worst kind of
// flake.
func (h *askHarness) transcriptHasRefusal() bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.busRec().toolResults() {
			if e.IsError && containsStr0(e.Output, "REFUSED BY THE PLATFORM") {
				return true
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// permissionAnswered drives one permission request and ANSWERS the card with decision when one is raised.
//
// IT IS THE CARD-PATH DRIVER: a carded ask blocks with no timeout (that is the point), so a test that only
// called permission() on a carded tool would hang until the deadline rather than exercise anything.
func (h *askHarness) permissionAnswered(t *testing.T, ctx context.Context, rawName, canonical string, input map[string]any, decision string) askDecision {
	t.Helper()
	done := h.permissionAsync(t, ctx, rawName, canonical, input)
	// Wait for the card to be raised, then answer it by its id.
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if evs := h.busRec().permissionEvents(); len(evs) > 0 {
			id = evs[len(evs)-1].PermissionID
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if id == "" {
		select {
		case d := <-done:
			return d
		case <-time.After(time.Second):
			t.Fatal("no permission card was raised and the handler did not answer")
		}
	}
	h.answer(t, id, decision)
	return h.awaitDecision(t, done)
}

// mustJSON marshals an input map, panicking in the test on failure.
func mustJSON(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
