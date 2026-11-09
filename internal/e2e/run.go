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
	"os/signal"
	"strings"
	"syscall"
	"time"

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
		runtimeImg  = fs.String("runtime-image", "orchicon-runtime:orchicon-dev", "image for runtime-mode tasks")
		localOnly   = fs.Bool("local-only", false, "skip runtime-mode tasks (no container engine, e.g. a cloud Mac)")
		keep        = fs.Bool("keep", false, "do not clean up anything this run created")
		dispatch    = fs.Bool("dispatch", false, "actually dispatch the six tasks (default: set them up only)")
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
		projectID: *projectID, runtimeImage: *runtimeImg, localOnly: *localOnly, keep: *keep,
		dispatch: *dispatch, out: *out, askTimeout: *askTimeout, taskTimeout: *taskTimeout,
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

	projectID2, ownProject := c.projectID, false
	if projectID2 == "" {
		id, err := createProject(ctx, cs, "e2e-adapter-matrix")
		if err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		projectID2, ownProject = id, true
	}
	rep.projectID = projectID2
	fmt.Printf("project  : %s\n\n", projectID2)

	workers, err := createWorkers(ctx, cs, adapters)
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
		if ctx.Err() != nil {
			fmt.Printf("\ninterrupted — cleaning up what this run created\n")
		}
		if c.keep {
			fmt.Printf("\n-keep: left project %s and %d worker(s) in place\n", projectID2, len(workers))
			rep.kept = true
			return
		}
		removeEverything(context.Background(), cs, rep, workers, projectID2, ownProject)
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

	// 2 — Brainstorm the epic, then read back what it actually created.
	epicID, tasks := askEpic(ctx, cs, rep, projectID2, adapters[0], c.askTimeout)
	if epicID == "" {
		id, err := createWorkItem(ctx, cs, projectID2, "", apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
			"Weather web app (harness fallback)", "Created because the Ask produced no epic.")
		if err != nil {
			return fmt.Errorf("create fallback epic: %w", err)
		}
		epicID = id
	}
	tasks = topUp(ctx, cs, projectID2, epicID, tasks, 6)
	rep.items = append(rep.items, workItem{id: epicID})
	rep.items = append(rep.items, tasks...)

	// 3 + 4 — Quick Work, alternating local/runtime, adapter rotating every two tasks.
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
		rep.plan = append(rep.plan, matrixRow{n: i + 1, adapter: a.kind, mode: modeString(mode), title: tasks[i].title})
		fmt.Printf("task %d/6  : %-8s %-9s %s\n", i+1, a.kind, modeString(mode), truncate(tasks[i].title, 60))
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
