package e2e

// Runner is the adapter/mode matrix. It lives in internal/e2e and is exposed as
// `orchicon e2e`, so it ships inside the binary an operator INSTALLS — not as a `make` target,
// which a Windows or macOS install has neither a Makefile nor a Go toolchain to run.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// Run parses args and executes the matrix. Returns an error for a non-zero exit.
func Run(args []string) error {
	fs := flag.NewFlagSet("orchicon e2e", flag.ContinueOnError)
	var (
		instance    = fs.String("instance", "", "dev|test|prod — target that instance's control port and ITS saved credential")
		profile     = fs.String("profile", "", "orch profile to use (default: $ORCHICON_PROFILE, else the active profile)")
		baseURL     = fs.String("url", "", "plane base URL (default: the resolved profile's, else $ORCHICON_URL)")
		token       = fs.String("token", "", "bearer token (default: the resolved profile's, else $ORCHICON_TOKEN)")
		projectID   = fs.String("project", "", "reuse this project (default: create a throwaway one)")
		// NO DEFAULT TAG, DELIBERATELY — see resolveRuntimeImage. A literal here is a tag that exists on ONE
		// machine: the harness defaulted to "orchicon-runtime:orchicon-dev", which is what a Linux dev host
		// happens to build, while a Windows plane's catalogue row for the same variant is
		// "orchicon-runtime:dev". On Windows every runtime task then failed at image lookup — and before
		// failRunAtStart recorded its reason, with NO error in the UI at all.
		runtimeImg  = fs.String("runtime-image", "", "image for runtime-mode tasks (default: this plane's own dev-variant tag, resolved from ListRuntimeImages)")
		localOnly   = fs.Bool("local-only", false, "skip runtime-mode tasks (no container engine, e.g. a cloud Mac)")
		cleanupAfter = fs.Bool("cleanup", false,
			"REMOVE everything this run created (default: leave the project, workers and tasks in place)")
		launch = fs.Bool("launch", false,
			"after setup, run the six tasks ONE AT A TIME, each with a different adapter")
		launchOnly = fs.Bool("launch-only", false,
			"skip setup and launch the tasks already in -project (re-pointing each at a different adapter)")
		cleanupOnly = fs.Bool("cleanup-only", false,
			"remove what an EARLIER run left in -project: its work items, the per-adapter workflows and its workers")
		out         = fs.String("out", "", "results file (default orchicon-e2e-<stamp>.md in the cwd)")
		askTimeout  = fs.Duration("ask-timeout", 4*time.Minute, "per-Ask timeout")
		taskTimeout = fs.Duration("task-timeout", 20*time.Minute, "per-task dispatch timeout")
		orchRef     = fs.String("orchicon-model", "orchicon/deepseek/deepseek-v4-flash", "model_ref for the orchicon adapter")
		claudeRef   = fs.String("claude-model", "", "model_ref for the claude adapter, e.g. claude/anthropic/claude-sonnet-4 (empty: skip)")
		openRef     = fs.String("opencode-model", "opencode/deepseek/deepseek-v4-flash", "model_ref for the opencode adapter (empty: skip)")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	c := opts{
		projectID: *projectID, runtimeImage: *runtimeImg, localOnly: *localOnly, cleanup: *cleanupAfter,
		launch: *launch, launchOnly: *launchOnly, cleanupOnly: *cleanupOnly,
		out: *out, askTimeout: *askTimeout, taskTimeout: *taskTimeout,
		orchiconRef: *orchRef, claudeRef: *claudeRef, opencodeRef: *openRef,
	}

	// CREDENTIALS COME FROM THE SAME PLACE `orch` USES, because that is what is already configured on an
	// installed box. The flags and the environment override it, in that order.
	if err := resolveConnection(&c, *instance, *profile, *baseURL, *token); err != nil {
		return err
	}
	if c.out == "" {
		c.out = fmt.Sprintf("orchicon-e2e-%s.md", time.Now().Format("20060102-150405"))
	}

	// THE REFRESH TOKEN MATTERS ON AN INSTALLED BOX. The profile's access token is short-lived (~15 min), so
	// a harness that passed only Token would fail `unauthenticated` on almost every run and look like a
	// broken plane. Passing the refresh token lets the client set renew it exactly as `orch` does.
	// AN INTERRUPTED RUN MUST STILL CLEAN UP. Go's signal default kills the process outright, so a Ctrl-C
	// (or a supervising timeout's SIGTERM) would skip every defer and leave the project, workers and work
	// items on the operator's plane — which is exactly what happened while testing this. Notifying puts the
	// signal on a channel instead, so the run unwinds through its defers and removes what it created.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cs := client.New(client.Options{
		BaseURL:            c.baseURL,
		Token:              c.token,
		RefreshToken:       c.refreshToken,
		InsecureSkipVerify: c.insecureTLS,
		Timeout:            60 * time.Second,
	})
	rep := &report{baseURL: c.baseURL, started: time.Now()}
	defer rep.write(c.out, c)

	fmt.Printf("plane    : %s\n", c.baseURL)
	fmt.Printf("token    : %s\n", redact(c.token))
	if err := probe(ctx, cs); err != nil {
		return fmt.Errorf("plane not reachable — is it running and the credential valid? (%w)", err)
	}

	// THE RUNTIME IMAGE IS THE PLANE'S OWN TAG, NOT A LITERAL — resolved AFTER probe, because it needs a
	// reachable plane, and BEFORE anything binds a task to a workflow.
	if !c.localOnly {
		tag, why, err := resolveRuntimeImage(ctx, cs, c.runtimeImage)
		if err != nil {
			return err
		}
		c.runtimeImage = tag
		if why != "" {
			fmt.Printf("image    : %s (%s)\n", tag, why)
		} else {
			fmt.Printf("image    : %s\n", tag)
		}
	}

	adapters := c.adapters()
	if len(adapters) == 0 {
		return errors.New("no adapters selected: pass -orchicon-model, -claude-model and/or -opencode-model")
	}
	fmt.Printf("adapters : %s\n", strings.Join(adapterNames(adapters), ", "))
	if c.localOnly {
		fmt.Printf("modes    : local only (-local-only)\n")
	} else {
		fmt.Printf("modes    : local + runtime (%s)\n", c.runtimeImage)
	}

	// -cleanup ON ITS OWN IS A CLEANUP PASS, NOT A SETUP THAT ENDS IN ONE.
	// It used to run the whole setup first — creating a project, workflows and six asks — and only remove
	// them in a deferred function, so asking for a cleanup looked like a hang while it built the very things
	// it was about to delete.
	if c.cleanupOnly || (c.cleanup && !c.launch && !c.launchOnly) {
		return runCleanupOnly(ctx, cs, c, adapters)
	}

	if c.launchOnly {
		// LAUNCH AN EXISTING SETUP. This is the workflow the artifacts exist for: set up once, then run the
		// matrix against a DIFFERENT set of adapters without recreating anything.
		if strings.TrimSpace(c.projectID) == "" {
			return errors.New("-launch-only needs -project <id> (the project an earlier setup pass left behind)")
		}
		// THE WORKFLOWS ARE THE LEVER, so an existing setup is resolved by finding them. They are created by
		// the setup pass and named per adapter, so this needs no stored state.
		workflows, ferr := findAdapterWorkflows(ctx, cs, adapters)
		if ferr != nil {
			return ferr
		}
		workflowCache[c.projectID] = workflows
		rep.projectID = c.projectID
		fmt.Printf("project  : %s (existing)\n\n", c.projectID)
		return runLaunches(ctx, cs, rep, c.projectID, adapters, c)
	}

	projectID2, ownProject := c.projectID, false
	if projectID2 == "" {
		dir := c.projectDir
		if dir == "" {
			dir = filepath.Join(os.TempDir(), fmt.Sprintf("orchicon-e2e-%d", time.Now().Unix()))
		}
		c.projectDir = dir
		id, err := createProject(ctx, cs, "e2e-adapter-matrix", dir)
		if err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		projectID2, ownProject = id, true
	}
	rep.projectID, rep.projectDir = projectID2, c.projectDir
	fmt.Printf("project  : %s\n\n", projectID2)

	workers, err := createWorkers(ctx, cs, adapters)
	workerCache[c.projectID] = workers
	workerCache[projectID2] = workers

	// THE ROTATION LIVES HERE: one Quick Work copy per adapter, whose step points at that adapter's worker.
	workflows, err := ensureAdapterWorkflows(ctx, cs, projectID2, adapters, workers)
	if err != nil {
		return fmt.Errorf("create the per-adapter workflows: %w", err)
	}
	// A WRONG STEP REF IS SILENT: the matrix would look like three adapters and run one three times.
	if err := verifyWorkflowPins(ctx, cs, workflows, workers); err != nil {
		return err
	}
	workflowCache[c.projectID] = workflows
	workflowCache[projectID2] = workflows
	if err != nil {
		// CLEANUP RUNS ON A LIVE CONTEXT: an interrupt cancels ctx, and a cancelled context would make
		// every delete fail — the precise case where cleanup matters most.
		cleanupWorkers(context.Background(), cs, rep, workers)
		if ownProject {
			removeProject(context.Background(), cs, rep, projectID2)
		}
		return fmt.Errorf("create workers: %w", err)
	}
	defer func() {
		// LEFT IN PLACE BY DEFAULT. The artifacts are the POINT: the operator launches these tasks from the
		// console, one at a time, pointing different adapters and models at them. Removing them by default
		// destroyed exactly what the run existed to produce.
		if !c.cleanup {
			rep.kept = true
			if ctx.Err() != nil {
				fmt.Printf("\ninterrupted — everything created is LEFT IN PLACE (pass -cleanup to remove it)\n")
			}
			fmt.Printf("\nkept     : project %s, %d worker(s) and the six tasks are in place\n", projectID2, len(workers))
			fmt.Printf("           launch them from the console, or chain them with: -launch-only -project %s\n", projectID2)
			return
		}
		if ctx.Err() != nil {
			fmt.Printf("\ninterrupted — cleaning up what this run created\n")
		}
		removeEverything(context.Background(), cs, rep, workers, workflows, projectID2, ownProject)
		if ownProject && c.projectDir != "" {
			if rerr := os.RemoveAll(c.projectDir); rerr != nil {
				fmt.Printf("cleanup  : could not remove %s: %v\n", c.projectDir, rerr)
			}
		}
	}()

	// 1 — Ask once per mode per adapter.
	for _, m := range []apiv1.ConversationMode{
		apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
		apiv1.ConversationMode_CONVERSATION_MODE_ITERATION,
	} {
		for _, a := range adapters {
			askOne(ctx, cs, rep, projectID2, m, a, c.askTimeout)
		}
	}

	// 2 — the epic and its six tasks, created by us. The Ask is exercised in step 1; it does not own the
	// plan, because a model that answers with a clarifying question leaves the matrix with nothing to run.
	_, tasks, err := createPlan(ctx, cs, rep, projectID2)
	if err != nil {
		return err
	}

	// 3 + 4 — bind each task to ITS ADAPTER's Quick Work copy, alternating local/runtime.
	//
	// THE WORKFLOW IS THE LEVER. The dispatcher resolves the worker from the workflow STEP's ref, so binding
	// the task to a per-adapter workflow is what makes this task run that adapter. An earlier version bound
	// every task to Quick Work and re-assigned the ITEM's worker, which the bound-workflow path never reads —
	// so all six ran one adapter and the matrix looked fine.
	for i := range tasks {
		a := adapters[(i/2)%len(adapters)]
		mode := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
		if !c.localOnly && i%2 == 1 {
			mode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
		}
		if err := bindTaskToWorkflow(ctx, cs, tasks[i].id, workflows[a.kind], mode, c.runtimeImage); err != nil {
			return fmt.Errorf("pin task %d: %w", i+1, err)
		}
		rep.plan = append(rep.plan, matrixRow{n: i + 1, adapter: a.kind, mode: modeString(mode), title: tasks[i].title})
		fmt.Printf("task %d/6  : %-8s %-9s %s\n", i+1, a.kind, modeString(mode), truncate(tasks[i].title, 60))
	}

	// 5 — optionally launch them one at a time, each re-pointed at a different adapter.
	if c.launch {
		return runLaunches(ctx, cs, rep, projectID2, adapters, c)
	}
	fmt.Printf("\n(pinned but not started — add -launch to run them one at a time)\n")
	return nil
}

