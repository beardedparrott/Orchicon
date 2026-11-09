package main

// orchicon-e2e runs the adapter/mode matrix against a live plane, builds a six-task epic
// pinned to Quick Work with alternating execution modes, records the results and cleans up.
//
// Go rather than shell so it runs unchanged on Linux, macOS and Windows and every call is a
// typed Connect client instead of hand-built JSON.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	tuiclient "github.com/beardedparrott/orchicon/internal/tui/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nfailed: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	instance     string
	baseURL      string
	token        string
	projectID    string
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

func run() error {
	var c config
	flag.StringVar(&c.instance, "instance", "dev", "instance whose control port to use (dev|test|prod)")
	flag.StringVar(&c.baseURL, "base-url", "", "plane base URL (default: the instance's control port)")
	flag.StringVar(&c.token, "token", "", "bearer token (default: $ORCHICON_TOKEN, then ~/.orchicon/config)")
	flag.StringVar(&c.projectID, "project", "", "reuse this project (default: create a throwaway one)")
	flag.StringVar(&c.runtimeImage, "runtime-image", "orchicon-runtime:orchicon-dev", "image for runtime-mode tasks")
	flag.BoolVar(&c.localOnly, "local-only", false, "skip runtime-mode tasks (no container engine, e.g. a cloud Mac)")
	flag.BoolVar(&c.keep, "keep", false, "do not clean up anything this run created")
	flag.BoolVar(&c.dispatch, "dispatch", false, "actually dispatch the six tasks (default: set them up only)")
	flag.StringVar(&c.out, "out", "", "results file (default orchicon-e2e-<stamp>.md in the cwd)")
	flag.DurationVar(&c.askTimeout, "ask-timeout", 4*time.Minute, "per-Ask timeout")
	flag.DurationVar(&c.taskTimeout, "task-timeout", 20*time.Minute, "per-task dispatch timeout")
	flag.StringVar(&c.orchiconRef, "orchicon-model", "orchicon/deepseek/deepseek-v4-flash",
		"model_ref for the orchicon adapter (adapter/provider/model)")
	flag.StringVar(&c.claudeRef, "claude-model", "",
		"model_ref for the claude adapter, e.g. claude/anthropic/claude-sonnet-4 (empty: skip it)")
	flag.StringVar(&c.opencodeRef, "opencode-model", "opencode/deepseek/deepseek-v4-flash",
		"model_ref for the opencode adapter (empty: skip it)")
	flag.Parse()

	if c.baseURL == "" {
		port, ok := map[string]int{"dev": 8080, "test": 8092, "prod": 8091}[c.instance]
		if !ok {
			return fmt.Errorf("-instance %q is not dev, test or prod", c.instance)
		}
		c.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	if c.token == "" {
		c.token = resolveToken()
	}
	if c.out == "" {
		c.out = fmt.Sprintf("orchicon-e2e-%s.md", time.Now().Format("20060102-150405"))
	}

	cs := tuiclient.New(tuiclient.Options{BaseURL: c.baseURL, Token: c.token, Timeout: 60 * time.Second})
	ctx := context.Background()
	rep := &report{baseURL: c.baseURL, started: time.Now()}
	defer rep.write(c.out, c)

	fmt.Printf("plane    : %s\n", c.baseURL)
	fmt.Printf("token    : %s\n", redact(c.token))
	if err := probe(ctx, cs); err != nil {
		return fmt.Errorf("plane not reachable — is it running and the token valid? (%w)", err)
	}

	adapters := c.adapters()
	if len(adapters) == 0 {
		return errors.New("no adapters selected: set -orchicon-model, -claude-model and/or -opencode-model")
	}
	fmt.Printf("adapters : %s\n", strings.Join(adapterNames(adapters), ", "))
	if c.localOnly {
		fmt.Printf("modes    : local only (-local-only)\n")
	} else {
		fmt.Printf("modes    : local + runtime (%s)\n", c.runtimeImage)
	}

	projectID, ownProject := c.projectID, false
	if projectID == "" {
		id, err := createProject(ctx, cs, "e2e-adapter-matrix")
		if err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		projectID, ownProject = id, true
	}
	rep.projectID = projectID
	fmt.Printf("project  : %s\n\n", projectID)

	workers, err := createWorkers(ctx, cs, adapters)
	if err != nil {
		cleanupWorkers(ctx, cs, rep, workers)
		return fmt.Errorf("create workers: %w", err)
	}
	defer func() {
		if c.keep {
			fmt.Printf("\n-keep: left project %s and %d worker(s) in place\n", projectID, len(workers))
			rep.kept = true
			return
		}
		removeEverything(context.Background(), cs, rep, workers, projectID, ownProject)
	}()

	// 1 — Ask once per mode per adapter, so every adapter is exercised in both modes.
	for _, m := range []apiv1.ConversationMode{
		apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
		apiv1.ConversationMode_CONVERSATION_MODE_ITERATION,
	} {
		for _, a := range adapters {
			askOne(ctx, cs, rep, projectID, m, a, c.askTimeout)
		}
	}

	// 2 — Brainstorm the epic, then read back what it actually created.
	epicID, tasks := askEpic(ctx, cs, rep, projectID, adapters[0], c.askTimeout)
	if epicID == "" {
		id, err := createWorkItem(ctx, cs, projectID, "", apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
			"Weather web app (harness fallback)", "Created because the Ask produced no epic.")
		if err != nil {
			return fmt.Errorf("create fallback epic: %w", err)
		}
		epicID = id
	}
	tasks = topUp(ctx, cs, projectID, epicID, tasks, 6)
	rep.items = append(rep.items, workItem{id: epicID})
	rep.items = append(rep.items, tasks...)

	// 3 + 4 — pin to Quick Work, alternate local/runtime, rotate the adapter every two tasks.
	quickWork, err := findWorkflow(ctx, cs, "Quick Work")
	if err != nil {
		return err
	}
	for i := range tasks {
		a := adapters[(i/2)%len(adapters)]
		mode := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
		if !c.localOnly && i%2 == 1 {
			mode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
		}
		if err := pinTask(ctx, cs, tasks[i], quickWork, mode, c.runtimeImage, workers[a.kind], c.dispatch); err != nil {
			return fmt.Errorf("pin task %d: %w", i+1, err)
		}
		fmt.Printf("task %d/6  : %-8s %-9s %s\n", i+1, a.kind, modeNameExec(mode), truncate(tasks[i].title, 60))
	}

	// 5 — dispatch and wait, unless this is a setup-only run.
	if !c.dispatch {
		fmt.Printf("\n(no -dispatch: the tasks are pinned but not started)\n")
		return nil
	}
	for i := range tasks {
		waitTask(ctx, cs, rep, tasks[i], i+1, c.taskTimeout)
	}
	return nil
}

// askOne exercises one adapter in one mode with a prompt that needs no project knowledge.
func askOne(ctx context.Context, cs *tuiclient.Clients, rep *report, projectID string,
	mode apiv1.ConversationMode, a adapter, timeout time.Duration) {

	const prompt = "In two sentences: what should a team verify first when building a small web app?"
	started := time.Now()
	convID, err := createConversation(ctx, cs, projectID, a.ref, mode, prompt)
	if err != nil {
		rep.asks = append(rep.asks, askRow{mode: modeName(mode), adapter: a.kind, err: err})
		fmt.Printf("ask %-10s %-8s FAILED  %v\n", modeName(mode), a.kind, err)
		return
	}
	reply, err := awaitReply(ctx, cs, convID, timeout)
	rep.asks = append(rep.asks, askRow{
		mode: modeName(mode), adapter: a.kind, elapsed: time.Since(started), err: err,
		reply: firstLines(reply, 2),
	})
	status := "ok"
	if err != nil {
		status = "FAILED  " + err.Error()
	}
	fmt.Printf("ask %-10s %-8s %-7s %s\n", modeName(mode), a.kind, status, time.Since(started).Round(time.Second))
}

// askEpic asks Brainstorm for the weather epic and reports what came back — the count is a
// RESULT read from the project, never an assumption.
func askEpic(ctx context.Context, cs *tuiclient.Clients, rep *report, projectID string,
	a adapter, timeout time.Duration) (string, []workItem) {

	prompt := `Do not ask clarifying questions — create the work items now, then summarise.

Create one epic with exactly six tasks for a basic weather web app on port 9090.

Requirements:
1. C/F readings, toggleable
2. Location finding with real city resolution
3. All searches, filters and buttons work fully
4. Single city, or compare two
5. Temperature, humidity, wind, high/low, and a storm/rain/sunny/cloudy description
6. Hourly and daily
7. News for Tech, Games and Business

Create the epic, then its six tasks as children of it. Finish by stating how many you created.`

	started := time.Now()
	convID, err := createConversation(ctx, cs, projectID, a.ref,
		apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, prompt)
	if err != nil {
		rep.epicErr = err
		fmt.Printf("\nepic ask : FAILED  %v\n", err)
		return "", nil
	}
	reply, err := awaitReply(ctx, cs, convID, timeout)
	rep.epicReply, rep.epicElapsed, rep.epicErr = firstLines(reply, 12), time.Since(started), err

	epicID, tasks := findEpicAndTasks(ctx, cs, projectID)
	rep.epicProduced, rep.epicTitle = len(tasks), titleOr(epicID, "<none>")
	fmt.Printf("\nepic ask : produced epic %q with %d task(s) in %s\n",
		rep.epicTitle, len(tasks), time.Since(started).Round(time.Second))
	if len(tasks) != 6 {
		fmt.Printf("           (the harness will %s to six)\n", pick(len(tasks) < 6, "top up", "trim"))
	}
	return epicID, tasks
}

func createConversation(ctx context.Context, cs *tuiclient.Clients, projectID, modelRef string,
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

// awaitReply polls for the assistant reply: ChatStream is a stream, but the collector persists the
// answer regardless, so polling ListMessages is simpler and cannot half-read a stream.
func awaitReply(ctx context.Context, cs *tuiclient.Clients, convID string, timeout time.Duration) (string, error) {
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

func findEpicAndTasks(ctx context.Context, cs *tuiclient.Clients, projectID string) (string, []workItem) {
	resp, err := cs.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: projectID, PageSize: 200,
	}))
	if err != nil {
		return "", nil
	}
	var epic string
	children := map[string][]workItem{}
	for _, w := range resp.Msg.GetWorkItems() {
		if epic == "" && w.GetKind() == apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC {
			epic = w.GetId()
		}
		children[w.GetParentId()] = append(children[w.GetParentId()], workItem{
			id: w.GetId(), title: w.GetTitle(), status: w.GetStatus().String(),
		})
	}
	if epic == "" {
		return "", nil
	}
	return epic, children[epic]
}

