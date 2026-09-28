package orchicon

// askpressure_notice_test.go — a collapse must reach the transcript, not just the log.
//
// The proactive pressure gate compacts INSIDE the bridge, which has no database
// handle. Before the notice sink existed, it therefore left exactly one trace: a
// WARN line in the server log. Prod's own log shows what that costs — a conversation
// of 2,343 messages collapsed into a summary, mid-conversation, with nothing in the
// transcript the operator was reading to say so.
//
// These tests pin that the gate REPORTS a real compaction with enough detail to name
// what was lost and where the original went, and that it reports nothing when
// nothing collapsed.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// windowTestProvider arms the pressure gate. chatTestProvider.ListModels returns no
// models, and a gate with no live window hint stays DISARMED by design
// ("compaction must never guess a window"), so a test of the gate needs a provider
// that reports one.
type windowTestProvider struct {
	*chatTestProvider
	model  string
	window int64
}

func (p *windowTestProvider) ListModels(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: p.model, Context: p.window}}, nil
}

type noticeRecorder struct {
	mu   sync.Mutex
	got  []scheduler.AskCompactNotice
	errs error
}

func (r *noticeRecorder) sink(_ context.Context, n scheduler.AskCompactNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errs != nil {
		return r.errs
	}
	r.got = append(r.got, n)
	return nil
}

func (r *noticeRecorder) notices() []scheduler.AskCompactNotice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scheduler.AskCompactNotice(nil), r.got...)
}