// runLaunches drives the six tasks sequentially, re-binding each to its adapter's worker first.
func runLaunches(ctx context.Context, cs *client.Clients, rep *report, projectID string,
	adapters []adapter, c opts) error {

	wf, ok := workflowCache[projectID]
	if !ok || len(wf) == 0 {
		return fmt.Errorf("no per-adapter workflows known for project %s — run the setup pass (which creates "+
			"them) without -launch-only first", projectID)
	}
	plan, err := discoverLaunchPlan(ctx, cs, projectID, adapters, c.localOnly)
	if err != nil {
		return err
	}
	for _, p := range plan {
		if err := launchOne(ctx, cs, rep, p, wf, c.runtimeImage, c.taskTimeout); err != nil {
			return err
		}
	}
	return nil
}

// resolveConnection fills the base URL and token from the flags, then the environment, then the orch
// profile file — the same precedence `orch` itself uses, so an installed box needs no extra setup.
func resolveConnection(c *opts, instance, profileName, urlFlag, tokenFlag string) error {
	var cfg *config.Config
	if path, err := config.DefaultPath(); err == nil {
		if loaded, lerr := config.Load(path); lerr == nil {
			cfg = loaded
		}
	}

	if strings.TrimSpace(instance) != "" {
		// AN INSTANCE IS A URL *AND* A CREDENTIAL, and they must be a matched pair: each instance has its own
		// signing key, so a dev URL with the active (prod) token fails `unauthenticated` with no hint that the
		// two disagreed. So the instance picks the URL, and only a profile ALREADY POINTING AT IT may supply
		// the credential.
		c.baseURL = config.InstanceURL(instance)
		if c.baseURL == "" {
			return fmt.Errorf("-instance %q is not dev, test or prod", instance)
		}
		p := config.ProfileForInstance(cfg, instance)
		if p == nil {
			return fmt.Errorf("no saved `orch` profile points at the %s instance (%s), so there is no credential "+
				"for it.\n  Connect `orch` to that instance once, or pass -token for it, or set ORCHICON_TOKEN.\n"+
				"  (The active profile's token will not work: each instance signs its own.)", instance, c.baseURL)
		}
		c.token, c.refreshToken, c.insecureTLS = p.Token, p.RefreshToken, p.InsecureSkipVerify
	} else if cfg != nil {
		if p := config.ResolveNamed(cfg, profileName); p != nil {
			c.baseURL, c.token = p.URL, p.Token
			c.refreshToken, c.insecureTLS = p.RefreshToken, p.InsecureSkipVerify
		}
	}
	if v := strings.TrimSpace(os.Getenv(config.EnvURL)); v != "" && c.baseURL == "" {
		c.baseURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv(config.EnvToken)); v != "" && c.token == "" {
		c.token = v
	}
	if strings.TrimSpace(urlFlag) != "" {
		c.baseURL = strings.TrimRight(strings.TrimSpace(urlFlag), "/")
	}
	if strings.TrimSpace(tokenFlag) != "" {
		c.token = strings.TrimSpace(tokenFlag)
	}
	if c.baseURL == "" {
		return errors.New("no plane to talk to: sign in with `orch` once (which writes the profile), " +
			"or pass -url/-token, or set ORCHICON_URL/ORCHICON_TOKEN")
	}
	return nil
}

