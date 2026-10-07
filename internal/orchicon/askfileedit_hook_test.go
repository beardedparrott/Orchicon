package orchicon

// Regression pins for the NATIVE Ask file-edit attribution gap: the opencode
// Ask path ledgers through the askorchicon collector hook (gated on a
// `tool_use` part), but the native bridge emits no such part — its Ask turn
// runs tools in-process via NativeBridge.executeToolCalls. The fix wires a
// dedicated Ask ledger hook (SetAskFileEditHook) fired from that funnel,
// attributed to (ask_conversation, <conversation id>) — the exact tuple both
// clients' Ask diff panes query.
//
// These tests pin the TUPLE (not a row count — a row written under the wrong
// owner is exactly the bug), the DELTA-PROOF ORDER (the live row is produced
// BY THE HOOK, with the causing tool's name and NOT `reconcile:git`, before the
// turn's terminal `idle`), and the NON-GIT case (the engine payload needs no
// repo, so the post-turn git sweep cannot be faking the result).

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askHookRecorder is the test-side Ask hook: it records owner id + tool + output
// so the test can assert the ATTRIBUTION the production hook would receive, and
// (optionally) ledgers the engine payload through a real fileedit.Service under
// the Ask owner kind, exactly as the production `newFileEditHook(svc, log,
// db.FileEditOwnerAskConversation)` does.
type askHookCall struct {
	ownerID string
	tool    string
	output  string
}

type askHookRecorder struct {
	mu      sync.Mutex
	calls   []askHookCall
	order   *[]string
	orderMu *sync.Mutex
}

func (r *askHookRecorder) hook(svc *fileedit.Service) func(ctx context.Context, ownerID, tenantID, execDir, tool string, input map[string]any, output string) {
	return func(ctx context.Context, ownerID, tenantID, execDir, tool string, input map[string]any, output string) {
		r.mu.Lock()
		r.calls = append(r.calls, askHookCall{ownerID: ownerID, tool: tool, output: output})
		r.mu.Unlock()
		if r.order != nil {
			r.orderMu.Lock()
			*r.order = append(*r.order, "hook:"+tool)
			r.orderMu.Unlock()
		}
		switch tool {
		case "write", "edit", "batch_write":
			svc.RecordEngineOutput(ctx, tenantID, db.FileEditOwnerAskConversation, ownerID, tool, output)
		}
	}
}

func (r *askHookRecorder) snapshot() []askHookCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]askHookCall(nil), r.calls...)
}

// autoApproveConsent starts a goroutine that answers every parked consent ask
// with "once" — the host-suite mutating tools (write/edit/batch_write/bash) are
// consent-gated, so a test driving executeToolCalls must approve or the call
// never runs. Returns a stop function.
func autoApproveConsent(t *testing.T, b *NativeBridge, ctx context.Context) func() {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, id := range pendingPermIDs(b) {
					_ = b.ReplyPermissionDecision(ctx, "sess", id, "once")
				}
			}
		}
	}()
	return stop
}

// TestAskToolCallFiresFileEditHookWithConversationOwner pins the tuple: a
// native Ask `write` fires the Ask hook once, with ownerID == the conversation
// id and the causing tool's name, and the engine payload ledgers a row under
// (ask_conversation, conv-1) tagged `write` — NOT `reconcile:git`. The tool
// name is the delta-proof: only a LIVE hook row carries the tool that caused
// it; the git sweep's rows are all `reconcile:git`.
func TestAskToolCallFiresFileEditHookWithConversationOwner(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tnt_test")
	b := newChatBridge(t, &chatTestProvider{})
	b.SetAskTools(&fakeAskTools{results: map[string]string{"write": nativeEngineWriteOutput}})

	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	rec := &askHookRecorder{}
	b.SetAskFileEditHook(rec.hook(svc))

	stop := autoApproveConsent(t, b, ctx)
	defer stop()

	bus := newChatBus()
	defer bus.Close()
	var working []Message
	b.executeToolCalls(ctx, bus, &working, []ToolCall{
		{ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	}, "conv-1")

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Ask hook fired %d times, want exactly 1", len(calls))
	}
	if calls[0].ownerID != "conv-1" {
		t.Fatalf("hook owner id = %q, want the conversation id %q", calls[0].ownerID, "conv-1")
	}
	if calls[0].tool != "write" {
		t.Fatalf("hook tool = %q, want write", calls[0].tool)
	}
	if !strings.Contains(calls[0].output, "file_edits") {
		t.Fatalf("hook output = %q, want the engine file_edits payload", calls[0].output)
	}
	// The ledgered row carries the Ask tuple and the causing tool.
	if got := store.count(); got != 1 {
		t.Fatalf("ledger rows = %d, want 1", got)
	}
	store.mu.Lock()
	row := store.rows[0]
	store.mu.Unlock()
	if row.OwnerKind != db.FileEditOwnerAskConversation || row.OwnerID != "conv-1" {
		t.Fatalf("row tuple = (%s, %s), want (%s, conv-1)",
			row.OwnerKind, row.OwnerID, db.FileEditOwnerAskConversation)
	}
	if row.Tool != "write" {
		t.Fatalf("row tool = %q, want write (NOT reconcile:git — this is a LIVE hook row)", row.Tool)
	}
}

