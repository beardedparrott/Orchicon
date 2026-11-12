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
	"github.com/beardedparrott/orchicon/internal/project"
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
	cleanup      bool
	launch       bool
	launchOnly   bool
	cleanupOnly  bool
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
	id     string
	title  string
	status string
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

// workerCache holds the adapter->worker map for a project, so the launch pass can find the worker a task
// must be re-pointed at. Keyed by project id because -launch-only starts from one.
var workerCache = map[string]map[string]worker{}

// workflowCache is kind -> workflow id per project: the per-adapter Quick Work copies, which are what a
// launch binds a task to.
var workflowCache = map[string]map[string]string{}

// findWorkersForProject resolves the setup pass's workers for an EXISTING project, by name.
//
// The setup pass names them `e2e-<adapter>-<stamp>`; rather than store that stamp, this picks the NEWEST
// matching worker per adapter, so a later setup pass supersedes an earlier one without ambiguity.
func findWorkersForProject(ctx context.Context, cs *client.Clients, adapters []adapter) (map[string]worker, error) {
	resp, err := cs.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	out := map[string]worker{}
	for _, a := range adapters {
		var best *apiv1.Worker
		for _, w := range resp.Msg.GetWorkers() {
			if !strings.HasPrefix(w.GetName(), "e2e-"+a.kind+"-") {
				continue
			}
			if best == nil || w.GetCreatedAt().AsTime().After(best.GetCreatedAt().AsTime()) {
				best = w
			}
		}
		if best == nil {
			return nil, fmt.Errorf("no %s harness worker found — run a setup pass with -%s-model first",
				a.kind, a.kind)
		}
		out[a.kind] = worker{id: best.GetId(), version: best.GetCurrentVersion()}
	}
	return out, nil
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

	// THE PROJECT MUST BE GIT-BACKED, and this is not cosmetic: every workflow run provisions a WORKTREE, and
	// `git worktree add` needs a HEAD to cut a branch from. With git_strategy=none the run's provisioning step
	// fails before any worker is dispatched —
	//   `worktree provisioning failed … step "Quick Software Engineer" cannot dispatch`
	// — which presents as a run that fails in 5s with an EMPTY error and no execution, exactly like a broken
	// adapter. EnsureGitState initialises the repository and makes the bootstrap commit that gives it a HEAD.
	if gs := project.EnsureGitState(ctx, dir, "local"); gs.Err != nil {
		return "", fmt.Errorf("prepare the project's git state (%s): %w", gs.Done, gs.Err)
	}

	resp, err := cs.Projects.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: slug, Slug: slug, GitStrategy: apiv1.GitStrategy_GIT_STRATEGY_LOCAL,
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

// numberedAC renders acceptance criteria as the numbered markdown list a worker's AC REPORT answers against.
func numberedAC(ac []string) string {
	if len(ac) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range ac {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c)
	}
	return b.String()
}