func topUp(ctx context.Context, cs *tuiclient.Clients, projectID, epicID string, tasks []workItem, want int) []workItem {
	if len(tasks) > want {
		return tasks[:want]
	}
	for i := len(tasks); i < want; i++ {
		title := fmt.Sprintf("Requirement %d (harness-created)", i+1)
		id, err := createWorkItem(ctx, cs, projectID, epicID, apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, title,
			"Added because the Ask produced fewer than six tasks.")
		if err != nil {
			fmt.Printf("top-up   : could not create task %d: %v\n", i+1, err)
			break
		}
		tasks = append(tasks, workItem{id: id, title: title})
	}
	return tasks
}

func createWorkItem(ctx context.Context, cs *tuiclient.Clients, projectID, parentID string,
	kind apiv1.WorkItemKind, title, desc string) (string, error) {

	resp, err := cs.WorkItems.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: projectID, ParentId: parentID, Kind: kind, Title: title, Description: desc,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetWorkItem().GetId(), nil
}

func pinTask(ctx context.Context, cs *tuiclient.Clients, t workItem, workflowID string,
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

func waitTask(ctx context.Context, cs *tuiclient.Clients, rep *report, t workItem, n int, timeout time.Duration) {
	started := time.Now()
	deadline := started.Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := cs.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: t.id}))
		if err == nil {
			st := resp.Msg.GetWorkItem().GetStatus().String()
			if isTerminalStatus(st) {
				rep.tasks = append(rep.tasks, taskRow{n: n, title: t.title, status: short(st), elapsed: time.Since(started)})
				fmt.Printf("run %d/6    : %-12s %s (%s)\n", n, short(st), truncate(t.title, 50),
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

func probe(ctx context.Context, cs *tuiclient.Clients) error {
	_, err := cs.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{}))
	return err
}

func createProject(ctx context.Context, cs *tuiclient.Clients, prefix string) (string, error) {
	slug := fmt.Sprintf("%s-%d", prefix, time.Now().Unix())
	resp, err := cs.Projects.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: slug, Slug: slug, GitStrategy: apiv1.GitStrategy_GIT_STRATEGY_NONE,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetProject().GetId(), nil
}

// createWorkers makes ONE worker per adapter, because the adapter is chosen by the worker's
// model_ref — the seeded workers mostly carry none, so reusing them would test nothing.
func createWorkers(ctx context.Context, cs *tuiclient.Clients, adapters []adapter) (map[string]worker, error) {
	out := map[string]worker{}
	for _, a := range adapters {
		resp, err := cs.Workers.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
			Name:    fmt.Sprintf("e2e-%s-%d", a.kind, time.Now().Unix()),
			ModelRef: a.ref,
			Purpose: "Harness worker for the adapter/mode matrix run.",
		}))
		if err != nil {
			return out, fmt.Errorf("create %s worker: %w", a.kind, err)
		}
		id := resp.Msg.GetWorker().GetId()
		if _, err := cs.Workers.PublishWorkerVersion(ctx, connect.NewRequest(&apiv1.PublishWorkerVersionRequest{
			WorkerId: id, VersionNote: "harness",
		})); err != nil {
			out[a.kind] = worker{id: id, version: resp.Msg.GetVersion().GetVersion()}
			return out, fmt.Errorf("publish %s worker: %w", a.kind, err)
		}
		out[a.kind] = worker{id: id, version: resp.Msg.GetVersion().GetVersion()}
	}
	return out, nil
}

func findWorkflow(ctx context.Context, cs *tuiclient.Clients, name string) (string, error) {
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

func resolveToken() string {
	if t := strings.TrimSpace(os.Getenv("ORCHICON_TOKEN")); t != "" {
		return t
	}
	dir := os.Getenv("ORCHICON_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".orchicon")
	}
	body, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		return ""
	}
	return regexp.MustCompile(`oc_[A-Za-z0-9._-]{8,}`).FindString(string(body))
}
