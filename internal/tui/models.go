// models.go — the /models command, the model picker it opens, and the
// composer's session stat strip (context / tokens / cache / cost).
//
// The picker itself is kit2.ModelPicker and the per-adapter data cascade is
// internal/tui/modelpick — both shared with the Control (tenant defaults) and
// Execution (worker model) screens, so all four surfaces choose a model the
// same way. This file owns the SHELL's copy: the App hosts the modal (the
// composer is the shell, not a screen), and the metrics read that feeds the
// stat strip.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// sessionMetrics is the OPEN conversation's usage roll-up — the numbers the
// composer's bottom-right stat strip shows.
type sessionMetrics struct {
	have      bool
	model     string
	ctxUsed   int64 // input tokens on the LATEST turn (context occupancy)
	ctxWindow int64 // the model's window (0 = unknown)
	tokens    int64 // session total (all turns)
	cacheRead int64
	prompt    int64
	costUSD   float64
}

// metricsMsg carries a finished metrics read.
type metricsMsg struct {
	convID string
	m      sessionMetrics
	err    error
}

// --- /models ----------------------------------------------------------------

// openModelsPicker opens the three-tier model picker seeded from the CURRENT
// conversation's model_ref (the operator's ask: "/models ... would set the ask
// orchicon model ... the current conversations model_ref").
func (m *App) openModelsPicker() tea.Cmd {
	subject := "new conversation"
	if m.chatConvID != "" {
		subject = m.chatConvID
	}
	mp := kit2.NewModelPicker("Ask model — " + subject)
	mp.PreferredAdapter = modelpick.NativeAdapterKind
	mp.SetScreen(m.width, m.height)
	mp.LoadAdapters = m.loadModelKinds
	mp.LoadProviders = m.loadModelProviders
	mp.LoadModels = m.loadModelModels
	m.modelPicker = mp
	mp.Commit = func(string) { m.modelPicker = nil }
	// The commit both closes the modal and issues the WRITE, so the chosen ref
	// is persisted without kit2 knowing anything about conversations.
	mp.CommitCmd = func(ref string) tea.Cmd { return m.commitAskModel(ref) }
	mp.Cancel = func() { m.modelPicker = nil }
	kind, provider, model := modelpick.SplitRef(m.currentAskModel())
	return mp.Open(kind, provider, model)
}

// commitAskModel writes the chosen ref. With a conversation OPEN it retargets
// THAT conversation (SetConversationModel — the change applies from the next
// message). With none open it records the ref the next conversation is created
// with, so /models is never a dead end.
func (m *App) commitAskModel(ref string) tea.Cmd {
	if m.chatConvID != "" {
		m.dock.SetError("")
		m.dock.SetNotice("ask model → " + ref + " (applies from the next message)")
		return m.chat.SetConversationModel(m.chatConvID, ref)
	}
	m.chat.SetPendingModel(ref)
	m.dock.SetNotice("ask model → " + ref + " (applies to the next new conversation)")
	return nil
}

// currentAskModel is the ref the composer reports and the picker seeds from:
// the open conversation's, else the pending one a new conversation will use.
func (m *App) currentAskModel() string {
	if m.chat == nil {
		return ""
	}
	if m.chatConvID != "" {
		for _, c := range m.chat.Conversations() {
			if c.ID == m.chatConvID && c.ModelRef != "" {
				return c.ModelRef
			}
		}
	}
	return m.chat.PendingModel()
}

// currentModeLabel is the persona the composer reports (the GUI renders this
// as its mode dropdown; the TUI's equivalent is the pill on the stat row).
func (m *App) currentModeLabel() string {
	if m.chat == nil {
		return ""
	}
	mode := m.chat.PendingMode()
	if m.chatConvID != "" {
		for _, c := range m.chat.Conversations() {
			if c.ID == m.chatConvID {
				mode = c.Mode
			}
		}
	}
	switch mode {
	case apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM:
		return "brainstorm"
	case apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED:
		return "brainstorm" // the server default
	default:
		return strings.ToLower(mode.String())
	}
}

