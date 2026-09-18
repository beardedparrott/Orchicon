package tui

// refresh.go — the ROLLING REFRESH WINDOW.
//
// The operator: "The follow-up prompt is there and works, however, when you send a message, nothing
// happens unless you move away from the execution and then come back. Then you can see your user
// message and the model's response. We need this to be refreshing every few seconds. That also
// brings up a good point about all of the other screens. Are we currently implementing a rolling
// refresh window? If not we should do that. Refresh every few seconds or if change has occurred (i.e.
// a work item is deleted or added)."
//
// THE ANSWER WAS NO. Before this file the TUI had exactly ONE timer in it — the clipboard toast. The
// only thing that could repaint a pane was an event POKE from a NATS stream, and only five screens
// even subscribed (execution, work, enforcement, overview, diffs). Everything else was refreshed by
// an explicit `r`.
//
// WHY POKES ALONE ARE NOT ENOUGH, using the reported case as the example: a follow-up's reply is
// written to the DURABLE transcript, and the live `execution.text` events that would poke the pane
// come from the TaskReconciler's dispatch callbacks — the follow-up path does not go through them. So
// nothing poked, nothing repainted, and the reply stayed invisible until the operator left the screen
// and came back, which re-fetched the durable side. A timer does not care which server path wrote the
// data: it re-reads, and the change appears.
//
// SO THERE ARE TWO TRIGGERS, and they are complementary rather than redundant:
//
//	EVENT POKE  — instant, but only for what the server chooses to emit, and only where a stream is
//	              subscribed. Kept as-is: it is what makes a running execution feel live.
//	REFRESH TICK — every few seconds, for everything, regardless of what the server emits. This is
//	              the safety net that makes "the data is there but the screen does not know" the
//	              operator's problem no longer.
//
// The tick is deliberately CHEAP and deliberately POLITE:
//   - it refreshes only the ACTIVE tab's VISIBLE source (plus the open detail), not every screen and
//     not every source — one list and one detail per tick, so a rolling window cannot become the
//     load problem the executions page just had;
//   - it does nothing while an editor or modal is open, because repainting under a form the operator
//     is typing into is worse than a stale row;
//   - it preserves the cursor, the filter and the scroll offset (kit2.Table.SetItems re-seats the
//     cursor BY ID, which is what makes a periodic reload invisible when nothing changed);
//   - it re-arms itself, so it is a window rather than a one-shot.

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// refreshInterval is the rolling window's period. It matches the GUI's own cadence (its list queries
// poll at `refetchInterval: 5_000`), so the two clients show the same freshness — an operator moving
// between them should not be able to tell which one refreshes faster.
const refreshInterval = 5 * time.Second

// refreshTickMsg is the rolling window's timer. It carries the GENERATION it belongs to, so a tick
// armed before a tab switch cannot refresh the tab the operator has since moved to (and a stale tick
// cannot re-arm a second chain, which would double the refresh rate on every switch).
type refreshTickMsg struct{ gen uint64 }

// Refresher is the optional screen hook the shell calls on every tick.
//
// A screen returns the command that re-reads what is ON SCREEN, and returns nil when a refresh would
// be disruptive (a form is open) or when there is nothing to re-read. The POLICY lives with the
// screen because only it knows what it is in the middle of; the shell only knows when to ask.
type Refresher interface {
	RefreshView() tea.Cmd
}

// refreshCmd arms the next tick for a generation.
func refreshCmd(gen uint64) tea.Cmd {
	return tea.Tick(refreshPeriod, func(time.Time) tea.Msg { return refreshTickMsg{gen: gen} })
}

// handleRefreshTick runs one turn of the rolling window and re-arms it.
//
// THERE IS EXACTLY ONE CHAIN, armed once at Init and re-armed HERE, always with the CURRENT
// generation. That is simpler than restarting a chain on every tab switch and it is strictly
// correct: a single chain cannot multiply, and because it is generation-agnostic at re-arm time it
// needs no restart to follow the operator. The generation is still carried on the message so a tick
// armed BEFORE a switch (by a previous incarnation, or a test) is ignored rather than acted on —
// refreshing the tab the operator has just left would be wasted work and a visible flicker.
func (m *App) handleRefreshTick(msg refreshTickMsg) (*App, tea.Cmd) {
	if msg.gen != m.refreshGen {
		return m, refreshCmd(m.refreshGen) // adopt the live generation: exactly one chain survives
	}
	cmd := m.refreshActiveView()
	// Re-arm unconditionally, even when the refresh did nothing: a skipped tick must not end the
	// window, or one open form would stop every screen refreshing for the rest of the session.
	return m, tea.Batch(cmd, refreshCmd(m.refreshGen))
}