// runCleanupOnly reclaims a setup an EARLIER process created. Cleanup normally runs from a deferred func that
// knows what THIS run made; a setup left in place has no such record, so everything is rediscovered by
// convention — the project's work items, the `e2e-quick-*` workflows and the `e2e-<adapter>-*` workers.
func runCleanupOnly(ctx context.Context, cs *client.Clients, c opts, adapters []adapter) error {
	// NO -project MEANS EVERY E2E PROJECT ON THIS PLANE. The command's job is to reclaim what previous test
	// runs left behind, and requiring the operator to remember an id for that makes it useless for the case
	// it exists for.
	rep := &report{baseURL: c.baseURL, started: time.Now(), projectID: c.projectID}
	projects := []string{c.projectID}
	if strings.TrimSpace(c.projectID) == "" {
		list, lerr := cs.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{}))
		if lerr != nil {
			return fmt.Errorf("list projects: %w", lerr)
		}
		projects = nil
		for _, pr := range list.Msg.GetProjects() {
			if strings.HasPrefix(pr.GetSlug(), "e2e-adapter-matrix-") {
				projects = append(projects, pr.GetId())
			}
		}
		if len(projects) == 0 {
			fmt.Printf("cleanup  : nothing to do — no e2e projects on this plane\n")
			return nil
		}
		fmt.Printf("cleanup  : reclaiming %d e2e project(s)\n", len(projects))
	}
	for _, pid := range projects {
		rep.projectID = pid
		reclaimProject(ctx, cs, rep, pid, adapters)
	}

	if len(rep.cleanupFailures) == 0 {
		fmt.Printf("cleanup  : complete — work items, ask conversations, per-adapter workflows, workers and projects\n")
		return nil
	}
	fmt.Printf("cleanup  : INCOMPLETE — still present: %s\n", strings.Join(rep.cleanupFailures, ", "))
	return nil
}

