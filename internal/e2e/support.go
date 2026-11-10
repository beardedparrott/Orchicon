package e2e

// support.go — the harness's types, its RPC calls, its cleanup and its markdown report. Split from
// run.go so that file reads as the six steps and nothing else.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

type opts struct {
	baseURL      string
	token        string
	refreshToken string
	insecureTLS  bool
	projectID    string
	projectDir   string
	runtimeImage string
	localOnly    bool
	keep         bool
	dispatch     bool
	out          string
	askTimeout   time.Duration
	taskTimeout  time.Duration
	orchiconRef  string
	claudeRef    string
	opencodeRef  string
}

type adapter struct{ kind, ref string }

type worker struct {
	id      string
	version int32
}

type workItem struct {
	id    string
	title string
}

type askRow struct {
	mode, adapter, reply string
	elapsed              time.Duration
	err                  error
}

type taskRow struct {
	n       int
	title   string
	status  string
	elapsed time.Duration
}

type matrixRow struct {
	n       int
	adapter string
	mode    string
	title   string
}

type report struct {
	baseURL         string
	started         time.Time
	projectID       string
	projectDir      string
	epicTitle       string
	asks            []askRow
	tasks           []taskRow
	plan            []matrixRow
	items           []workItem
	cleanupFailures []string
	kept            bool
}

// adapters builds the list from the flags in a fixed order, so a run is reproducible.
func (c opts) adapters() []adapter {
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

// --- RPC -------------------------------------------------------------------

func probe(ctx context.Context, cs *client.Clients) error {
	_, err := cs.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{}))
	return err
}

// createProject makes the throwaway project ACTIVE, giving it a real directory first.
//
// A project is created in `drafting` and NOTHING can be created inside one — the server refuses with
// `project not active: project is "drafting", must be active`. So: create, set project_dir, activate. The
// directory is set BEFORE activation, because an active project with no directory cannot run a task.
func createProject(ctx context.Context, cs *client.Clients, prefix, dir string) (string, error) {
	slug := fmt.Sprintf("%s-%d", prefix, time.Now().Unix())

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create project dir %s: %w", dir, err)
	}

	resp, err := cs.Projects.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: slug, Slug: slug, GitStrategy: apiv1.GitStrategy_GIT_STRATEGY_NONE,
	}))
	if err != nil {
		return "", err
	}
	id := resp.Msg.GetProject().GetId()

	if _, err := cs.Projects.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
		Id: id, ProjectDir: &dir,
	})); err != nil {
		return id, fmt.Errorf("set the project directory: %w", err)
	}
	if _, err := cs.Projects.ActivateProject(ctx, connect.NewRequest(&apiv1.ActivateProjectRequest{Id: id})); err != nil {
		return id, fmt.Errorf("activate the project: %w", err)
	}
	return id, nil
}

// createWorkers makes one worker per adapter: the adapter IS the worker's model_ref segment, and the
// seeded workers mostly carry none, so reusing them would exercise nothing.
func createWorkers(ctx context.Context, cs *client.Clients, adapters []adapter) (map[string]worker, error) {
	out := map[string]worker{}
	for _, a := range adapters {
		resp, err := cs.Workers.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
			Name:     fmt.Sprintf("e2e-%s-%d", a.kind, time.Now().Unix()),
			ModelRef: a.ref,
			Purpose:  "Harness worker for the adapter/mode matrix.",
		}))
		if err != nil {
			return out, fmt.Errorf("create %s worker: %w", a.kind, err)
		}
		id := resp.Msg.GetWorker().GetId()
		w := worker{id: id, version: resp.Msg.GetVersion().GetVersion()}
		if _, err := cs.Workers.PublishWorkerVersion(ctx, connect.NewRequest(&apiv1.PublishWorkerVersionRequest{
			WorkerId: id, VersionNote: "harness",
		})); err != nil {
			out[a.kind] = w
			return out, fmt.Errorf("publish %s worker: %w", a.kind, err)
		}
		out[a.kind] = w
	}
	return out, nil
}

