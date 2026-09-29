package claude

// compact.go is the Claude-native context compaction + budget-ladder
// enforcement for the claude bridge.
//
// Claude Code has NO in-container serve (servePortFor("claude") == 0, and
// claude is a MOUNT-only boot-profile entry — never a serve-dependent kind),
// so there is no summarize endpoint to call and no HTTP client here. The
// compact is NATIVE: one directive turn written onto the same long-lived
// stdio session, which makes the session summarize its own working context
// in place (Claude Code's built-in auto-compact) while preserving the
// goal/acceptance-criteria scope. It is the same soft, lossy compact the
// opencode adapter's `summarize` produces, and it stays reversible: an
// unfinished turn remains resumable with `--resume <session-id>`.
//
// ONE SOURCE OF TRUTH: every threshold and every message comes from the
// SHARED budget ladder the opencode adapter parses out of the SAME merged
// budget JSON (ExecutionManifest.Budgets → opencode.ParseBudgetLadder, i.e.
// tenant defaults merged with the worker's budget_overrides). This file
// holds NO threshold and NO ladder message text of its own — the
// warn/escalate/final copy and the compact policy are read from the ladder,
// so opencode and claude share the ladder AND the text, editable in
// Settings → Defaults.

import (
	"context"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// budgetDims are the spend dimensions the claude bridge evaluates on the
// shared ladder. Both grow with RE-SENT context — fresh tokens and the
// cache-aware priced cost (claude reports total_cost_usd for the turn,
// which prices cache reads at the provider's real cache rate) — which is
// exactly what a compaction can relieve. tool_call_count / wall_clock are
// deliberately not gated here: claude's ladder-driven abort is about
// runaway cache-cost / transcript growth.
var budgetDims = []string{"tokens", "cost_usd"}

// observeTurnBudget folds one completed turn's LIVE provider-reported usage
// into the shared spend accumulator and evaluates the ladder. It returns a
// non-empty TERMINAL reason ("budget_abort:<dim>") when the abort tier has
// been reached — the caller fails the execution (opencode parity: the
// ladder ceiling is terminal; only the COMPACT action is best-effort).
//
// The cost is CACHE-AWARE: it is the provider's priced figure for the turn
// (cache reads at the cache rate, included), and the cache-read tokens ride
// along so the token dimension stays fresh-only exactly like opencode's.
func (s *session) observeTurnBudget(ctx context.Context) string {
	s.mu.Lock()
	s.step++
	s.mu.Unlock()
	if usage, cost, ok := s.mapper.turnUsageInfo(); ok {
		s.spend.AddFromUsage(usage.InputTokens, usage.OutputTokens, 0, usage.CacheReadTokens, cost)
	}
	return s.maybeEnforceLadder(ctx)
}

// maybeEnforceLadder walks the shared ladder for this turn's accumulated
// spend: the abort tier is terminal, and each warn/escalate/final tier
// injects the ladder's OWN message (read from the merged budget JSON) and —
// when the tier AND the dimension permit it — compacts.
func (s *session) maybeEnforceLadder(ctx context.Context) string {
	if s.ladder == nil {
		return ""
	}
	for _, dim := range budgetDims {
		frac := s.spend.Fraction(s.ladder, dim, time.Since(s.startedAt), 0)
		if frac < 0 {
			continue // the dimension has no effective limit
		}
		lvl := s.ladder.LevelName(dim, frac)
		switch lvl {
		case "none":
			continue
		case "abort":
			// Terminal, and NOT a compaction (opencode budget_abort parity).
			return "budget_abort:" + dim
		}
		if s.latchTier(dim, lvl) {
			continue // one firing per tier per dimension
		}
		if msg := s.ladder.Message(dim, frac); msg != "" {
			s.injectLadderMessage(ctx, msg)
		}
		if s.ladder.CompactsAt(lvl) && s.ladder.CompactsDim(dim) {
			s.doCompact(ctx, "budget:"+dim+":"+lvl)
		}
	}
	return ""
}

// injectLadderMessage delivers one ladder warning to the live session. The
// text is the ladder's own (never a literal here), and it rides the same
// single write path as every other user turn.
func (s *session) injectLadderMessage(ctx context.Context, msg string) {
	if err := s.SendTurn(ctx, msg); err != nil {
		s.b.log.Warn("claude: budget warning injection failed", "execution", s.execID, "error", err)
	}
}

// latchTier reports whether this (dimension, tier) pair already fired, and
// latches it. The ladder is cumulative, so the latch is what keeps a
// breached tier from re-firing its message every turn.
func (s *session) latchTier(dim, lvl string) bool {
	key := dim + ":" + lvl
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tierLatch == nil {
		s.tierLatch = map[string]bool{}
	}
	if s.tierLatch[key] {
		return true
	}
	s.tierLatch[key] = true
	return false
}

// maybeTurnCountCompact applies the orthogonal turn-count context-hygiene
// gate (compact_max_turns, read from the SAME merged budget JSON): a chatty
// session is compacted periodically even when no budget dimension breached,
// because every turn re-sends the accumulated transcript. <= 0 disables it.
func (s *session) maybeTurnCountCompact(ctx context.Context) {
	if s.ladder == nil {
		return
	}
	maxTurns, ok := s.ladder.CompactMaxTurns()
	if !ok || maxTurns <= 0 {
		return
	}
	s.mu.Lock()
	turns := s.step
	if s.lastCompactStep > 0 {
		turns = s.step - s.lastCompactStep
	}
	s.mu.Unlock()
	if turns >= maxTurns {
		s.doCompact(ctx, "turn_count")
	}
}

// doCompact fires the Claude-native compact subject to the shared guards.
// It is best-effort and never terminal (opencode parity): a refused or
// failed compaction logs and the session continues.
func (s *session) doCompact(ctx context.Context, reason string) bool {
	if err := s.armCompact(); err != nil {
		s.b.log.Debug("claude: compaction not armed", "execution", s.execID, "reason", reason, "error", err)
		return false
	}
	return s.writeCompactDirective(ctx, reason, s.remainingScope()) == nil
}

// armCompact applies every shared compaction guard and latches the
// compaction when it passes:
//   - never at session start (the ladder's min-turn floor),
//   - at most ONCE per step,
//   - the min-turn floor between two compactions,
//   - the per-execution cap.
func (s *session) armCompact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return errNoLiveSessionForCompact(s.execID)
	}
	if s.ladder == nil {
		return errCompactUnsupported(s.execID, "no budget ladder was resolved for this execution")
	}
	floor := s.ladder.CompactionTurnFloor()
	if s.step < floor {
		return errCompactNotReady(s.execID, "never at session start (min-turn floor)")
	}
	if s.lastCompactStep == s.step {
		return errCompactNotReady(s.execID, "already compacted once on this step")
	}
	if s.lastCompactStep > 0 && s.step-s.lastCompactStep < floor {
		return errCompactNotReady(s.execID, "min-turn floor between compactions")
	}
	if maxC := s.ladder.CompactionMax(); maxC <= 0 || s.compactions >= maxC {
		return errCompactNotReady(s.execID, "per-execution compaction cap reached")
	}
	s.lastCompactStep = s.step
	s.compactions++
	return nil
}

