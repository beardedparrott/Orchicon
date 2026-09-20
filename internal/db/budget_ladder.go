package db

import (
	"encoding/json"
	"fmt"
)

// BudgetLadder holds the per-tenant execution-budget ladder: the gate
// ceilings and the per-dimension warn → escalate → final threshold + message
// triple that drives the unified escalation ladder in the adapters.
//
// It is the typed, DB-backed source of truth for the ladder. The legacy
// default_budget_overrides jsonb column is treated as the wire/API transport:
// BudgetJSON() serializes these columns to that JSON shape (for GetSettings
// and for dispatch via the scheduler) and ApplyBudgetJSON ingests a client's
// JSON into these columns.
type BudgetLadder struct {
	// Gate ceilings. A nil pointer means "built-in default" (the column is
	// NULL); an explicit 0 disables that gate.
	Tokens          *float64 `json:"-"`
	CostUSD         *float64 `json:"-"`
	ToolCallCount   *float64 `json:"-"`
	WallClockSecs   *float64 `json:"-"`
	CompactMaxTurns *float64 `json:"-"`

	// Per-tier compaction toggles (budget_compact_*_tier): whether that
	// budget-ladder tier ALSO triggers a context compaction, or only injects
	// its warning message. Mirrors the budget JSON `compact_tiers` key that
	// the adapters' parseBudgetSpec honors at dispatch, so the Settings API
	// and per-worker budget overrides stay in sync. Default {warn:false,
	// escalate:true, final:true} — compaction at the earliest tier is OFF
	// because the lossy collapse interrupts the worker mid-flight.
	CompactWarnTier  bool `json:"-"`
	CompactEscalTier bool `json:"-"`
	CompactFinalTier bool `json:"-"`

	// Thresholds (fractions of the limit). Columns are NOT NULL DEFAULT, so
	// these are always populated (0.25 / 0.5 / 0.75 by default).
	WarnFracTokens   float64
	EscFracTokens    float64
	FinalFracTokens  float64
	WarnFracCostUSD  float64
	EscFracCostUSD   float64
	FinalFracCostUSD float64
	WarnFracTools    float64
	EscFracTools     float64
	FinalFracTools   float64
	WarnFracTime     float64
	EscFracTime      float64
	FinalFracTime    float64

	// Messages injected at each stage.
	WarnMsgTokens   string
	EscMsgTokens    string
	FinalMsgTokens  string
	WarnMsgCostUSD  string
	EscMsgCostUSD   string
	FinalMsgCostUSD string
	WarnMsgTools    string
	EscMsgTools     string
	FinalMsgTools   string
	WarnMsgTime     string
	EscMsgTime      string
	FinalMsgTime    string
}

// budgetIsZero reports whether a BudgetLadder carries no meaningful values —
// every gate nil, all tier toggles false, all fractions 0, all messages empty.
// It is used to detect a caller that did not intend to change the budget, so
// the write path can preserve the current ladder instead of clobbering it with
// zeros (see UpdateTenantSettings).
func budgetIsZero(l BudgetLadder) bool {
	if l.Tokens != nil || l.CostUSD != nil || l.ToolCallCount != nil ||
		l.WallClockSecs != nil || l.CompactMaxTurns != nil {
		return false
	}
	if l.CompactWarnTier || l.CompactEscalTier || l.CompactFinalTier {
		return false
	}
	if l.WarnFracTokens != 0 || l.EscFracTokens != 0 || l.FinalFracTokens != 0 ||
		l.WarnFracCostUSD != 0 || l.EscFracCostUSD != 0 || l.FinalFracCostUSD != 0 ||
		l.WarnFracTools != 0 || l.EscFracTools != 0 || l.FinalFracTools != 0 ||
		l.WarnFracTime != 0 || l.EscFracTime != 0 || l.FinalFracTime != 0 {
		return false
	}
	if l.WarnMsgTokens != "" || l.EscMsgTokens != "" || l.FinalMsgTokens != "" ||
		l.WarnMsgCostUSD != "" || l.EscMsgCostUSD != "" || l.FinalMsgCostUSD != "" ||
		l.WarnMsgTools != "" || l.EscMsgTools != "" || l.FinalMsgTools != "" ||
		l.WarnMsgTime != "" || l.EscMsgTime != "" || l.FinalMsgTime != "" {
		return false
	}
	return true
}