func createConversation(ctx context.Context, cs *client.Clients, projectID, modelRef string,
	mode apiv1.ConversationMode, msg string) (string, error) {

	resp, err := cs.Ask.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
		ProjectId: projectID, ModelRef: modelRef, Mode: mode, InitialMessage: msg,
	}))
	if err != nil {
		return "", err
	}
	id := resp.Msg.GetConversation().GetId()
	if id == "" {
		return "", errors.New("CreateConversation returned no conversation id")
	}
	return id, nil
}

// awaitReply polls for the assistant message: ChatStream is a stream, but the collector persists the
// answer regardless, so polling is simpler and cannot half-read one.
func awaitReply(ctx context.Context, cs *client.Clients, convID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := cs.Ask.ListMessages(ctx, connect.NewRequest(&apiv1.ListMessagesRequest{ConversationId: convID}))
		if err == nil {
			for _, m := range resp.Msg.GetMessages() {
				if strings.EqualFold(m.GetRole(), "assistant") && strings.TrimSpace(m.GetContent()) != "" {
					return m.GetContent(), nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("no assistant reply within %s", timeout)
}

func createWorkItem(ctx context.Context, cs *client.Clients, projectID, parentID string,
	kind apiv1.WorkItemKind, title, desc string) (string, error) {

	resp, err := cs.WorkItems.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: projectID, ParentId: parentID, Kind: kind, Title: title, Description: desc,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetWorkItem().GetId(), nil
}

func pinTask(ctx context.Context, cs *client.Clients, t workItem, workflowID string,
	mode apiv1.ExecutionMode, image string, w worker, dispatch bool) error {

	upd := &apiv1.UpdateWorkItemRequest{Id: t.id, WorkflowId: &workflowID, ExecutionMode: &mode}
	if mode == apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME && image != "" {
		upd.RuntimeImage = &image
	}
	if dispatch {
		yes := true
		upd.AutoStartWorkflow = &yes
	}
	if _, err := cs.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(upd)); err != nil {
		return err
	}
	ref := fmt.Sprintf(`{"worker_id":%q,"version":%d}`, w.id, w.version)
	_, err := cs.WorkItems.AssignWorker(ctx, connect.NewRequest(&apiv1.AssignWorkerRequest{Id: t.id, WorkerRef: ref}))
	return err
}

func findWorkflow(ctx context.Context, cs *client.Clients, name string) (string, error) {
	resp, err := cs.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{}))
	if err != nil {
		return "", err
	}
	for _, w := range resp.Msg.GetWorkflows() {
		if strings.EqualFold(w.GetName(), name) {
			return w.GetId(), nil
		}
	}
	return "", fmt.Errorf("no workflow named %q on this plane", name)
}

// --- steps 1 and 2 ---------------------------------------------------------

func askOne(ctx context.Context, cs *client.Clients, rep *report, projectID string,
	mode apiv1.ConversationMode, a adapter, timeout time.Duration) {

	const prompt = "In two sentences: what should a team verify first when building a small web app?"
	started := time.Now()
	convID, err := createConversation(ctx, cs, projectID, a.ref, mode, prompt)
	if err != nil {
		rep.asks = append(rep.asks, askRow{mode: modeString2(mode), adapter: a.kind, err: err})
		fmt.Printf("ask %-10s %-8s FAILED   %v\n", modeString2(mode), a.kind, err)
		return
	}
	reply, err := awaitReply(ctx, cs, convID, timeout)
	rep.asks = append(rep.asks, askRow{
		mode: modeString2(mode), adapter: a.kind, elapsed: time.Since(started), err: err,
		reply: firstLines(reply, 2),
	})
	status := "ok"
	if err != nil {
		status = "FAILED   " + oneLine(err.Error())
	}
	fmt.Printf("ask %-10s %-8s %-8s %s\n", modeString2(mode), a.kind, status, time.Since(started).Round(time.Second))
}