// createWorkItemWithAC creates a work item carrying acceptance criteria.
//
// A TASK WITH NO CRITERIA CANNOT BE VERIFIED. The worker's AC REPORT answers the item's criteria; with none it
// has nothing to answer, the report is absent, and the verdict abstains — so the step is recorded UNVERIFIED
// with "the worker reported no acceptance-criteria outcomes". That is a property of the WORK ITEM, not of the
// worker, and a harness that leaves them out tests the abstention path instead of the verification one.
func createWorkItemWithAC(ctx context.Context, cs *client.Clients, projectID, parentID string,
	kind apiv1.WorkItemKind, title, desc, ac string) (string, error) {

	resp, err := cs.WorkItems.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: projectID, ParentId: parentID, Kind: kind, Title: title,
		Description: desc, AcceptanceCriteria: ac,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetWorkItem().GetId(), nil
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
// planTask is one task of the epic: its title plus the acceptance criteria it is measured against.
//
// THE ACCEPTANCE CRITERIA ARE NOT DECORATION. A work item with none produces a worker with nothing to report
// against, so the run's AC REPORT is absent and the verification verdict ABSTAINS with
// "the worker reported no acceptance-criteria outcomes" — which is exactly the notice that made a successful
// quick-worker run look like a failure. Every real work item carries criteria; a harness task without them
// exercises the abstention path instead of the verification path.
type planTask struct {
	title string
	ac    []string
}

var planEpic = struct {
	title string
	tasks []planTask
}{
	title: "Weather web app on :9090 — keyless weather + news SPA (C/F, city resolution, compare, hourly/daily, news tabs)",
	tasks: []planTask{
		{
			title: "Foundation: zero-dep Node 22 server on :9090, SPA shell, design tokens, test harness + .orchicon/verify gate",
			ac: []string{
				"The server binds 127.0.0.1:9090 and `curl -si http://127.0.0.1:9090/` returns 200 with an HTML body",
				"The gate is declared: `.orchicon/verify` exists, is tracked by git, and holds one command line",
				"`node scripts/gate.mjs` prints named PASS lines, zero FAIL lines, and exits 0",
			},
		},
		{
			title: "BFF data layer: /api/geocode + /api/weather, canonical °C, WMO→condition mapping, TTL cache, stale fallback, fixture tests",
			ac: []string{
				"`GET /api/weather` returns the documented payload with temperatures in canonical °C",
				"An upstream failure returns `{stale:true}` with the last known payload rather than an error",
				"Fixture-based tests cover the WMO→condition mapping and pass offline",
			},
		},
		{
			title: "City resolution + compare slots: debounced combobox with ambiguous results and keyboard nav, geolocation, pure 1-or-2-city store",
			ac: []string{
				"Typing a city name shows a debounced list of real resolution matches",
				"The combobox is keyboard navigable and commits the highlighted entry on Enter",
				"Two slots render side by side in compare mode and the store holds exactly one or two cities",
			},
		},
		{
			title: "Conditions + forecast UI: current card, 24h strip in location timezone, 7-day list, C/F toggle bound to real conversion",
			ac: []string{
				"A current-conditions card renders values from `/api/weather` for the selected city",
				"The hourly strip shows 24 entries labelled in the LOCATION's timezone, not the browser's",
				"Toggling C/F reconverts every rendered temperature rather than relabelling it",
			},
		},
		{
			title: "News panel: Tech/Games/Business via server-side RSS proxy, per-source failure isolation, tabs, filter, refresh",
			ac: []string{
				"Three tabs fetch Tech, Games and Business through the server-side RSS proxy",
				"One failing feed degrades only its own tab and names the failure",
				"The refresh control re-issues the fetch without losing the selected tab",
			},
		},
		{
			title: "End-to-end interaction contract: catalogue every button/search/filter, prove single+compare parity, keyboard and resize through localhost:9090",
			ac: []string{
				"Every button, search and filter in the app is catalogued with the behaviour it must exhibit",
				"Single-city and compare modes render the same component contract for each slot",
				"Keyboard navigation and a narrow viewport behave correctly end to end against localhost:9090",
			},
		},
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
	for i, t := range planEpic.tasks {
		id, cerr := createWorkItemWithAC(ctx, cs, projectID, epicID, apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			t.title, fmt.Sprintf("Task %d of 6 in the mirrored weather-web-app epic.", i+1),
			numberedAC(t.ac))
		if cerr != nil {
			return epicID, tasks, fmt.Errorf("create task %q: %w", t.title, cerr)
		}
		rep.items = append(rep.items, workItem{id: id, title: t.title})
		tasks = append(tasks, workItem{id: id, title: t.title})
	}
	fmt.Printf("\nplan     : epic + %d task(s) created\n", len(tasks))
	return epicID, tasks, nil
}

// --- step 5 ----------------------------------------------------------------

// waitTask follows a launched task to a terminal state.
//
// TWO SIGNALS, AND THE SECOND IS THE ONE THAT MATTERS. A task bound to a workflow RUN reports `running`
// while the run works, and only the RUN reaches completed/failed — so polling the item's own status saw
// "running" for the whole window and reported a timeout on work that was progressing fine. The item is
// still checked, because a task with no workflow run (or one whose run was never created) is finished when
// the ITEM is terminal.
func waitTask(ctx context.Context, cs *client.Clients, rep *report, t workItem, n int, timeout time.Duration) {
	started := time.Now()
	deadline := started.Add(timeout)

	record := func(status string) {
		rep.tasks = append(rep.tasks, taskRow{n: n, title: t.title, status: status, elapsed: time.Since(started)})
		fmt.Printf("run %d/6    : %-12s %s (%s)\n", n, status, truncate(t.title, 50),
			time.Since(started).Round(time.Second))
	}

	for time.Now().Before(deadline) {
		item, err := cs.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: t.id}))
		if err == nil {
			raw := item.Msg.GetWorkItem().GetStatus().String()
			runID := item.Msg.GetWorkItem().GetWorkflowRunId()

			// The RUN is authoritative once one exists.
			if runID != "" {
				if run, rerr := cs.Workflows.GetWorkflowRun(ctx, connect.NewRequest(
					&apiv1.GetWorkflowRunRequest{Id: runID})); rerr == nil {
					if st := run.Msg.GetRun().GetStatus().String(); terminalStatus(st) {
						record(shortStatus(st) + " (run)")
						return
					}
				}
			} else if terminalStatus(raw) {
				record(shortStatus(raw))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	record("timeout")
}

// --- step 6 ----------------------------------------------------------------

func removeEverything(ctx context.Context, cs *client.Clients, rep *report,
	workers map[string]worker, workflows map[string]string, projectID string, ownProject bool) {

	for _, it := range rep.items {
		if _, err := cs.WorkItems.DeleteWorkItem(ctx,
			connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: it.id})); err != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "work item "+it.id)
		}
	}
	// THE WORKFLOWS ARE CREATED OUTSIDE THE PROJECT, so deleting the project does not reclaim them. Without
	// this they accumulate one set per setup pass and every one of them is visible in the console.
	cleanupWorkflows(ctx, cs, rep, workflows)
	cleanupWorkers(ctx, cs, rep, workers)
	if ownProject {
		removeProject(ctx, cs, rep, projectID)
	}
	if len(rep.cleanupFailures) == 0 {
		fmt.Printf("\ncleanup  : complete (work items, %d workflow(s), workers%s)\n",
			len(workflows), pick(ownProject, ", project", ""))
		return
	}
	fmt.Printf("\ncleanup  : INCOMPLETE — still present: %s\n", strings.Join(rep.cleanupFailures, ", "))
}