// reclaimProject removes every artifact attributable to one e2e project, in dependency order.
func reclaimProject(ctx context.Context, cs *client.Clients, rep *report, projectID string, adapters []adapter) {
	fmt.Printf("\nproject  : %s\n", projectID)

	// WORK ITEMS: everything in the project, children first so a parent is never refused for having children.
	items, err := cs.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: projectID, PageSize: 200,
	}))
	if err != nil {
		rep.cleanupFailures = append(rep.cleanupFailures, "list work items ("+err.Error()+")")
		return
	}
	var tasks, epics []string
	for _, w := range items.Msg.GetWorkItems() {
		if w.GetKind() == apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC {
			epics = append(epics, w.GetId())
		} else {
			tasks = append(tasks, w.GetId())
		}
	}
	for _, id := range append(tasks, epics...) {
		if _, derr := cs.WorkItems.DeleteWorkItem(ctx,
			connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: id})); derr != nil {
			rep.cleanupFailures = append(rep.cleanupFailures, "work item "+id)
		}
	}

	// CONVERSATIONS: the Ask chats are attributed to the project, and nothing else reclaims them.
	cleanupConversations(ctx, cs, rep, projectID)

	// WORKFLOWS: rediscovered by name, since no record of them survives a process.
	if wfs, werr := findAdapterWorkflows(ctx, cs, adapters); werr == nil {
		cleanupWorkflows(ctx, cs, rep, wfs)
	}

	// WORKERS: swept by NAME, not by the adapters these flags happen to name — otherwise a setup run that used
	// different adapters leaves workers this cannot see.
	sweepHarnessWorkers(ctx, cs, rep)

	// THE PROJECT ITSELF, and the directory it was given — read from the row, since a cleanup pass has no
	// record of what an earlier process chose.
	if p, perr := cs.Projects.GetProject(ctx, connect.NewRequest(
		&apiv1.GetProjectRequest{Id: projectID})); perr == nil {
		if d := strings.TrimSpace(p.Msg.GetProject().GetProjectDir()); d != "" {
			_ = os.RemoveAll(d)
		}
	}
	removeProject(ctx, cs, rep, projectID)
}