// --- picker loads (shared cascade) ------------------------------------------

func (m *App) loadModelKinds() tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		kinds, ask, err := modelpick.FetchKinds(ctx, cl)
		return modelpick.KindsMsg{Kinds: kinds, AskCapable: ask, Err: err}
	}
}

func (m *App) loadModelProviders(kind string) tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		opts, err := modelpick.FetchProviders(ctx, cl, kind)
		return modelpick.ProvidersMsg{Adapter: kind, Opts: opts, Err: err}
	}
}

func (m *App) loadModelModels(kind, provider string) tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		// CLI discovery shells out, so it gets a longer budget than a read.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		opts, degraded, err := modelpick.FetchModels(ctx, cl, kind, provider)
		return modelpick.ModelsMsg{Adapter: kind, Provider: provider, Opts: opts, Degraded: degraded, Err: err}
	}
}

func (m *App) applyModelKinds(msg modelpick.KindsMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil // a late load must never resurrect a closed picker
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetAdapters(msg.Kinds, msg.AskCapable)
	return m.modelPicker.Sync()
}

func (m *App) applyModelProviders(msg modelpick.ProvidersMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetProviders(msg.Adapter, msg.Opts)
	return m.modelPicker.Sync()
}

func (m *App) applyModelModels(msg modelpick.ModelsMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetModels(msg.Adapter, msg.Provider, msg.Opts, msg.Degraded)
	return nil
}

// --- session metrics --------------------------------------------------------

// refreshMetrics re-reads the open conversation's usage roll-up: the session
// totals, cache counters and cost come from GetUsage scoped by session_id
// (the read-back path added for exactly this), and the model's context window
// is resolved once per model and cached.
//
// It is called on conversation open/switch and on every turn completion, so
// the strip updates LIVE as the conversation progresses.
func (m *App) refreshMetrics() tea.Cmd {
	convID := m.chatConvID
	if convID == "" {
		m.metrics = sessionMetrics{}
		m.syncComposerStats()
		return nil
	}
	cl := m.clients
	model := m.currentAskModel()
	cachedWindow := int64(0)
	needWindow := false
	if model != "" {
		// Only a NON-ZERO window counts as resolved. Caching 0 treated "the read
		// raced the provider list, which had not loaded yet" as "this model has no
		// context window", so the lookup was never retried and the composer showed
		// a bare token count with no limit for the life of the conversation.
		if m.ctxWindowFor == model && m.ctxWindow > 0 {
			cachedWindow = m.ctxWindow
		} else {
			needWindow = true
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out := sessionMetrics{have: true, model: model, ctxWindow: cachedWindow}
		if cl != nil && cl.AIGateway != nil {
			resp, err := cl.AIGateway.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{
				SessionId: convID,
				PageSize:  200,
			}))
			if err != nil {
				return metricsMsg{convID: convID, err: err}
			}
			// Records are newest-first, so the FIRST record's input side is the
			// best available proxy for the conversation's CURRENT context
			// occupancy (the sum across turns is not the context size).
			for i, r := range resp.Msg.GetRecords() {
				out.tokens += r.GetTotalTokens()
				out.cacheRead += r.GetCacheReadTokens()
				out.prompt += r.GetPromptTokens()
				out.costUSD += r.GetCostUsd()
				if i == 0 {
					out.ctxUsed = r.GetPromptTokens() + r.GetCacheReadTokens() + r.GetCacheWriteTokens()
				}
			}
		}
		if needWindow && model != "" {
			if w, err := modelpick.ContextWindow(ctx, cl, model); err == nil {
				out.ctxWindow = w
			}
		}
		return metricsMsg{convID: convID, m: out}
	}
}

