package main

// support.go holds the harness's small types, its markdown report and its cleanup — kept apart
// from main.go so the flow above reads as the six steps and nothing else.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	tuiclient "github.com/beardedparrott/orchicon/internal/tui/client"
)

type adapter struct {
	kind string
	ref  string
}

type worker struct {
	id      string
	version int32
}

type workItem struct {
	id     string
	title  string
	status string
}

type askRow struct {
	mode    string
	adapter string
	reply   string
	elapsed time.Duration
	tokens  int64
	err     error
}

type taskRow struct {
	n       int
	title   string
	status  string
	elapsed time.Duration
}

// matrixRow is the PLANNED adapter/mode for one task slot, so the report shows intent even when a
// task never dispatched.
type matrixRow struct {
	n       int
	adapter string
	mode    string
	title   string
}

type report struct {
	baseURL        string
	started        time.Time
	projectID      string
	epicTitle      string
	epicProduced   int
	epicReply      string
	epicElapsed    time.Duration
	epicTokens     int64
	epicErr        error
	asks           []askRow
	tasks          []taskRow
	items          []workItem // every work item this run created, for cleanup
	cleanupFailures []string
	kept           bool
}

// adapters builds the adapter list from the flags, in a fixed order so a run is reproducible.
func (c config) adapters() []adapter {
	var out []adapter
	for _, a := range []adapter{
		{kind: "orchicon", ref: c.orchiconRef},
		{kind: "claude", ref: c.claudeRef},
		{kind: "opencode", ref: c.opencodeRef},
	} {
		if strings.TrimSpace(a.ref) != "" {
			out = append(out, a)
		}
	}
	return out
}

func adapterNames(as []adapter) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.kind)
	}
	return out
}

// write renders the run as markdown. It is deferred, so it runs even when a step returns an error —
// the record of what happened is worth more than an empty file on a failed run.
func (r *report) write(path string, c config) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchicon adapter/mode matrix\n\n")
	fmt.Fprintf(&b, "- started: %s\n- plane: %s\n- project: %s\n",
		r.started.Format(time.RFC3339), r.baseURL, r.projectID)
	fmt.Fprintf(&b, "- adapters: %s\n", strings.Join(adapterNames(c.adapters()), ", "))
	fmt.Fprintf(&b, "- modes: %s\n", pick(c.localOnly, "local only", "local + runtime ("+c.runtimeImage+")"))
	fmt.Fprintf(&b, "- dispatched: %v\n- duration: %s\n\n", c.dispatch, time.Since(r.started).Round(time.Second))

	b.WriteString("## 1. Ask, per mode per adapter\n\n")
	b.WriteString("| mode | adapter | result | elapsed |\n|---|---|---|---|\n")
	for _, a := range r.asks {
		result := "ok"
		if a.err != nil {
			result = "FAILED: " + oneLine(a.err.Error())
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", a.mode, a.adapter, result, a.elapsed.Round(time.Second))
	}
	for _, a := range r.asks {
		if a.reply != "" {
			fmt.Fprintf(&b, "\n**%s / %s** replied:\n\n> %s\n", a.mode, a.adapter, strings.ReplaceAll(a.reply, "\n", "\n> "))
		}
	}

	b.WriteString("\n## 2. Epic from Brainstorm\n\n")
	fmt.Fprintf(&b, "- epic: %s\n- tasks produced by the Ask: **%d** (target 6)\n- elapsed: %s\n",
		r.epicTitle, r.epicProduced, r.epicElapsed.Round(time.Second))
	if r.epicErr != nil {
		fmt.Fprintf(&b, "- error: %s\n", oneLine(r.epicErr.Error()))
	}
	if r.epicReply != "" {
		fmt.Fprintf(&b, "\nReply (truncated):\n\n> %s\n", strings.ReplaceAll(r.epicReply, "\n", "\n> "))
	}

	b.WriteString("\n## 3+4. Task matrix\n\n")
	b.WriteString("| # | adapter | mode | title |\n|---|---|---|---|\n")
	for i, t := range r.matrixRows(c) {
		_ = i
		fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", t.n, t.adapter, t.mode, t.title)
	}

	if c.dispatch {
		b.WriteString("\n## 5. Dispatch results\n\n| # | status | elapsed | title |\n|---|---|---|---|\n")
		for _, t := range r.tasks {
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", t.n, t.status, t.elapsed.Round(time.Second), t.title)
		}
	}

	b.WriteString("\n## 6. Cleanup\n\n")
	switch {
	case r.kept:
		b.WriteString("- skipped (`-keep`): project and workers left in place\n")
	case len(r.cleanupFailures) == 0:
		b.WriteString("- complete: work items, workers and the throwaway project removed\n")
	default:
		fmt.Fprintf(&b, "- INCOMPLETE — still present: %s\n", strings.Join(r.cleanupFailures, ", "))
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "\nwarning: could not write the report to %s: %v\n", path, err)
		return
	}
	fmt.Printf("\nreport   : %s\n", path)
}