// planEpic is the epic and its six tasks, mirrored from the WorkerTesting project where this shape is
// already proven. The DESCRIPTIONS are deliberately not copied: they are ~10 KB of prompt engineering
// specific to that project, and this harness exercises adapters and execution modes rather than re-running
// that epic.
var planEpic = struct {
	title string
	tasks []string
}{
	title: "Weather web app on :9090 — keyless weather + news SPA (C/F, city resolution, compare, hourly/daily, news tabs)",
	tasks: []string{
		"Foundation: zero-dep Node 22 server on :9090, SPA shell, design tokens, test harness + .orchicon/verify gate",
		"BFF data layer: /api/geocode + /api/weather, canonical °C, WMO→condition mapping, TTL cache, stale fallback, fixture tests",
		"City resolution + compare slots: debounced combobox with ambiguous results and keyboard nav, geolocation, pure 1-or-2-city store",
		"Conditions + forecast UI: current card, 24h strip in location timezone, 7-day list, C/F toggle bound to real conversion",
		"News panel: Tech/Games/Business via server-side RSS proxy, per-source failure isolation, tabs, filter, refresh",
		"End-to-end interaction contract: catalogue every button/search/filter, prove single+compare parity, keyboard and resize through localhost:9090",
	},
}

// createPlan creates the epic and its six tasks. DETERMINISTIC BY CHOICE: an Ask asked to build a plan can
// answer with a clarifying question or stall, and a matrix run that depends on a model behaving fails for
// reasons unrelated to what it tests — measured, twice, as "produced epic <none> with 0 task(s)". The Ask is
// still exercised by step 1; it just does not own the plan.
func createPlan(ctx context.Context, cs *client.Clients, rep *report, projectID string) (string, []workItem, error) {
	epicID, err := createWorkItem(ctx, cs, projectID, "", apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		planEpic.title, "Weather web app epic, mirrored from the WorkerTesting project.")
	if err != nil {
		return "", nil, fmt.Errorf("create the epic: %w", err)
	}
	rep.epicTitle = planEpic.title
	rep.items = append(rep.items, workItem{id: epicID, title: planEpic.title})

	tasks := make([]workItem, 0, len(planEpic.tasks))
	for i, title := range planEpic.tasks {
		id, cerr := createWorkItem(ctx, cs, projectID, epicID, apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			title, fmt.Sprintf("Task %d of 6 in the mirrored weather-web-app epic.", i+1))
		if cerr != nil {
			return epicID, tasks, fmt.Errorf("create task %q: %w", title, cerr)
		}
		rep.items = append(rep.items, workItem{id: id, title: title})
		tasks = append(tasks, workItem{id: id, title: title})
	}
	fmt.Printf("\nplan     : epic + %d task(s) created\n", len(tasks))
	return epicID, tasks, nil
}

// --- step 5 ----------------------------------------------------------------

func waitTask(ctx context.Context, cs *client.Clients, rep *report, t workItem, n int, timeout time.Duration) {
	started := time.Now()
	deadline := started.Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := cs.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: t.id}))
		if err == nil {
			raw := resp.Msg.GetWorkItem().GetStatus().String()
			if terminalStatus(raw) {
				st := shortStatus(raw)
				rep.tasks = append(rep.tasks, taskRow{n: n, title: t.title, status: st, elapsed: time.Since(started)})
				fmt.Printf("run %d/6    : %-12s %s (%s)\n", n, st, truncate(t.title, 50),
					time.Since(started).Round(time.Second))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	rep.tasks = append(rep.tasks, taskRow{n: n, title: t.title, status: "timeout", elapsed: time.Since(started)})
	fmt.Printf("run %d/6    : %-12s %s (timed out)\n", n, "timeout", truncate(t.title, 50))
}

// --- step 6 ----------------------------------------------------------------

func removeEverything(ctx context.Context, cs *client.Clients, rep *report,
	workers map[string]worker, projectID string, ownProject bool) {

	for _, it := range rep.items {
		if _, err := cs.WorkItems.DeleteWorkItem(ctx,
			connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: it.id})); err != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "work item "+it.id)
		}
	}
	cleanupWorkers(ctx, cs, rep, workers)
	if ownProject {
		removeProject(ctx, cs, rep, projectID)
	}
	if len(rep.cleanupFailures) == 0 {
		fmt.Printf("\ncleanup  : complete (work items, workers%s)\n", pick(ownProject, ", project", ""))
		return
	}
	fmt.Printf("\ncleanup  : INCOMPLETE — still present: %s\n", strings.Join(rep.cleanupFailures, ", "))
}