// cleanupWorkflows removes the per-adapter workflows. Each holds run history, so a delete that fails is
// reported rather than retried blindly — it usually means a run is still in flight against it.
func cleanupWorkflows(ctx context.Context, cs *client.Clients, rep *report, workflows map[string]string) {
	for kind, id := range workflows {
		if _, err := cs.Workflows.DeleteWorkflow(ctx,
			connect.NewRequest(&apiv1.DeleteWorkflowRequest{Id: id})); err != nil {
			rep.cleanupFailures = append(rep.cleanupFailures,
				fmt.Sprintf("%s workflow %s (%v)", adapterWorkflowName(kind), id, err))
		}
	}
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
	fmt.Fprintf(&b, "- launched: %v\n- left in place: %v\n- duration: %s\n\n",
		c.launch, !c.cleanup, time.Since(r.started).Round(time.Second))

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

	if c.launch {
		b.WriteString("\n## 5. Launch results\n\n| # | status | elapsed | title |\n|---|---|---|---|\n")
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
// terminalStatus reports whether a status string is a FINAL state.
//
// THE TWO ENUMS DO NOT AGREE ON THE WORD, which is what stalled the launch loop: a finished WORK ITEM is
// SUCCEEDED, but a finished WORKFLOW RUN is COMPLETED (WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED).
// `COMPLETED` matched none of the item's words, so a run that had finished never looked terminal — the wait
// spun to its deadline, and the six-task chain advanced only when that happened, which looked like "it never
// moved on to the next task".
//
// ABORTED and REJECTED are here for the same reason: they are final for the types that use them, and a
// status that is terminal but unrecognised is a hang, not a slow test.
func terminalStatus(s string) bool {
	for _, want := range []string{
		"SUCCEEDED", "FAILED", "CANCELLED", "SKIPPED", "ARCHIVED", // work items
		"COMPLETED", "ABORTED", "REJECTED", "EXPIRED", // workflow runs and other terminal states
	} {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// shortStatus trims whichever enum prefix the status carries. There are TWO — WORK_ITEM_STATUS_ and
// WORKFLOW_RUN_STATUS_ — and trimming only the item's left every run row reading
// "workflow_run_status_failed" in the report.
func shortStatus(s string) string {
	s = strings.TrimPrefix(s, "WORK_ITEM_STATUS_")
	s = strings.TrimPrefix(s, "WORKFLOW_RUN_STATUS_")
	return strings.ToLower(s)
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

// sweepHarnessWorkers deletes every worker this harness ever created, by NAME, rather than only the three the
// current flags happen to name.
//
// The map-driven cleanup reclaims what THIS process made, which is right for a deferred cleanup but useless
// after the fact: a setup run with different adapter flags — or an earlier interrupted run — leaves workers
// whose kind no longer appears in the map, and they accumulate silently. The prefix is the harness's own
// namespace, so a name sweep is the honest boundary.
func sweepHarnessWorkers(ctx context.Context, cs *client.Clients, rep *report) {
	resp, err := cs.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{}))
	if err != nil {
		rep.cleanupFailures = append(rep.cleanupFailures, "list workers ("+err.Error()+")")
		return
	}
	removed := 0
	for _, w := range resp.Msg.GetWorkers() {
		if !strings.HasPrefix(w.GetName(), "e2e-") {
			continue
		}
		if _, derr := cs.Workers.DeleteWorker(ctx,
			connect.NewRequest(&apiv1.DeleteWorkerRequest{Id: w.GetId()})); derr != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "worker "+w.GetName())
			continue
		}
		removed++
	}
	if removed > 0 {
		fmt.Printf("cleanup  : removed %d harness worker(s) by name\n", removed)
	}
}

// cleanupConversations deletes the Ask conversations this harness created in a project.
//
// THEY WERE NEVER RECLAIMED AT ALL, which is why a "cleaned" plane still listed nine e2e chats. Every ask the
// harness makes — six matrix asks and the epic prompt — is a real conversation with real messages, attributed
// to the throwaway project via Conversation.project_id, so it is unambiguously ours to remove.
func cleanupConversations(ctx context.Context, cs *client.Clients, rep *report, projectID string) {
	list, err := cs.Ask.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{
		PageSize: 200,
	}))
	if err != nil {
		rep.cleanupFailures = append(rep.cleanupFailures, "list conversations ("+err.Error()+")")
		return
	}
	removed := 0
	for _, c := range list.Msg.GetConversations() {
		if c.GetProjectId() != projectID {
			continue
		}
		if _, derr := cs.Ask.DeleteConversation(ctx,
			connect.NewRequest(&apiv1.DeleteConversationRequest{Id: c.GetId()})); derr != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "conversation "+c.GetId())
			continue
		}
		removed++
	}
	if removed > 0 {
		fmt.Printf("cleanup  : removed %d ask conversation(s)\n", removed)
	}
}

