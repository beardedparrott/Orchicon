package execution

// execution_context.go — the CONTEXT / token / cost section of the Executions detail.
//
// The operator: "there is no context, token, or cost information being shown."
//
// Why the row's own columns were not enough, and what this reads instead:
//
// `GetExecution` DOES enrich token/cost totals (internal/execution/service.go's enrichUsageTotals
// sums usage_records into WorkerExecution.token_usage/cost_usd), so those two fields were correct.
// They were being BLANKED, not missing — a live session repaint replaced the whole body with a
// four-field stub, which is the composition defect in execution_detail.go. That is fixed; the
// totals now survive.
//
// What genuinely had no source here is the GUI's "Context" panel, and the GUI is explicit about
// where its numbers come from (frontend/src/components/executions/ExecutionContextSidebar.tsx):
//
//   - exec.tokenUsage/costUsd are "served from the usage-records sum (the row columns are
//     write-never)" — so usage_records is the authority, not the row.
//   - usage[] (useGetUsage({executionId})) carries the per-turn breakdown: prompt / completion /
//     reasoning / cache read, plus provider+model.
//   - The CONTEXT PERCENT is measured against the model's real window, and the numerator is the
//     PEAK single-step FRESH token count (prompt+completion+reasoning, cache EXCLUDED) —
//     "cache reads are re-sends of already-counted tokens".
//
// That is what this reproduces. The denominator comes from modelpick.ContextWindow, the same
// resolver the model picker uses, so the TUI does not carry a second, drifting table of windows.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// execUsageMsg carries one execution's usage records and the context window for its model.
type execUsageMsg struct {
	execID string
	usage  *execUsage
	err    error
}

// execUsage is one execution's context picture, derived from its usage records.
type execUsage struct {
	provider string
	model    string
	// workingSet is the PEAK single-step FRESH token count across the execution's turns:
	// prompt + completion + reasoning, cache EXCLUDED. It is the numerator of the context
	// percentage (the GUI's own definition) because it is the largest amount of context the
	// model was actually holding at once, rather than the sum across turns (which grows without
	// bound and would read as "5000% of context" on a long run).
	workingSet int64
	// window is the model's real context window; 0 when it could not be resolved, in which case
	// the percentage is omitted rather than guessed at.
	window int64
	// The summed buckets, for the breakdown line.
	prompt     int64
	completion int64
	reasoning  int64
	cacheRead  int64
	total      int64
	costUSD    float64
	turns      int
}

// percent is the context occupancy against the model's window, or -1 when the window is unknown.
func (u *execUsage) percent() int {
	if u == nil || u.window <= 0 || u.workingSet <= 0 {
		return -1
	}
	// Rounded, and clamped: a real run can exceed its nominal window (the provider truncates, the
	// accounting still counts), and "103%" is a truer sentence than "3% of a full bar".
	p := int((u.workingSet*100 + u.window/2) / u.window)
	if p < 0 {
		p = 0
	}
	return p
}

// loadExecutionUsage reads an execution's usage records and resolves the model's context window.
//
// Best effort end to end: a failed read leaves whatever is cached and never turns "open an
// execution" into an error, because the context panel is context, not the subject.
func (m *Model) loadExecutionUsage(execID string) tea.Cmd {
	if m.cl == nil || m.cl.AIGateway == nil || execID == "" {
		return nil
	}
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := cl.AIGateway.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{
			ExecutionId: execID,
			PageSize:    500,
		}))
		if err != nil {
			return execUsageMsg{execID: execID, err: err}
		}
		u := summarizeUsage(resp.Msg.GetRecords())
		if u != nil && u.provider != "" && u.model != "" {
			// The window is resolved through the model picker's own resolver, keyed by "provider/model"
			// — the shape a usage record carries and a shape ParseModelRef accepts. A miss leaves
			// window at 0 and the percentage is simply not drawn.
			if w, err := modelpick.ContextWindow(ctx, cl, u.provider+"/"+u.model); err == nil && w > 0 {
				u.window = w
			}
		}
		return execUsageMsg{execID: execID, usage: u}
	}
}

// usageCache holds each execution's context picture, and when it was fetched. The stamp is what
// lets the panel be drawn on every paint while the fetch happens on a CLOCK rather than on its own
// arrival — the same rule the todo cache follows, and for the same reason (a fetch gated on its own
// landing is a loop; see execution_detail.go's todosRefreshCmd).
type usageCache struct {
	mu sync.Mutex
	m  map[string]*execUsage
	at map[string]time.Time
}

func (c *usageCache) put(id string, u *execUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*execUsage{}
		c.at = map[string]time.Time{}
	}
	c.m[id] = u
	c.at[id] = time.Now()
}

func (c *usageCache) get(id string) *execUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id]
}

// usageRefreshCmd returns the usage fetch for an execution WHEN its cache is stale, and nil
// otherwise.
//
// A LONGER TTL than the todo list on purpose: a todo list is rewritten between a worker's turns
// while a run is live, whereas usage records accumulate per model turn and the panel's numbers are
// aggregates. Re-reading them twice a second would cost an RPC per repaint to redraw almost the
// same text.
func (m *Model) usageRefreshCmd(execID string) tea.Cmd {
	if m.cl == nil || m.cl.AIGateway == nil || execID == "" {
		return nil
	}
	m.execUsage.mu.Lock()
	at, ever := m.execUsage.at[execID]
	m.execUsage.mu.Unlock()
	if !ever || time.Since(at) > usageTTL {
		return m.loadExecutionUsage(execID)
	}
	return nil
}