// policyIsSet reports whether a TenantSettingsRow carries a compaction or
// memory policy the caller intends to write (D4). An all-zero policy — the
// migration defaults are true/0.8/6/true/5, so every meaningful policy
// disables a feature via an explicit boolean AND still carries a valid
// numeric — means "not part of this update". Used by the write path to
// preserve the current policy on partial updates (see UpdateTenantSettings).
func (r TenantSettingsRow) policyIsSet() bool {
	if r.ContextCompactionEnabled || r.ContextCompactionPressureFrac != 0 || r.ContextRecentTurns != 0 {
		return true
	}
	if r.MemoryEnabled || r.MemoryDigestEntries != 0 {
		return true
	}
	return false
}

// ladderDims maps the four budget dimensions to their JSON key names, in
// the same order parseBudgetSpec reads them.
var ladderDims = []struct {
	jsonKey string
	frac    func(l *BudgetLadder) [3]float64
	msg     func(l *BudgetLadder) [3]string
}{
	{"tokens", func(l *BudgetLadder) [3]float64 {
		return [3]float64{l.WarnFracTokens, l.EscFracTokens, l.FinalFracTokens}
	},
		func(l *BudgetLadder) [3]string { return [3]string{l.WarnMsgTokens, l.EscMsgTokens, l.FinalMsgTokens} }},
	{"cost_usd", func(l *BudgetLadder) [3]float64 {
		return [3]float64{l.WarnFracCostUSD, l.EscFracCostUSD, l.FinalFracCostUSD}
	},
		func(l *BudgetLadder) [3]string {
			return [3]string{l.WarnMsgCostUSD, l.EscMsgCostUSD, l.FinalMsgCostUSD}
		}},
	{"tool_call_count", func(l *BudgetLadder) [3]float64 { return [3]float64{l.WarnFracTools, l.EscFracTools, l.FinalFracTools} },
		func(l *BudgetLadder) [3]string { return [3]string{l.WarnMsgTools, l.EscMsgTools, l.FinalMsgTools} }},
	{"wall_clock_seconds", func(l *BudgetLadder) [3]float64 { return [3]float64{l.WarnFracTime, l.EscFracTime, l.FinalFracTime} },
		func(l *BudgetLadder) [3]string { return [3]string{l.WarnMsgTime, l.EscMsgTime, l.FinalMsgTime} }},
}

// jsonLadder returns the JSON shape of the ladder (the `warnings` block)
// read by the adapter's parseBudgetSpec.
func (l *BudgetLadder) jsonWarnings() map[string]any {
	fracs := map[string]any{}
	msgs := map[string]any{}
	for _, d := range ladderDims {
		fracs[d.jsonKey] = d.frac(l)
		msgs[d.jsonKey] = d.msg(l)
	}
	return map[string]any{"fractions": fracs, "messages": msgs}
}