// TestAskLiveRowPrecedesTerminal is the DELTA-PROOF against the git sweep: the
// ledger row is produced BY THE HOOK during the turn, BEFORE the turn's
// terminal `idle`. The post-turn reconciler cannot satisfy this — it runs only
// after the terminal transition (askorchicon/chat.go, api.go), and the native
// path never invokes it at all. The recorded sequence shows the hook firing
// before `idle`, and the row's tool is `write` (never `reconcile:git`).
func TestAskLiveRowPrecedesTerminal(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tnt_test")
	prov := &chatTestProvider{rounds: [][]Event{
		{
			TextDelta{Text: "Writing "},
			ToolCall{ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
			Finish{StopReason: StopToolUse},
		},
		{
			TextDelta{Text: "Done."},
			Finish{StopReason: StopStop},
		},
	}}
	b := newChatBridge(t, prov)
	b.SetAskTools(&fakeAskTools{results: map[string]string{"write": nativeEngineWriteOutput}})

	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	var order []string
	var orderMu sync.Mutex
	rec := &askHookRecorder{order: &order, orderMu: &orderMu}
	b.SetAskFileEditHook(rec.hook(svc))

	stop := autoApproveConsent(t, b, ctx)
	defer stop()

	sid, _ := b.CreateConversationSession(ctx, "conv-live", "ask-orchicon:conv-live")
	bus, _ := b.Subscribe(ctx, "conv-live")

	// Consume the bus on a goroutine, recording the event order in the SAME
	// sequence the hook records into. The hook fires causally BEFORE the round's
	// tool_result and idle are emitted, so a correct implementation places
	// "hook:write" ahead of "idle"; the git sweep (which runs after the terminal)
	// could only ever place its row behind it.
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case evt, ok := <-bus.Events():
				if !ok {
					return
				}
				orderMu.Lock()
				switch evt.Kind {
				case "tool_result":
					order = append(order, "tool_result")
				case "idle":
					order = append(order, "idle")
				}
				orderMu.Unlock()
			case <-bus.Done():
				for {
					select {
					case evt, ok := <-bus.Events():
						if !ok {
							return
						}
						orderMu.Lock()
						switch evt.Kind {
						case "tool_result":
							order = append(order, "tool_result")
						case "idle":
							order = append(order, "idle")
						}
						orderMu.Unlock()
					default:
						return
					}
				}
			}
		}
	}()

	if err := b.SendTurnMessage(ctx, "conv-live", sid, "system", "orchicon/ollama/deepseek-v4-flash", "write notes/a.txt"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}

	select {
	case <-consumerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never terminated (no idle)")
	}

	orderMu.Lock()
	seq := append([]string(nil), order...)
	orderMu.Unlock()

	hookIdx, idleIdx := -1, -1
	for i, s := range seq {
		if strings.HasPrefix(s, "hook:") && hookIdx == -1 {
			hookIdx = i
		}
		if s == "idle" && idleIdx == -1 {
			idleIdx = i
		}
	}
	if hookIdx == -1 {
		t.Fatalf("the Ask file-edit hook never fired during the turn (sequence %v)", seq)
	}
	if idleIdx == -1 {
		t.Fatalf("no idle event observed (sequence %v)", seq)
	}
	if hookIdx >= idleIdx {
		t.Fatalf("hook fired at %d, idle at %d — the live row must precede the terminal transition (sequence %v)",
			hookIdx, idleIdx, seq)
	}
	// The row is a live hook row, tagged with the causing tool — never the
	// post-turn sweep's `reconcile:git`.
	if got := store.count(); got != 1 {
		t.Fatalf("ledger rows = %d, want 1", got)
	}
	store.mu.Lock()
	row := store.rows[0]
	store.mu.Unlock()
	if row.OwnerKind != db.FileEditOwnerAskConversation || row.OwnerID != "conv-live" {
		t.Fatalf("row tuple = (%s, %s), want (%s, conv-live)", row.OwnerKind, row.OwnerID, db.FileEditOwnerAskConversation)
	}
	if row.Tool != "write" {
		t.Fatalf("row tool = %q, want write (a reconcile:git row would mean the sweep, not the live hook)", row.Tool)
	}
}

