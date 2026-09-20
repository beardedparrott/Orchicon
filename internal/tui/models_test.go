package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
)

// The strip's pure pieces: the ratio and the context funnel.

// cacheHitRatio is the share of INPUT tokens served from the cache. The
// denominator is cache_read + uncached input — the two halves the usage
// recorder stores separately — and an input-free session reports NOTHING rather
// than a misleading "0%".
func TestCacheHitRatio(t *testing.T) {
	cases := []struct {
		name      string
		read      int64
		prompt    int64
		want      string
		wantShown bool
	}{
		{"typed prefix hit", 940000, 260000, "78%", true},
		{"full hit", 100, 0, "100%", true},
		{"no cache", 0, 500, "0%", true},
		{"no input at all", 0, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := cacheHitRatio(c.read, c.prompt)
			if ok != c.wantShown {
				t.Fatalf("shown = %v, want %v", ok, c.wantShown)
			}
			if got != c.want {
				t.Errorf("cacheHitRatio(%d, %d) = %q, want %q", c.read, c.prompt, got, c.want)
			}
		})
	}
}

// fmtCtx shows occupancy against the window, and NEVER fabricates a
// denominator when the window is unknown.
func TestFmtCtx(t *testing.T) {
	if got := fmtCtx(124000, 200000); got != "124K/200K" {
		t.Errorf("fmtCtx with a window = %q, want 124K/200K", got)
	}
	if got := fmtCtx(124000, 0); got != "124K" {
		t.Errorf("fmtCtx without a window = %q, want the bare occupancy", got)
	}
}

// metricsLine reports every number the operator asked for, from the open
// conversation's roll-up.
func TestMetricsLineShowsEveryRequestedNumber(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-1"
	m.metrics = sessionMetrics{
		have: true, model: "orchicon/anthropic/claude-sonnet-4",
		ctxUsed: 124000, ctxWindow: 200000,
		tokens: 1200000, cacheRead: 940000, prompt: 260000, costUSD: 1.2345,
	}
	line := m.metricsLine()
	for _, want := range []string{
		"orchicon/anthropic/claude-sonnet-4", // the current ask model, in text
		"ctx 124K/200K",                      // context count against the window
		"1.2M tok",                           // token count
		"cache 78%",                          // cache hit ratio
		"(940K)",                             // cache hit tokens
		"$1.2345",                            // current cost of the session
	} {
		if !strings.Contains(line, want) {
			t.Errorf("metricsLine missing %q:\n%s", want, line)
		}
	}
}

// With no conversation open there is nothing to report, so the strip stays
// empty (it must not print an orphaned model name).
func TestMetricsLineEmptyWithoutAConversation(t *testing.T) {
	m := newTestApp()
	m.metrics = sessionMetrics{have: true, model: "orchicon/a/b"}
	if got := m.metricsLine(); got != "" {
		t.Fatalf("metricsLine with no conversation = %q, want empty", got)
	}
}

// A conversation whose usage has not landed yet still shows the model — the
// strip is never blank while a chat is open.
func TestMetricsLineShowsTheModelBeforeUsageLands(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-1"
	m.metrics = sessionMetrics{model: "orchicon/anthropic/claude-sonnet-4"}
	if got := m.metricsLine(); !strings.Contains(got, "claude-sonnet-4") {
		t.Fatalf("metricsLine = %q, want the model while usage is pending", got)
	}
}

// applyMetrics installs the read AND pushes it into the composer; a read for a
// conversation the operator has left is dropped.
func TestApplyMetricsInstallsAndDropsStale(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-2"

	m.applyMetrics(metricsMsg{convID: "conv-1", m: sessionMetrics{have: true, model: "stale/a/b"}})
	if m.metrics.model == "stale/a/b" {
		t.Fatal("a stale conversation's metrics were installed")
	}

	m.applyMetrics(metricsMsg{convID: "conv-2", m: sessionMetrics{
		have: true, model: "orchicon/anthropic/claude-sonnet-4", ctxWindow: 200000,
	}})
	if m.metrics.model != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("metrics = %+v, want the current conversation's", m.metrics)
	}
	if m.dock.Stats == "" {
		t.Error("the composer's stat strip was not populated")
	}
	if m.dock.Mode == "" {
		t.Error("the composer's mode pill was not populated")
	}
	if m.ctxWindowFor != "orchicon/anthropic/claude-sonnet-4" || m.ctxWindow != 200000 {
		t.Errorf("ctxWindow = (%q, %d), want the RESOLVED window cached", m.ctxWindowFor, m.ctxWindow)
	}
}