// BudgetJSON serializes the typed budget columns into the budget JSON
// document the adapters' parseBudgetSpec reads and the Settings API returns
// on the default_budget_overrides string. Gates are emitted only when set
// (NULL = built-in default); the ladder is always emitted (columns are
// NOT NULL DEFAULT).
func (r *TenantSettingsRow) BudgetJSON() []byte {
	out := map[string]any{"warnings": r.Budget.jsonWarnings()}
	if r.Budget.Tokens != nil {
		out["tokens"] = *r.Budget.Tokens
	}
	if r.Budget.CostUSD != nil {
		out["cost_usd"] = *r.Budget.CostUSD
	}
	if r.Budget.ToolCallCount != nil {
		out["tool_call_count"] = *r.Budget.ToolCallCount
	}
	if r.Budget.WallClockSecs != nil {
		out["wall_clock_seconds"] = *r.Budget.WallClockSecs
	}
	if r.Budget.CompactMaxTurns != nil {
		out["compact_max_turns"] = *r.Budget.CompactMaxTurns
	}
	// Per-tier compaction toggles are always emitted (NOT NULL DEFAULT
	// columns), and mergeBudgets layers a worker's override on top.
	out["compact_tiers"] = []bool{r.Budget.CompactWarnTier, r.Budget.CompactEscalTier, r.Budget.CompactFinalTier}
	// Compaction + memory policy (D4): the typed tenant_settings columns
	// serialize as context_compaction / memory objects — the same keys the
	// native session's policyFromSettings and a worker's budget_overrides
	// parse, so mergeBudgets layers worker-over-tenant per key. Only
	// meaningful values are emitted (zero = built-in default) so a fresh
	// tenant's JSON stays minimal.
	out["context_compaction"] = map[string]any{
		"enabled":       r.ContextCompactionEnabled,
		"pressure_frac": r.ContextCompactionPressureFrac,
		"recent_turns":  r.ContextRecentTurns,
	}
	out["memory"] = map[string]any{
		"enabled":        r.MemoryEnabled,
		"digest_entries": r.MemoryDigestEntries,
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Marshal of a flat map of numbers/strings cannot fail.
		return []byte("{}")
	}
	return b
}