// applyMetrics installs a finished read, dropping a result that belongs to a
// conversation the operator has already left.
func (m *App) applyMetrics(msg metricsMsg) tea.Cmd {
	if msg.convID != m.chatConvID {
		return nil // stale: the operator moved on
	}
	if msg.err != nil {
		// Keep the last good numbers rather than blanking the strip — a
		// transient usage-read failure must not erase the operator's context.
		return nil
	}
	m.metrics = msg.m
	if msg.m.ctxWindow > 0 {
		m.ctxWindowFor, m.ctxWindow = msg.m.model, msg.m.ctxWindow
	} else {
		// 0 means UNRESOLVED, not "no window": leave it un-cached so the next
		// refresh retries (the provider list may still be loading) instead of
		// freezing a missing context limit onto the strip.
		m.ctxWindowFor = ""
	}
	m.syncComposerStats()
	return nil
}

// syncComposerStats pushes the stat strip and the mode pill into the composer.
func (m *App) syncComposerStats() {
	// The MODEL goes in its own field (rendered left) rather than being prefixed
	// to the stats: a long ref pushed the right-aligned numbers past the pane
	// edge and clipped the cost.
	m.dock.Model = m.metricsModel()
	m.dock.Stats = m.metricsStats()
	m.dock.Mode = m.currentModeLabel()
}

// metricsModel is the ask model ref for the strip's left side ("" when unknown).
func (m *App) metricsModel() string {
	if m.chatConvID == "" {
		return ""
	}
	return m.metrics.model
}

// metricsStats is the NUMERIC half of the strip (no model) — the right side.
func (m *App) metricsStats() string {
	if m.chatConvID == "" {
		return ""
	}
	mx := m.metrics
	if !mx.have {
		return ""
	}
	segs := make([]string, 0, 4)
	segs = append(segs, "ctx "+fmtCtx(mx.ctxUsed, mx.ctxWindow))
	segs = append(segs, modelpick.FmtTokens(mx.tokens)+" tok")
	if ratio, ok := cacheHitRatio(mx.cacheRead, mx.prompt); ok {
		segs = append(segs, "cache "+ratio+" ("+modelpick.FmtTokens(mx.cacheRead)+")")
	}
	segs = append(segs, fmtCost(mx.costUSD))
	return strings.Join(segs, " · ")
}

// metricsLine renders the composer's bottom-right stat strip: the ask model,
// the context occupancy against the model's window, the session token total,
// the cache-hit ratio with the cached token count, and the session cost.
func (m *App) metricsLine() string {
	if m.chatConvID == "" {
		return ""
	}
	mx := m.metrics
	segs := make([]string, 0, 5)
	if mx.model != "" {
		segs = append(segs, mx.model)
	}
	if !mx.have {
		return strings.Join(segs, " · ")
	}
	segs = append(segs, "ctx "+fmtCtx(mx.ctxUsed, mx.ctxWindow))
	segs = append(segs, modelpick.FmtTokens(mx.tokens)+" tok")
	if ratio, ok := cacheHitRatio(mx.cacheRead, mx.prompt); ok {
		segs = append(segs, "cache "+ratio+" ("+modelpick.FmtTokens(mx.cacheRead)+")")
	}
	segs = append(segs, fmtCost(mx.costUSD))
	return strings.Join(segs, " · ")
}

// cacheHitRatio is the share of INPUT tokens served from the prompt cache:
// cache_read / (cache_read + uncached input). The usage recorder stores the
// cached reads and the plain input separately (opencode's tokens.cache.read vs
// tokens.input), so the sum is the true input side. ok=false when there is no
// input at all, so the strip never prints a meaningless "0%".
func cacheHitRatio(cacheRead, prompt int64) (string, bool) {
	denom := cacheRead + prompt
	if denom <= 0 {
		return "", false
	}
	pct := float64(cacheRead) / float64(denom) * 100
	return fmt.Sprintf("%.0f%%", pct), true
}

// fmtCtx renders context occupancy against the window, degrading to the bare
// occupancy when the window is unknown (never a fabricated denominator).
func fmtCtx(used, window int64) string {
	if window > 0 {
		return modelpick.FmtTokens(used) + "/" + modelpick.FmtTokens(window)
	}
	return modelpick.FmtTokens(used)
}

// fmtCost renders the session cost at the precision the usage records carry.
func fmtCost(usd float64) string {
	return fmt.Sprintf("$%.4f", usd)
}