// writeCompactDirective writes the compact directive turn and records the
// compaction in the durable transcript.
func (s *session) writeCompactDirective(ctx context.Context, reason, scope string) error {
	if err := s.SendTurn(ctx, s.compactDirectiveText(scope)); err != nil {
		s.b.log.Warn("claude: compact directive write failed", "execution", s.execID, "reason", reason, "error", err)
		return err
	}
	provider, model := s.mapper.providerModel()
	s.recordPart(ctx, db.SessionPartCompacted, map[string]any{
		"step":     s.stepSnapshot(),
		"reason":   reason,
		"provider": provider,
		"model":    model,
		"scope":    scope,
	})
	return nil
}

// Compact performs the Claude-native context compaction on behalf of the
// bridge/session API: the same soft, lossy compact the opencode adapter's
// summarize performs, implemented as an in-session directive turn because
// claude has no in-container serve to call. provider/model are the resolved
// identity recorded on the compaction part; remainingScope directs what must
// survive the collapse (empty = this session's own goal + AC).
func (s *session) Compact(ctx context.Context, provider, model, remainingScope string) error {
	scope := strings.TrimSpace(remainingScope)
	if scope == "" {
		scope = s.remainingScope()
	}
	if err := s.armCompact(); err != nil {
		return err
	}
	if err := s.writeCompactDirective(ctx, "bridge:"+provider+"/"+model, scope); err != nil {
		// The session IS live (armCompact armed it): the directive write is
		// what failed, so the true cause rides along rather than being
		// flattened into a "no live session" diagnosis.
		return errCompactWriteFailed(s.execID, err)
	}
	return nil
}

// compactDirectiveText is the Claude-native compact directive: one user turn
// that makes the session summarize its own working context in place
// (Claude Code's own auto-compact) and continue in the SAME session
// afterwards, so the collapse stays soft, lossy and reversible. The
// remaining scope is directed through so the goal and the acceptance
// criteria survive the compact.
func (s *session) compactDirectiveText(remainingScope string) string {
	var b strings.Builder
	b.WriteString("CONTEXT COMPACTION — compact your working context now.\n")
	b.WriteString("Summarize in this session what you have established, what is still open, and the exact next steps; keep the goal and acceptance criteria below verbatim; drop stale intermediate detail (superseded reads, abandoned attempts, verbose tool output). Then CONTINUE the task in this same session — do not start over and do not re-derive state that is settled.\n")
	if scope := strings.TrimSpace(remainingScope); scope != "" {
		b.WriteString("\n--- MUST SURVIVE THE COMPACT ---\n")
		b.WriteString(scope)
		b.WriteString("\n")
	}
	return b.String()
}

// remainingScope composes the goal + acceptance criteria the compact must
// preserve, straight from the execution manifest (the same text the initial
// turn carried).
func (s *session) remainingScope() string {
	var b strings.Builder
	if g := strings.TrimSpace(s.manifest.Goal); g != "" {
		b.WriteString("GOAL:\n" + g + "\n")
	}
	if a := strings.TrimSpace(s.manifest.AcceptanceCriteria); a != "" {
		b.WriteString("\nACCEPTANCE CRITERIA:\n" + a + "\n")
	}
	return strings.TrimSpace(b.String())
}

// stepSnapshot reads the completed-turn counter under the session lock.
func (s *session) stepSnapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.step
}

// compactionsAtStep reports how many compactions have fired for this
// execution (test/diagnostic surface).
func (s *session) compactionsSoFar() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactions
}

// ladderFor is a small accessor used by the tests to assert the ladder was
// parsed from the SAME merged budget JSON the opencode adapter reads.
func (s *session) ladderFor() *opencode.BudgetLadder { return s.ladder }

// spendFor exposes the shared spend accumulator (test/telemetry surface).
func (s *session) spendFor() *opencode.BudgetSpend { return s.spend }