// ApplyBudgetJSON merges a client-supplied budget JSON (the same shape the
// legacy default_budget_overrides blob carried) into the typed columns,
// starting from the current columns so a partial update preserves existing
// values. Unknown keys are ignored; gates absent from the JSON leave the
// current value (a caller that wants to clear a gate explicitly sends 0,
// which is a real value that disables that gate).
func (r *TenantSettingsRow) ApplyBudgetJSON(budgets []byte) error {
	if len(budgets) == 0 {
		return nil
	}
	var raw struct {
		Tokens            *float64 `json:"tokens"`
		CostUSD           *float64 `json:"cost_usd"`
		ToolCallCount     *float64 `json:"tool_call_count"`
		WallClockSecs     *float64 `json:"wall_clock_seconds"`
		CompactMaxTurns   *float64 `json:"compact_max_turns"`
		CompactTiers      []bool   `json:"compact_tiers"`
		ContextCompaction *struct {
			Enabled      *bool    `json:"enabled"`
			PressureFrac *float64 `json:"pressure_frac"`
			RecentTurns  *int     `json:"recent_turns"`
		} `json:"context_compaction"`
		Memory *struct {
			Enabled       *bool `json:"enabled"`
			DigestEntries *int  `json:"digest_entries"`
		} `json:"memory"`
		Warnings struct {
			Fractions map[string][3]float64 `json:"fractions"`
			Messages  map[string][3]string  `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(budgets, &raw); err != nil {
		return fmt.Errorf("db: apply budget json: %w", err)
	}

	// Gates: present key is stored as-is (NULL stays NULL → default);
	// absent key keeps the current value (partial update).
	if raw.Tokens != nil {
		v := *raw.Tokens
		r.Budget.Tokens = &v
	}
	if raw.CostUSD != nil {
		v := *raw.CostUSD
		r.Budget.CostUSD = &v
	}
	if raw.ToolCallCount != nil {
		v := *raw.ToolCallCount
		r.Budget.ToolCallCount = &v
	}
	if raw.WallClockSecs != nil {
		v := *raw.WallClockSecs
		r.Budget.WallClockSecs = &v
	}
	if raw.CompactMaxTurns != nil {
		v := *raw.CompactMaxTurns
		r.Budget.CompactMaxTurns = &v
	}
	// Per-tier compaction toggles: a full [warn, escalate, final] bool array.
	// Absent (or not a 3-element array) leaves the current values, so a
	// partial update preserves the existing policy.
	if len(raw.CompactTiers) == 3 {
		r.Budget.CompactWarnTier = raw.CompactTiers[0]
		r.Budget.CompactEscalTier = raw.CompactTiers[1]
		r.Budget.CompactFinalTier = raw.CompactTiers[2]
	}

	// Compaction + memory policy (D4): present sub-keys are ingested (with
	// the same validation policyFromSettings applies — pressure_frac in
	// (0,1], counts > 0); absent sub-keys leave the current value so a
	// partial update never clobbers a stored policy with zeros.
	if cc := raw.ContextCompaction; cc != nil {
		if cc.Enabled != nil {
			r.ContextCompactionEnabled = *cc.Enabled
		}
		if cc.PressureFrac != nil && *cc.PressureFrac > 0 && *cc.PressureFrac <= 1 {
			r.ContextCompactionPressureFrac = *cc.PressureFrac
		}
		if cc.RecentTurns != nil && *cc.RecentTurns > 0 {
			r.ContextRecentTurns = *cc.RecentTurns
		}
	}
	if m := raw.Memory; m != nil {
		if m.Enabled != nil {
			r.MemoryEnabled = *m.Enabled
		}
		if m.DigestEntries != nil && *m.DigestEntries > 0 {
			r.MemoryDigestEntries = *m.DigestEntries
		}
	}

	setFrac := func(dim string, set func(i int, v float64)) {
		f, ok := raw.Warnings.Fractions[dim]
		if !ok {
			return
		}
		for i := 0; i < 3; i++ {
			if f[i] >= 0 && f[i] <= 1 {
				set(i, f[i])
			}
		}
	}
	setMsg := func(dim string, set func(i int, v string)) {
		m, ok := raw.Warnings.Messages[dim]
		if !ok {
			return
		}
		for i := 0; i < 3; i++ {
			set(i, m[i])
		}
	}

	setFrac("tokens", func(i int, v float64) {
		assignFrac(&r.Budget.WarnFracTokens, &r.Budget.EscFracTokens, &r.Budget.FinalFracTokens, i, v)
	})
	setFrac("cost_usd", func(i int, v float64) {
		assignFrac(&r.Budget.WarnFracCostUSD, &r.Budget.EscFracCostUSD, &r.Budget.FinalFracCostUSD, i, v)
	})
	setFrac("tool_call_count", func(i int, v float64) {
		assignFrac(&r.Budget.WarnFracTools, &r.Budget.EscFracTools, &r.Budget.FinalFracTools, i, v)
	})
	setFrac("wall_clock_seconds", func(i int, v float64) {
		assignFrac(&r.Budget.WarnFracTime, &r.Budget.EscFracTime, &r.Budget.FinalFracTime, i, v)
	})

	setMsg("tokens", func(i int, v string) {
		assignMsg(&r.Budget.WarnMsgTokens, &r.Budget.EscMsgTokens, &r.Budget.FinalMsgTokens, i, v)
	})
	setMsg("cost_usd", func(i int, v string) {
		assignMsg(&r.Budget.WarnMsgCostUSD, &r.Budget.EscMsgCostUSD, &r.Budget.FinalMsgCostUSD, i, v)
	})
	setMsg("tool_call_count", func(i int, v string) {
		assignMsg(&r.Budget.WarnMsgTools, &r.Budget.EscMsgTools, &r.Budget.FinalMsgTools, i, v)
	})
	setMsg("wall_clock_seconds", func(i int, v string) {
		assignMsg(&r.Budget.WarnMsgTime, &r.Budget.EscMsgTime, &r.Budget.FinalMsgTime, i, v)
	})

	return nil
}

func assignFrac(warn, esc, final *float64, i int, v float64) {
	switch i {
	case 0:
		*warn = v
	case 1:
		*esc = v
	case 2:
		*final = v
	}
}

func assignMsg(warn, esc, final *string, i int, v string) {
	switch i {
	case 0:
		*warn = v
	case 1:
		*esc = v
	case 2:
		*final = v
	}
}