// resolveRuntimeImage picks the image runtime-mode tasks run in.
//
// A LITERAL DEFAULT IS A TAG THAT EXISTS ON ONE MACHINE. This harness shipped with
// "orchicon-runtime:orchicon-dev" — what a Linux dev host happens to build — while a Windows plane's
// catalogue row for the SAME variant is "orchicon-runtime:dev". On Windows every runtime task therefore failed
// at image lookup, and (before failRunAtStart recorded its reason) with no error in the UI at all. The variant
// is the identity; the tag is how a particular instance addresses it, and only the instance knows that.
//
// So the plane is asked. ListRuntimeImages is the same call on every host, and the row whose tag belongs to the
// dev variant carries whatever name that host actually uses.
//
// An explicit -runtime-image always wins — an operator may be testing a custom image, and a harness that
// overrode that would be worse than the bug it fixes.
func resolveRuntimeImage(ctx context.Context, cs *client.Clients, override string) (tag, why string, err error) {
	if strings.TrimSpace(override) != "" {
		return override, "", nil
	}
	resp, lerr := cs.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{}))
	if lerr != nil {
		return "", "", fmt.Errorf("resolve the runtime image: could not list this plane's images (%w) — pass "+
			"-runtime-image if the plane cannot answer", lerr)
	}
	var ready, anyDev []string
	for _, img := range resp.Msg.GetRuntimeImages() {
		// THE VARIANT IS THE IDENTITY. Both spellings name the dev variant: the catalogue's ":dev" and the
		// historical ":orchicon-dev" that local builds and the seeded worker prompts still use.
		slug := strings.ToLower(img.GetSlug() + " " + img.GetTag())
		if !strings.Contains(slug, "dev") || strings.Contains(slug, "devops") {
			continue
		}
		anyDev = append(anyDev, img.GetTag())
		if img.GetStatus() == apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY {
			ready = append(ready, img.GetTag())
		}
	}
	switch {
	case len(ready) > 0:
		return ready[0], "this plane's dev image", nil
	case len(anyDev) > 0:
		// NOT READY is still a better answer than a guess: the run will fail on an image the operator can see
		// and build, rather than on a name this host has never heard of.
		return anyDev[0], "dev image (not built yet — build it before the runtime tasks)", nil
	}
	return "", "", errors.New("this plane has no dev runtime image in its catalogue, so runtime-mode tasks " +
		"cannot run. Create or download one (Settings → Runtime Images), or pass -runtime-image with a tag " +
		"this host's engine has")
}