// refreshActiveView asks the active screen to re-read what is on screen. It is the tick's whole
// payload, and the only place the shell reaches into a screen for the refresh.
func (m *App) refreshActiveView() tea.Cmd {
	s := m.screens[m.active]
	if s == nil {
		return nil
	}
	// A MODAL OWNS THE SCREEN. The shell's own overlays (the model picker, the help overlay, the
	// connect form, a slash palette) are typing surfaces: refreshing underneath them would move rows
	// the operator is choosing from.
	if m.refreshBlocked() {
		return nil
	}
	// THE ASK TRANSCRIPT IS A REFRESH TARGET TOO, and it was the ONE surface this window never touched.
	//
	// Every other screen re-reads itself through the Refresher hook below, which reloads a LIST and the
	// open DETAIL from the server. Ask cannot: its transcript is not a server read on a timer — it is a
	// MERGE of the durable conversation and the live chunks the shell has already accumulated, painted by
	// onChatWake. So there was no hook to implement, and the surface was skipped.
	//
	// WHAT THAT COST. Liveness on this pane rested ENTIRELY on the wake chain: a stream chunk calls
	// AppendLiveItem, which pokes a CAP-1 channel with a NON-BLOCKING send, and the waiter that drains it
	// is re-armed only from appMsg's own branches. One missed poke (a full channel, a waiter that was
	// never re-armed on some path) therefore does not degrade — it stops, silently and permanently, and
	// the only thing left that repaints is a full reload: opening the conversation again or clicking away
	// and back. That is the operator's report exactly:
	//
	//	"The conversations in the TUI are NOT truly live. I have to click away and back again to see
	//	 updates. No 'Orchicon is thinking...' block. No reasoning block. No live thinking text block or
	//	 live update when responses happen."
	//
	// This tick is the same safety net the rest of the TUI already has, applied to the one surface that
	// was missing it. It is cheap (a cache merge, no RPC), it cannot multiply (one chain), and it makes
	// "the poke was lost" a five-second delay instead of a dead pane — which is the difference between a
	// feature that feels live and one that feels broken.
	//
	// IT IS ADDITIVE, NOT A REPLACEMENT. The Ask TAB also carries a real screen — the one whose rail
	// lists the conversations — so its own Refresher still has work to do (re-reading that list). Both run,
	// as a batch.
	//
	// AND THAT SCREEN'S Refresher IS NOT kit2.Base's, deliberately: the Ask screen overrides RefreshView to
	// reload the LIST ONLY, because Base's default also re-requests the open detail — and for a
	// conversation that landing carries an EMPTY body (ask.detail returns "" by design; the shell paints
	// the transcript), which SETS the pane's body and erases the transcript onChatWake just painted. The
	// operator: "the conversation pane is completely blank on every chat." See ask.Model.RefreshView.
	if m.active == TabAsk {
		if r, ok := s.(Refresher); ok {
			return tea.Batch(m.onChatWake(), r.RefreshView())
		}
		return m.onChatWake()
	}
	if r, ok := s.(Refresher); ok {
		return r.RefreshView()
	}
	return nil
}

// refreshBlocked reports whether the shell is in a state where a background refresh would be rude:
// a modal or form is up, and the operator is interacting with it rather than with the list.
func (m *App) refreshBlocked() bool {
	switch {
	case m.help.open:
		return true
	case m.palette.connectOpen:
		return true
	case m.palette.PaletteOpen():
		return true
	case m.modelPicker != nil:
		return true
	}
	return false
}

// refreshPeriod is the window's live period. It is a VARIABLE only so a test can shorten it: a test
// that had to wait the real five seconds would make the suite slower than the feature is worth, and
// the timing is not what is under test (the re-arm, the generation and the guards are).
var refreshPeriod = refreshInterval
