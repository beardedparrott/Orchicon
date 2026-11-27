package antigravity

// ask_harness_test.go — the harness every Ask-transport test drives.
//
// IT REUSES testsupport_test.go's fake `agy acp` child (fakeACP) and adds the Ask side: a Bridge wired for
// Ask (ask root, project resolver, usage recorder, the ASK diff ledger), a helper that subscribes and
// sends a turn, and a helper that drains the bus into a slice so a test can assert the exact event
// sequence the collector would see.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askHarness is one Ask conversation's test rig.
type askHarness struct {
	t   *testing.T
	b   *Bridge
	dir string // the conversation project's directory
	con string // the conversation id

	mu     sync.Mutex
	procs  []*fakeACP
	usage  []scheduler.UsageRecord
	ledger []askLedgerCall
	// sid is the agy session id CreateConversationSession returned (agy's own).
	sid string
}

// askLedgerCall is one ASK diff-ledger invocation.
type askLedgerCall struct {
	OwnerID  string
	TenantID string
	Dir      string
	Tool     string
	Input    map[string]any
}

// newAskHarness builds a Bridge wired exactly as the server wires it for Ask, over a fake child.
func newAskHarness(t *testing.T, sc script) *askHarness {
	t.Helper()
	project := t.TempDir()
	askRoot := t.TempDir()

	h := &askHarness{t: t, b: New(quiet()), dir: project, con: "conv_agy_1"}
	h.b.SetAskRoot(askRoot)
	h.b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal", ShareTelemetry: false}, "", nil
	})
	h.b.SetBinaryOverride(func() (string, error) { return "/fake/agy", nil })
	h.b.SetProjectDirResolver(func(_ context.Context, _, projectID string) (string, error) {
		if projectID == "" {
			return "", nil
		}
		return project, nil
	})
	h.b.SetUsageRecorder(func(_ context.Context, rec scheduler.UsageRecord) error {
		h.mu.Lock()
		h.usage = append(h.usage, rec)
		h.mu.Unlock()
		return nil
	})
	// THE ASK LEDGER, NOT THE EXECUTION ONE — the distinction this test exists to pin.
	h.b.SetDiffLedger(nil, func(_ context.Context, ownerID, tenantID, dir, tool string, input map[string]any, _ string) {
		h.mu.Lock()
		h.ledger = append(h.ledger, askLedgerCall{OwnerID: ownerID, TenantID: tenantID, Dir: dir, Tool: tool, Input: input})
		h.mu.Unlock()
	})
	h.b.SetSpawnOverride(func(_ context.Context, spec acp.ProcSpec) (acp.Proc, error) {
		fp := newFakeACP(t, sc)
		fp.argv = append([]string(nil), spec.Argv...)
		fp.env = append([]string(nil), spec.Env...)
		fp.cwd = spec.Cwd
		h.mu.Lock()
		h.procs = append(h.procs, fp)
		h.mu.Unlock()
		t.Cleanup(fp.shutdown)
		return fp, nil
	})
	t.Cleanup(h.b.CloseAsk)
	return h
}

// turnCtx stamps a turn's context exactly as askorchicon does before dispatch.
func (h *askHarness) turnCtx(mode string) context.Context {
	ctx := context.Background()
	ctx = tenant.WithID(ctx, "tnt_ask")
	ctx = askmode.WithMode(ctx, mode)
	return askmode.WithConversationScope(ctx, askmode.ConversationScope{
		ConversationID: h.con,
		ProjectID:      "prj_ask",
	})
}

// create opens the conversation's session the way a first message does.
func (h *askHarness) create(mode string) string {
	h.t.Helper()
	sid, err := h.b.CreateConversationSession(h.turnCtx(mode), h.con, "ask-orchicon:"+h.con)
	if err != nil {
		h.t.Fatalf("CreateConversationSession: %v", err)
	}
	if sid == "" {
		h.t.Fatal("CreateConversationSession returned an empty session id")
	}
	h.mu.Lock()
	h.sid = sid
	h.mu.Unlock()
	return sid
}

// subscribe opens a bus for the conversation.
func (h *askHarness) subscribe(ctx context.Context) scheduler.SessionBus {
	h.t.Helper()
	sub, err := h.b.Subscribe(ctx, h.con)
	if err != nil {
		h.t.Fatalf("Subscribe: %v", err)
	}
	h.t.Cleanup(sub.Close)
	return sub
}

// sendTurn delivers one user turn on a goroutine (the collector's shape) and returns its error channel.
func (h *askHarness) sendTurn(ctx context.Context, sid, mode, text string) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- h.b.SendTurnMessage(ctx, h.con, sid, "you are the Orchicon assistant", "antigravity/google/"+harnessModel, text)
	}()
	return ch
}

// proc returns the i-th fake child (fails when it does not exist).
func (h *askHarness) proc(i int) *fakeACP {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.procs) {
		h.t.Fatalf("no agy child at index %d (have %d)", i, len(h.procs))
	}
	return h.procs[i]
}

// procOrNil returns the i-th fake child, or nil.
func (h *askHarness) procOrNil(i int) *fakeACP {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.procs) {
		return nil
	}
	return h.procs[i]
}

// usageRecords returns the recorded usage samples.
func (h *askHarness) usageRecords() []scheduler.UsageRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]scheduler.UsageRecord(nil), h.usage...)
}

// ledgerCalls returns the recorded ASK ledger invocations.
func (h *askHarness) ledgerCalls() []askLedgerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]askLedgerCall(nil), h.ledger...)
}