// pressureTestBridge builds a bridge with a persistable history, a measured prompt
// size, and an armed gate, returning the bridge, its session id and the provider.
func pressureTestBridge(t *testing.T, dir string, promptTokens, window int64) (*NativeBridge, string, *windowTestProvider) {
	t.Helper()
	prov := &windowTestProvider{
		chatTestProvider: &chatTestProvider{events: []Event{TextDelta{Text: "SUMMARY OF EVERYTHING"}}},
		model:            "deepseek-flash",
		window:           window,
	}
	resolver := ProviderResolverFunc(func(context.Context, string, string) (Provider, error) {
		return prov, nil
	})
	b := NewBridge(resolver, "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	b.SetAskHistoryDir(dir)

	const convID = "01PRESSUREPRESSURETEST000000"
	ctx := context.Background()
	sid, err := b.CreateConversationSession(ctx, convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	// A history long enough to compact, and PERSISTED so the archive step has a
	// file to copy (which is what gives the notice an ArchivePath to name).
	hist := make([]Message, 0, 20)
	for i := 0; i < 20; i++ {
		hist = append(hist, Message{Role: RoleUser, Content: []Content{{Text: compactTestStr("message body")}}})
	}
	b.mu.Lock()
	b.chatHistory[sid] = hist
	b.persistAskHistoryLocked(sid)
	b.mu.Unlock()
	// The measured numerator. 0 disarms the gate, so this is what lets it fire.
	b.recordAskPromptTokens(sid, promptTokens)
	return b, sid, prov
}

// TestAPressureCompactionReportsADurableNotice is the fix: the collapse that prod
// performed silently now produces a record carrying what happened and where the
// original went.
func TestAPressureCompactionReportsADurableNotice(t *testing.T) {
	dir := t.TempDir()
	// 990 of a 1000-token window is over the 0.95 threshold.
	b, sid, prov := pressureTestBridge(t, dir, 990, 1000)

	rec := &noticeRecorder{}
	b.SetAskCompactNotice(rec.sink)

	ctx := tenant.WithID(context.Background(), "tnt")
	b.maybeCompactForPressure(ctx, prov, "01PRESSUREPRESSURETEST000000", sid, "orchicon/deepseek/deepseek-flash", "deepseek-flash")

	got := rec.notices()
	if len(got) != 1 {
		t.Fatalf("notices reported = %d, want exactly 1 — a compaction this consequential must not "+
			"exist only as a log line", len(got))
	}
	n := got[0]
	if n.ConversationID != "01PRESSUREPRESSURETEST000000" {
		t.Errorf("notice conversation = %q, want the compacted conversation", n.ConversationID)
	}
	if n.Reason != "pressure" {
		t.Errorf("notice reason = %q, want %q — the reason is what tells the operator this ran on its "+
			"own rather than because they asked", n.Reason, "pressure")
	}
	if !strings.Contains(n.Detail, "compacted") || !strings.Contains(n.Detail, "summary") {
		t.Errorf("notice detail = %q, want the adapter's outcome (how many messages became what)", n.Detail)
	}
	if n.ArchivePath == "" {
		t.Error("the notice names no archive — the pre-collapse transcript is recoverable by hand, and " +
			"that is the one piece of information the marker exists to give")
	}
	// The compaction really happened, not merely got reported.
	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after >= 20 {
		t.Fatalf("history has %d messages after the gate ran, want it collapsed", after)
	}
}

// TestAGateThatDoesNotFireReportsNoNotice is the other half: a COMPACTION is what
// earns a marker, so a gate that stays under the threshold — the overwhelmingly
// common case — must report nothing at all.
func TestAGateThatDoesNotFireReportsNoNotice(t *testing.T) {
	dir := t.TempDir()
	b, sid, prov := pressureTestBridge(t, dir, 900, 1000) // 90%, under the 0.95 threshold

	rec := &noticeRecorder{}
	b.SetAskCompactNotice(rec.sink)

	ctx := tenant.WithID(context.Background(), "tnt")
	b.maybeCompactForPressure(ctx, prov, "01PRESSUREPRESSURETEST000000", sid, "orchicon/deepseek/deepseek-flash", "deepseek-flash")

	if got := rec.notices(); len(got) != 0 {
		t.Fatalf("an unfired gate reported %d notices (%+v) — the transcript must not gain a marker "+
			"for a compaction that did not happen", len(got), got)
	}
	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after != 20 {
		t.Fatalf("history has %d messages, want it untouched at 20", after)
	}
}

// TestANoticeRecordingFailureDoesNotBreakTheCompaction pins the error posture. The
// collapse has already been applied and cannot be undone, so a sink that fails must
// be survivable: the transcript loses its marker, the conversation does not lose its
// ability to continue.
func TestANoticeRecordingFailureDoesNotBreakTheCompaction(t *testing.T) {
	dir := t.TempDir()
	b, sid, prov := pressureTestBridge(t, dir, 990, 1000)

	rec := &noticeRecorder{errs: errors.New("database unavailable")}
	b.SetAskCompactNotice(rec.sink)

	ctx := tenant.WithID(context.Background(), "tnt")
	// Must not panic, and must not propagate the sink's error.
	b.maybeCompactForPressure(ctx, prov, "01PRESSUREPRESSURETEST000000", sid, "orchicon/deepseek/deepseek-flash", "deepseek-flash")

	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after == 0 || after >= 20 {
		t.Fatalf("history has %d messages — the compaction should still have been applied despite the "+
			"notice failing", after)
	}
}

// TestAnUnwiredSinkIsNotAnError pins the nil path: a plane with no Ask service (or a
// bare test bridge) keeps the log line as the only record, which was the behaviour
// before the sink existed.
func TestAnUnwiredSinkIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	b, sid, prov := pressureTestBridge(t, dir, 990, 1000)

	ctx := tenant.WithID(context.Background(), "tnt")
	b.maybeCompactForPressure(ctx, prov, "01PRESSUREPRESSURETEST000000", sid, "orchicon/deepseek/deepseek-flash", "deepseek-flash")

	b.mu.Lock()
	after := len(b.chatHistory[sid])
	b.mu.Unlock()
	if after == 0 || after >= 20 {
		t.Fatalf("history has %d messages — an unwired sink must not stop the gate compacting", after)
	}
}
