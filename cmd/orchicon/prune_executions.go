package main

// prune_executions.go — the operator-invoked execution retention pass.
//
// The operator: "I want to keep the 150 most recent executions and delete any other execution record. The
// goal is just to clean up my console a little. 3500 executions roughly is a lot to keep in history and is
// unnecessary."
//
// WHY A CLI VERB RATHER THAN A BUTTON. The act is irreversible and it is not a per-row decision, so it
// wants an explicit, scriptable, re-runnable entry point that prints what it is about to do before it does
// it — and the TUI's own bulk delete cannot express it at all (one RPC per marked row, one 30s deadline
// shared across the batch, so a large selection dies part-way; and marking thousands of rows by hand is
// not a gesture anyone can perform).
//
// Usage:
//
//	orchicon prune-executions --keep 150            # report only (the default — nothing is deleted)
//	orchicon prune-executions --keep 150 --apply    # actually delete
//
// THE DEFAULT IS THE DRY RUN, deliberately. A destructive verb whose bare invocation deletes is a verb
// that deletes on a typo; this one has to be told twice, and the report it prints is produced by the same
// code path that performs the delete, so the numbers cannot disagree.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
)

func runPruneExecutions(args []string, log *slog.Logger) int {
	keep := 150
	apply := false
	batch := 500

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--apply":
			apply = true
		case a == "--dry-run":
			apply = false
		case a == "-h" || a == "--help":
			fmt.Println("Usage: orchicon prune-executions [--keep N] [--batch N] [--apply]")
			fmt.Println()
			fmt.Println("Deletes every execution older than the newest N for this deployment's tenant,")
			fmt.Println("together with its session transcript and recovery rows. Usage records are KEPT")
			fmt.Println("(de-linked) because they are the tenant's spend ledger.")
			fmt.Println()
			fmt.Println("  --keep N    how many of the most recent executions to keep (default 150)")
			fmt.Println("  --batch N   executions per delete statement (default 500)")
			fmt.Println("  --apply     actually delete; without it this only reports what would be removed")
			return 0
		case a == "--keep" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "--keep needs a non-negative integer, got %q\n", args[i])
				return 1
			}
			keep = n
		case strings.HasPrefix(a, "--keep="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--keep="))
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "--keep needs a non-negative integer, got %q\n", a)
				return 1
			}
			keep = n
		case a == "--batch" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "--batch needs a positive integer, got %q\n", args[i])
				return 1
			}
			batch = n
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q (try --help)\n", a)
			return 1
		}
	}

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("db open", "error", err)
		return 1
	}
	defer pool.Close()

	tenantID := cfg.DeploymentTenantID

	if !apply {
		plan, err := pool.PlanExecutionRetention(ctx, tenantID, keep)
		if err != nil {
			log.Error("plan execution retention", "error", err)
			return 1
		}
		printExecutionRetentionPlan(plan, false)
		fmt.Println()
		fmt.Println("Nothing was deleted. Re-run with --apply to perform this.")
		return 0
	}

	plan, err := pool.PruneExecutionHistory(ctx, tenantID, keep, batch)
	if err != nil {
		log.Error("prune execution history", "error", err)
		printExecutionRetentionPlan(plan, true)
		return 1
	}
	printExecutionRetentionPlan(plan, true)
	return 0
}

// printExecutionRetentionPlan reports the prune's scope in the terms the operator reasons in — how many
// executions, WHICH WINDOW of time, and what child records go with them.
func printExecutionRetentionPlan(p db.ExecutionRetentionPlan, applied bool) {
	verb := "would remove"
	if applied {
		verb = "removed"
	}
	fmt.Printf("tenant:            %s\n", p.TenantID)
	fmt.Printf("executions total:  %d\n", p.TotalExecutions)
	fmt.Printf("keeping newest:    %d\n", p.KeepExecutions)
	if p.KeepBoundary != nil {
		fmt.Printf("keep boundary:     %s (survives; everything older goes)\n", p.KeepBoundary.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Printf("executions to %s: %d\n", verb, p.DeleteExecutions)
	if p.OldestDeleted != nil && p.NewestDeleted != nil {
		fmt.Printf("removal window:    %s  ..  %s\n",
			p.OldestDeleted.Format("2006-01-02 15:04:05"), p.NewestDeleted.Format("2006-01-02 15:04:05"))
	}
	if p.LiveAmongDelete > 0 {
		fmt.Printf("STILL LIVE:        %d  (a prune refuses while any candidate is live)\n", p.LiveAmongDelete)
	}
	fmt.Println()
	fmt.Println("child records:")
	fmt.Printf("  session transcript parts   %d\n", p.SessionParts)
	fmt.Printf("  checkpoints                %d\n", p.Checkpoints)
	fmt.Printf("  recovery executions        %d\n", p.RecoveryExecs)
	fmt.Printf("  recovery step runs         %d\n", p.RecoveryStepRuns)
	fmt.Printf("  workflow step-run refs     %d  (reference cleared, step runs kept)\n", p.StepRunRefs)
	fmt.Printf("  usage records de-linked    %d  (KEPT: the spend ledger)\n", p.UsageRecords)
	fmt.Printf("  usage records on survivors %d\n", p.UsageRecordsKept)
	fmt.Printf("  worktree dirs on disk      %d\n", p.WorktreeDirsOnDisk)
	if applied {
		fmt.Printf("  worktrees removed          %d\n", p.WorktreesRemoved)
		if p.WorktreeError != nil {
			fmt.Printf("  worktree removal error:    %v\n", p.WorktreeError)
		}
	}
}