// eventCollector drains a SessionBus into a slice, in arrival order.
type eventCollector struct {
	mu     sync.Mutex
	events []scheduler.SessionEvent
	done   chan struct{}
}

// drain starts collecting. It stops when the bus ends or the test does.
func drain(t *testing.T, bus scheduler.SessionBus) *eventCollector {
	t.Helper()
	c := &eventCollector{done: make(chan struct{})}
	go func() {
		for ev := range bus.Events() {
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		}
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}()
	return c
}

// snapshot returns the events collected so far.
func (c *eventCollector) snapshot() []scheduler.SessionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]scheduler.SessionEvent(nil), c.events...)
}

// ofKind returns every collected event of one Kind.
func (c *eventCollector) ofKind(kind string) []scheduler.SessionEvent {
	var out []scheduler.SessionEvent
	for _, ev := range c.snapshot() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// awaitKind blocks until at least n events of one Kind have arrived, and returns them.
func (c *eventCollector) awaitKind(t *testing.T, kind string, n int) []scheduler.SessionEvent {
	t.Helper()
	waitFor(t, kind+" events on the bus", func() bool { return len(c.ofKind(kind)) >= n })
	return c.ofKind(kind)
}

// sessionID is the conversation's live agy session id, for ReplyPermissionDecision/AbortConversationSession.
func (h *askHarness) sessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sid == "" {
		h.t.Fatal("sessionID: no agy session has been opened")
	}
	return h.sid
}

// latestProc returns the most recently spawned fake child. A mode change respawns (the mode's `--settings`
// and MCP set are fixed at spawn), so a test that changed the mode must look at the NEW child.
func (h *askHarness) latestProc() *fakeACP {
	h.t.Helper()
	h.mu.Lock()
	n := len(h.procs)
	h.mu.Unlock()
	if n == 0 {
		h.t.Fatal("latestProc: no agy child has been spawned")
	}
	return h.proc(n - 1)
}

// turnInFlight starts a turn IN THE GIVEN MODE on a HANGING script and returns the send's error channel plus
// a channel the test closes (via ackTurn) to end the turn. It is the state a permission test needs: a prompt
// genuinely in flight, so the request arrives mid-turn exactly as agy sends it.
//
// THE MODE MATTERS: the mode is captured from the turn's context and it is what the permission handler gates
// on. A turn sent under the wrong mode would test the wrong boundary.
func turnInFlight(t *testing.T, h *askHarness, fp *fakeACP, mode string) (<-chan error, chan struct{}) {
	t.Helper()
	sent := h.sendTurn(h.turnCtx(mode), h.sessionID(), mode, "list the directory")
	// THE CHILD MAY BE REPLACED BY THE TURN ITSELF: a mode change rewrites the spawn-time MCP fingerprint, and
	// the turn respawns before prompting. So the prompt lands on the LATEST child, not on the one the caller
	// happened to hold.
	waitFor(t, "the prompt to reach agy", func() bool { return len(h.latestProc().promptsSeen()) >= 1 })
	fp = h.latestProc()
	ack := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-ack:
		default:
			close(ack)
		}
	})
	go func() {
		<-ack
		fp.release()
	}()
	return sent, ack
}

// ackTurn ends a hanging turn and waits for the send to return.
func ackTurn(t *testing.T, _ *fakeACP, ack chan struct{}) {
	t.Helper()
	select {
	case <-ack:
	default:
		close(ack)
	}
}

// askPermissionTargetsFor builds a tool call from raw shapes and extracts the card's targets.
func askPermissionTargetsFor(tool string, args map[string]any, locations []string) []string {
	call := acp.ToolCallInfo{Name: tool, ToolCallID: "call_x"}
	if len(args) > 0 {
		call.RawInput = jsonOf(args)
	}
	for _, l := range locations {
		call.Locations = append(call.Locations, acp.Location{Path: l})
	}
	return askPermissionTargets(tool, args, call)
}

// mkdirAskProject makes a real directory inside the harness project, so a write target is a real path the
// boundary rule can judge.
func (h *askHarness) mkdirAskProject(t *testing.T) string {
	t.Helper()
	docs := filepath.Join(h.dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return docs
}

// containsFold reports whether s contains sub, case-insensitively.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// validJSONObject reports whether s is a syntactically valid JSON object.
func validJSONObject(s string) bool {
	var v map[string]any
	return json.Unmarshal([]byte(s), &v) == nil
}

// awaitCardOrAnswer blocks until the conversation either RAISED A CARD for reqID or agy was ANSWERED, and
// reports which happened. It is the only correct way to assert both halves of the mode boundary in one
// place: a card parks forever until an operator answers, so a test that waited for an answer unconditionally
// would hang on exactly the cases that are supposed to card.
//
// It polls the fake's recorded answers directly rather than draining a channel, so a caller that later reads
// the outcome does not race a second reader.
func awaitCardOrAnswer(t *testing.T, col *eventCollector, fp *fakeACP, reqID string, window time.Duration) (bool, permissionOutcomeDoc) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		if len(col.ofKind("permission")) > 0 {
			return true, permissionOutcomeDoc{}
		}
		fp.mu.Lock()
		_, answered := fp.permAnswers[strconv.Quote(reqID)]
		fp.mu.Unlock()
		if answered {
			return false, fp.awaitPermAnswer(t, reqID)
		}
		if time.Now().After(deadline) {
			if len(col.ofKind("permission")) > 0 {
				return true, permissionOutcomeDoc{}
			}
			// Neither: survivable only if the answer is merely slow. awaitPermAnswer fails the test with the
			// real reason if it never comes.
			return false, fp.awaitPermAnswer(t, reqID)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