// An UNRESOLVED window (0) must not be cached as though it were resolved.
//
// 0 is ambiguous — it means "no context hint" AND "the lookup has not succeeded
// yet" (most often because the read raced the provider list). Caching it as
// resolved froze the missing limit onto the strip for the life of the
// conversation: the composer rendered a bare "ctx 24K" with no window and never
// retried. Leaving it uncached means the next refresh tries again.
func TestUnresolvedContextWindowIsNotCached(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-1"
	const model = "orchicon/opencode/muse-spark-1.3"

	// First read: the window could not be resolved yet.
	m.applyMetrics(metricsMsg{convID: "conv-1", m: sessionMetrics{have: true, model: model, ctxUsed: 24000}})
	if m.ctxWindowFor != "" {
		t.Fatalf("an unresolved window was cached as resolved (ctxWindowFor=%q)", m.ctxWindowFor)
	}

	// A later read resolves it, and NOW it is cached.
	m.applyMetrics(metricsMsg{convID: "conv-1", m: sessionMetrics{
		have: true, model: model, ctxUsed: 24000, ctxWindow: 200000,
	}})
	if m.ctxWindowFor != model || m.ctxWindow != 200000 {
		t.Fatalf("the resolved window was not cached: (%q, %d)", m.ctxWindowFor, m.ctxWindow)
	}
	if got := m.metricsLine(); !strings.Contains(got, "200K") {
		t.Errorf("the strip must show the limit once resolved:\n%s", got)
	}
}

// A failed read keeps the last good numbers: a transient usage error must not
// erase the operator's context.
func TestApplyMetricsKeepsLastGoodNumbersOnError(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-1"
	m.applyMetrics(metricsMsg{convID: "conv-1", m: sessionMetrics{have: true, model: "orchicon/a/b", costUSD: 9}})
	m.applyMetrics(metricsMsg{convID: "conv-1", err: errTestMetrics})
	if m.metrics.costUSD != 9 {
		t.Fatalf("a failed read blanked the strip: %+v", m.metrics)
	}
}

var errTestMetrics = errMetrics("boom")

type errMetrics string

func (e errMetrics) Error() string { return string(e) }

// --- the /models command ----------------------------------------------------

// /models opens the model picker (it does not just print a notice).
func TestSlashModelsOpensThePicker(t *testing.T) {
	m := newTestApp()
	handled, _ := m.dispatchSlash("/models")
	if !handled {
		t.Fatal("/models must be handled as a command")
	}
	if m.modelPicker == nil {
		t.Fatal("/models must open the model picker")
	}
}

// While the picker is open it owns the keyboard: a shell chord must not reach
// the tabs, and esc closes the picker rather than the screen behind it.
func TestModelsPickerOverlayOwnsTheKeyboard(t *testing.T) {
	m := newTestApp()
	m.dispatchSlash("/models")
	if m.modelPicker == nil {
		t.Fatal("precondition: the picker is open")
	}
	before := m.ActiveTab()

	next, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlO})
	if next.ActiveTab() != before {
		t.Fatal("a tab chord reached the shell while the picker owned the keyboard")
	}
	if next.modelPicker == nil {
		t.Fatal("the picker must stay open")
	}

	closed, _ := next.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	if closed.modelPicker != nil {
		t.Fatal("esc must close the picker")
	}
	if closed.ActiveTab() != before {
		t.Fatal("closing the picker must not navigate")
	}
}

// A late picker load must never resurrect a closed modal.
func TestLatePickerLoadAfterCloseIsDropped(t *testing.T) {
	m := newTestApp()
	m.dispatchSlash("/models")
	closed, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	if closed.modelPicker != nil {
		t.Fatal("precondition: the picker is closed")
	}
	if cmd := closed.applyModelKinds(modelpick.KindsMsg{Kinds: []string{"orchicon"}}); cmd != nil {
		t.Fatal("a late load must not produce work")
	}
	if closed.modelPicker != nil {
		t.Fatal("a late load resurrected the picker")
	}
}

// The mode pill reports the conversation's persona; with no conversation it
// reports the pending default (never blank behind a live chat).
func TestCurrentModeLabel(t *testing.T) {
	m := newTestApp()
	if got := m.currentModeLabel(); got != "brainstorm" {
		t.Fatalf("default mode label = %q, want brainstorm (the server default)", got)
	}
}