// matrixRows reconstructs the planned adapter/mode per task so the report shows the INTENT even when
// a task never dispatched. It mirrors run()'s selection, deliberately, so the two cannot drift.
func (r *report) matrixRows(c config) []matrixRow {
	as := c.adapters()
	out := make([]matrixRow, 0, 6)
	for i := 0; i < 6; i++ {
		kind, mode := "-", "local"
		if len(as) > 0 {
			kind = as[(i/2)%len(as)].kind
		}
		if !c.localOnly && i%2 == 1 {
			mode = "runtime"
		}
		title := "(not dispatched)"
		if i < len(r.tasks) {
			title = r.tasks[i].title
		}
		out = append(out, matrixRow{n: i + 1, adapter: kind, mode: mode, title: title})
	}
	return out
}

// removeEverything deletes what the run created. It reports failures rather than erroring: a
// half-cleaned plane is a fact to record, not a reason to lose the report.
func removeEverything(ctx context.Context, cs *tuiclient.Clients, r *report,
	workers map[string]worker, projectID string, ownProject bool) {

	var failed []string
	for _, it := range r.items {
		if _, err := cs.WorkItems.DeleteWorkItem(ctx,
			connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: it.id})); err != nil {
			failed = append(failed, "work item "+it.id)
		}
	}
	cleanupWorkers(ctx, cs, r, workers)
	if ownProject {
		if _, err := cs.Projects.DeleteProject(ctx,
			connect.NewRequest(&apiv1.DeleteProjectRequest{Id: projectID})); err != nil {
			failed = append(failed, "project "+projectID)
		}
	}
	r.cleanupFailures = append(r.cleanupFailures, failed...)
	if len(r.cleanupFailures) == 0 {
		fmt.Printf("\ncleanup  : complete (work items, workers%s)\n",
			pick(ownProject, ", project", ""))
		return
	}
	fmt.Printf("\ncleanup  : INCOMPLETE — still present: %s\n", strings.Join(r.cleanupFailures, ", "))
}

func cleanupWorkers(ctx context.Context, cs *tuiclient.Clients, r *report, workers map[string]worker) {
	for _, w := range workers {
		if _, err := cs.Workers.DeleteWorker(ctx,
			connect.NewRequest(&apiv1.DeleteWorkerRequest{Id: w.id})); err != nil {
			r.cleanupFailures = append(r.cleanupFailures, "worker "+w.id)
		}
	}
}

// --- tiny formatters -------------------------------------------------------

func modeName(m apiv1.ConversationMode) string {
	switch m {
	case apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM:
		return "brainstorm"
	case apiv1.ConversationMode_CONVERSATION_MODE_ITERATION:
		return "iteration"
	case apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK:
		return "quick-work"
	}
	return "unspecified"
}

func modeNameExec(m apiv1.ExecutionMode) string {
	if m == apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		return "runtime"
	}
	return "local"
}

// isTerminalStatus matches on the enum's own name so a new status cannot silently look terminal.
func isTerminalStatus(s string) bool {
	for _, want := range []string{"SUCCEEDED", "FAILED", "CANCELLED", "SKIPPED", "ARCHIVED"} {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func short(status string) string {
	return strings.ToLower(strings.TrimPrefix(status, "WORK_ITEM_STATUS_"))
}

func redact(tok string) string {
	if len(tok) <= 10 {
		return "(none)"
	}
	return tok[:6] + "…" + tok[len(tok)-4:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func titleOr(id, fallback string) string {
	if strings.TrimSpace(id) == "" {
		return fallback
	}
	return id
}

func pick[T any](cond bool, ifTrue, ifFalse T) T {
	if cond {
		return ifTrue
	}
	return ifFalse
}