// TestAskLedgerWorksOutsideAGitRepo is the non-git AC: a native Ask session
// whose project dir is NOT a git repository still ledgers its edit. The engine
// payload is exact ground truth produced in-process, so it needs no repo and no
// observer — the post-turn git reconciler would produce nothing here, which is
// precisely why this case cannot be masked.
func TestAskLedgerWorksOutsideAGitRepo(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tnt_test")
	b := newChatBridge(t, &chatTestProvider{})
	b.SetAskTools(&fakeAskTools{results: map[string]string{"write": nativeEngineWriteOutput}})

	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	rec := &askHookRecorder{}
	b.SetAskFileEditHook(rec.hook(svc))

	stop := autoApproveConsent(t, b, ctx)
	defer stop()

	// A t.TempDir() is not a git repo, and the hook is invoked with an empty
	// exec dir (the native Ask path passes "" — the observer is inert), so no
	// git state is involved anywhere in this path.
	nonRepoDir := t.TempDir()
	bus := newChatBus()
	defer bus.Close()
	var working []Message
	b.executeToolCalls(ctx, bus, &working, []ToolCall{
		{ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	}, "conv-nogit")

	if got := store.count(); got != 1 {
		t.Fatalf("ledger rows = %d outside a git repo, want 1 (non-repo dir %s)", got, nonRepoDir)
	}
	store.mu.Lock()
	row := store.rows[0]
	store.mu.Unlock()
	if row.OwnerKind != db.FileEditOwnerAskConversation || row.OwnerID != "conv-nogit" {
		t.Fatalf("row tuple = (%s, %s), want (%s, conv-nogit)", row.OwnerKind, row.OwnerID, db.FileEditOwnerAskConversation)
	}
	if row.Tool != "write" {
		t.Fatalf("row tool = %q, want write", row.Tool)
	}
}

// TestAskHookFailedCallDoesNotLedger is the gating half: a FAILED mutating call
// carries no ground truth, so it must not fire the Ask hook (parity with the
// execution funnel in loop.go, which fires only after a nil error). A phantom
// row for a failed edit is worse than no row.
func TestAskHookFailedCallDoesNotLedger(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tnt_test")
	b := newChatBridge(t, &chatTestProvider{})
	b.SetAskTools(&fakeAskTools{
		results: map[string]string{"write": nativeEngineWriteOutput},
		errs:    map[string]error{"write": context.DeadlineExceeded},
	})

	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	rec := &askHookRecorder{}
	b.SetAskFileEditHook(rec.hook(svc))

	stop := autoApproveConsent(t, b, ctx)
	defer stop()

	bus := newChatBus()
	defer bus.Close()
	var working []Message
	b.executeToolCalls(ctx, bus, &working, []ToolCall{
		{ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt"}`},
	}, "conv-fail")

	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("Ask hook fired %d times on a FAILED call, want 0", len(calls))
	}
	if got := store.count(); got != 0 {
		t.Fatalf("ledger rows = %d on a failed call, want 0", got)
	}
}

// TestAskHookNilIsANoop pins the nil-hook posture: with no Ask hook wired the
// tool still runs and the turn is unaffected (the pre-fix behaviour, minus the
// breakage).
func TestAskHookNilIsANoop(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tnt_test")
	b := newChatBridge(t, &chatTestProvider{})
	b.SetAskTools(&fakeAskTools{results: map[string]string{"write": nativeEngineWriteOutput}})

	stop := autoApproveConsent(t, b, ctx)
	defer stop()

	bus := newChatBus()
	defer bus.Close()
	var working []Message
	b.executeToolCalls(ctx, bus, &working, []ToolCall{
		{ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	}, "conv-nil")

	if len(working) != 1 || working[0].Role != RoleTool || working[0].Content[0].ToolResult.IsError {
		t.Fatalf("working = %+v, want one successful tool result (a nil hook must not break the call)", working)
	}
}