func cleanupWorkers(ctx context.Context, cs *client.Clients, rep *report, workers map[string]worker) {
	for _, w := range workers {
		if _, err := cs.Workers.DeleteWorker(ctx,
			connect.NewRequest(&apiv1.DeleteWorkerRequest{Id: w.id})); err != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "worker "+w.id)
		}
	}
}

func removeProject(ctx context.Context, cs *client.Clients, rep *report, projectID string) {
	if _, err := cs.Projects.DeleteProject(ctx,
		connect.NewRequest(&apiv1.DeleteProjectRequest{Id: projectID})); err != nil {
		rep.cleanupFailures = append(rep.cleanupFailures, "project "+projectID)
	}
}

// --- the report ------------------------------------------------------------

// write renders the run as markdown. It is deferred, so it lands even when a step fails — the record
// of what happened is worth more than an empty file.
func (r *report) write(path string, c opts) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchicon adapter/mode matrix\n\n")
	fmt.Fprintf(&b, "- started: %s\n- plane: %s\n- project: %s\n",
		r.started.Format(time.RFC3339), r.baseURL, r.projectID)
	fmt.Fprintf(&b, "- adapters: %s\n", strings.Join(adapterNames(c.adapters()), ", "))
	fmt.Fprintf(&b, "- modes: %s\n", pick(c.localOnly, "local only", "local + runtime ("+c.runtimeImage+")"))
	fmt.Fprintf(&b, "- dispatched: %v\n- duration: %s\n\n", c.dispatch, time.Since(r.started).Round(time.Second))

	b.WriteString("## 1. Ask, per mode per adapter\n\n| mode | adapter | result | elapsed |\n|---|---|---|---|\n")
	for _, a := range r.asks {
		result := "ok"
		if a.err != nil {
			result = "FAILED: " + oneLine(a.err.Error())
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", a.mode, a.adapter, result, a.elapsed.Round(time.Second))
	}
	for _, a := range r.asks {
		if a.reply != "" {
			fmt.Fprintf(&b, "\n**%s / %s** replied:\n\n> %s\n", a.mode, a.adapter,
				strings.ReplaceAll(a.reply, "\n", "\n> "))
		}
	}

	fmt.Fprintf(&b, "\n## 2. Epic and tasks\n\n- epic: %s\n- tasks created: **%d** of 6\n",
		r.epicTitle, len(r.items)-1)

	b.WriteString("\n## 3+4. Task matrix\n\n| # | adapter | mode | title |\n|---|---|---|---|\n")
	for _, p := range r.plan {
		fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", p.n, p.adapter, p.mode, p.title)
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
		fmt.Fprintf(os.Stderr, "\nwarning: could not write %s: %v\n", path, err)
		return
	}
	fmt.Printf("\nreport   : %s\n", path)
}

// --- small helpers ---------------------------------------------------------

func modeString2(m apiv1.ConversationMode) string {
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

func modeString(m apiv1.ExecutionMode) string {
	if m == apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		return "runtime"
	}
	return "local"
}

// terminalStatus matches the enum's own name so a new status cannot silently look terminal.
func terminalStatus(s string) bool {
	for _, want := range []string{"SUCCEEDED", "FAILED", "CANCELLED", "SKIPPED", "ARCHIVED"} {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func shortStatus(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "WORK_ITEM_STATUS_"))
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

func titleOr(id string) string {
	if strings.TrimSpace(id) == "" {
		return "<none>"
	}
	return id
}

func pick[T any](cond bool, ifTrue, ifFalse T) T {
	if cond {
		return ifTrue
	}
	return ifFalse
}