// usageTTL is how long a cached context picture is drawn before it is re-read.
const usageTTL = 20 * time.Second

// summarizeUsage folds usage records into the context picture. Records arrive newest-first from the
// list, which is irrelevant here because every field is an aggregate or a maximum.
func summarizeUsage(records []*apiv1.UsageRecord) *execUsage {
	if len(records) == 0 {
		return nil
	}
	// Newest first for the provider/model, so the label names the model that ran LAST — the one
	// the run's tail is actually in. Sorted defensively rather than trusting the server's order.
	sorted := append([]*apiv1.UsageRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].GetOccurredAt().AsTime().After(sorted[j].GetOccurredAt().AsTime())
	})
	u := &execUsage{turns: len(sorted)}
	for _, r := range sorted {
		if u.provider == "" && r.GetProvider() != "" {
			u.provider, u.model = r.GetProvider(), r.GetModel()
		}
		u.prompt += r.GetPromptTokens()
		u.completion += r.GetCompletionTokens()
		u.reasoning += r.GetReasoningTokens()
		u.cacheRead += r.GetCacheReadTokens()
		u.total += r.GetTotalTokens()
		u.costUSD += r.GetCostUsd()
		if ws := r.GetPromptTokens() + r.GetCompletionTokens() + r.GetReasoningTokens(); ws > u.workingSet {
			u.workingSet = ws
		}
	}
	return u
}

// renderUsage draws the context / token / cost block.
//
// Every line is omitted when its numbers are zero, so an execution the gateway recorded no usage
// for gets NO panel rather than a row of zeroes that reads as "it used nothing" — which for a
// failed-to-start execution would be true and for an old one would just be missing data. The
// caller draws the section only when this returns something.
func renderUsage(u *execUsage, width int) string {
	if u == nil || (u.total == 0 && u.costUSD == 0) {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render("context") + "\n")

	// The model, the share of its window, and a bar — the one line the operator asked for.
	if u.model != "" {
		label := u.model
		if u.provider != "" {
			label = u.provider + "/" + u.model
		}
		if p := u.percent(); p >= 0 {
			b.WriteString(fmt.Sprintf("%s  ·  %s of context (%s)\n",
				label, fmt.Sprintf("%d%%", p), humanTokens(u.workingSet)))
			b.WriteString(contextBar(p, width) + "\n")
		} else {
			// No window: the working set is still worth stating, it just has no denominator.
			b.WriteString(fmt.Sprintf("%s  ·  %s working set\n", label, humanTokens(u.workingSet)))
		}
	}

	// The breakdown, in the order the GUI shows it. Cache lines matter: on a long run they are the
	// difference between "this cost a lot" and "this re-sent a lot".
	parts := []string{}
	if u.prompt > 0 {
		parts = append(parts, "in "+humanTokens(u.prompt))
	}
	if u.completion > 0 {
		parts = append(parts, "out "+humanTokens(u.completion))
	}
	if u.reasoning > 0 {
		parts = append(parts, "reasoning "+humanTokens(u.reasoning))
	}
	if u.cacheRead > 0 {
		parts = append(parts, "cache read "+humanTokens(u.cacheRead))
	}
	if u.total > 0 {
		parts = append(parts, "total "+humanTokens(u.total))
	}
	if u.turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", u.turns))
	}
	if len(parts) > 0 {
		b.WriteString(theme.HintText.Render(strings.Join(parts, " · ")) + "\n")
	}
	if u.costUSD > 0 {
		b.WriteString(fmt.Sprintf("cost $%.4f\n", u.costUSD))
	}
	return strings.TrimRight(b.String(), "\n")
}

// contextBar draws a fixed-width occupancy bar. It is CLAMPED at the width so a working set over
// the nominal window cannot overflow the pane (a bar one cell too long wraps, and a wrapped bar
// pushed the rest of the panel down).
func contextBar(percent, width int) string {
	const cells = 24
	filled := percent * cells / 100
	if filled > cells {
		filled = cells
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
	// Truncate BY RUNE, and drop the partial cell's rune entirely: `bar[:width]` slices BYTES, and
	// both bar glyphs are three bytes wide, so a byte slice silently returns a quarter of the cells
	// it claims (the defect this line fixes — a 10-cell pane got 4 cells of bar).
	if width > 0 && cells > width {
		bar = string([]rune(bar)[:width])
	}
	// Warn colouring at the point a context window starts costing quality, so the bar is
	// readable at a glance rather than needing the percentage. Purely presentational: the number
	// beside it is the authority.
	style := theme.HintText
	if percent >= 80 {
		style = theme.ErrorText
	} else if percent >= 50 {
		style = theme.ListTitle
	}
	return style.Render(bar)
}

// humanTokens renders a token count compactly: a long run's totals are millions, and the exact
// digit is noise in a panel (the precise figures are in the usage records).
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
