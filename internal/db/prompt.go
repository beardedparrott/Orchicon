package db

import (
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// WorkerIdentityPreamble is prepended to every worker system prompt so the
// model knows it is an autonomous Orchicon worker, not a human operator or an
// interactive session. It is the identity statement that distinguishes an
// in-Orchicon worker (operates autonomously, reports via the ORCHICON WORKER
// SUMMARY contract) from a human-facing session (must ask before
// PRing/merging). Both composite builders (the scheduler's
// buildStandaloneComposite and the workflow buildCompositePrompt) emit it so
// every dispatch carries the same self-definition. Kept in sync with
// cannedWorkerIdentity (the first sentence) in seed_workers.go — they live in
// the same package so a drift is immediately visible.
const WorkerIdentityPreamble = "You are an autonomous worker running inside the Orchicon orchestration platform. " +
	"You are not a human operator and there is no human attached to this run. " +
	"You execute one assigned work item per run, operate within your role and the project's acceptance criteria, " +
	"and report your result via the ORCHICON WORKER SUMMARY contract at the end of your output. " +
	"Work autonomously to completion; do not wait for interactive approval for work that is within your assigned scope.\n\n"

// efficiencyBlock is the shared "reduce token overhead" directive injected
// into the stable prompt prefix of every worker (StablePromptPrefix). It has
// two parts:
//
//   - Tool-output discipline: prefer compact command output (short git/gh
//     forms) and bounded reads of `.orchicon/<run>/` files, because every
//     output token the model consumes is context it must process on every
//     later round-trip.
//   - Tool-call batching: fewer, larger tool calls. Each tool call re-sends
//     the whole accumulated conversation to the model, so the NUMBER of
//     round-trips dominates local-model cost more than any single output.
//
// The guardrail is explicit: compact commands are still REAL commands — the
// "Verify, don't assume" contract survives. Batching combines commands, never
// results; a worker must not fabricate a combined output it did not observe.
const efficiencyBlock = "\n## Efficiency — minimize tool output and tool calls\n" +
	"- **Do not spawn subagents or delegate work.** Orchicon already splits your workflow into focused sibling steps (each with its own worker). A subagent re-prepends its system prompt and re-carries the parent's history, roughly doubling context cost. Do the work directly in this session.\n" +
	"- **Minimize tool output.** Prefer compact formats: `git status --short`, `git log --oneline -5`, `git branch --list`, single-line `gh` queries. When reading `.orchicon/<run>/` files, read `summary`, `facts_learned`, and `issues` in full, but only `grep` `touched_files` for paths relevant to your task — do not read every touched file wholesale.\n" +
	"- **Batch your tool calls — split calls are FORBIDDEN.** Multiple split tool calls in a row are NOT allowed. Read multiple files in a single `read` call, combine related commands into ONE `bash` call (e.g. one `git status && git log`), and prefer `grep`/`glob` over sequential reads. Each tool call re-sends the whole conversation to the model, so every split call multiplies cost. If you have more than one tool call to make, you MUST combine them into a single round-trip.\n" +
	"- **Watch your working directory.** You operate inside your project directory (or an isolated worktree of it). In the runtime container `/tmp` is a private tmpfs: write scratch under `/tmp/orchicon/` (writable; see Runtime environment) and keep all real work + final artifacts inside the project. Do not fight a blocked path — choose a writable one.\n" +
	"- Compact commands are still real commands: you MUST verify state with actual tool calls and never fabricate output. Batching combines commands, not results — never invent a combined result you did not observe.\n\n"

// todoListBlock is the shared "## Todo list" guidance block injected into
// the stable prompt prefix of every worker (StablePromptPrefix). It mirrors
// opencode's todowrite tool guidance so workers proactively maintain a
// structured task list that the execution UI surfaces live with an X/Y
// progress counter. The `todowrite` tool ships with the opencode runtime's
// built-in tool set; this block is what tells the worker to actually use it
// (replacement semantics, one in_progress at a time, immediate completion).
// stepDisciplineBlock is the shared "one pass, then deliver" directive
// injected into the stable prompt prefix of every worker. It targets
// turns-to-success rather than per-output size: each tool call re-sends the
// whole accumulated conversation, so the NUMBER of turns dominates cost more
// than any single output (see efficiencyBlock, which this complements). It has
// three parts:
//
//   - One-pass context: gather everything you need in ONE batched tool
//     round-trip before acting; re-read only on a real failure. Sequential
//     single-purpose reads are the main turn-burner observed in practice.
//   - Deliverable-not-journey: for design/review/decision steps the deliverable
//     is the DECISION and the concrete DELTA (ADR + file/function change list),
//     not the step-by-step verification narrative that produced it. Downstream
//     steps inherit the facts, not the process.
//   - Verify once, stop: establish each fact once from a real command, never
//     re-derive what a prior step already recorded, and stop once the
//     acceptance criteria are met — no gold-plating or re-verification.
const stepOutputBlock = "\n## Step output — deliver the decision and the delta, not the journey\n" +
	"- **Gather context in one pass.** Before the first action, batch ALL reads and probes into a single tool round-trip (one `bash` chained with `&&`, one `read` covering several paths, `grep`/`glob` instead of sequential reads). Then act. Re-read only when a call actually fails — do not meter context out one read per turn.\n" +
	"- **Deliver the decision + delta, not the verification.** For design, review, and decision steps, your deliverable is the DECISION and the concrete DELTA: the ADR (Context → Decision → Consequences), the exact file(s) and function(s) to change, and why. Do not narrate the sequence of reads/checks that produced it. Later steps inherit the facts, not your process.\n" +
	"- **Verify once, then stop.** Establish each fact once, from a real tool call; never re-derive something a prior step or the run's `facts_learned` already records. Stop the moment the acceptance criteria are met — resist extra corroboration, gold-plating, or re-verifying settled state. Every extra turn re-sends the whole conversation, so finishing is worth more than polishing.\n\n"

const todoListBlock = "\n## Todo list — maintain it EVERY turn\n" +
	"- **Emit a `todowrite` call after every tool call or turn boundary** while the task is in progress — this is not an end-of-task summary. The operator watches this list live, so a stale or absent list reads as no progress.\n" +
	"- **Before your first action**, write the plan as `todowrite` with concrete items and one item `in_progress`. Then keep it current: update items to `completed`/`in_progress`/`cancelled` as work actually progresses, **each turn**.\n" +
	"- Use it **proactively for multi-step work (roughly 3+ steps)**: break the task into specific, actionable items and track each one.\n" +
	"- Keep exactly **one** item `in_progress` at a time.\n" +
	"- Mark items `completed` **immediately** as the work finishes — never batch completions at the end.\n" +
	"- Mark items `cancelled` when they become irrelevant instead of silently dropping them.\n" +
	"- `todowrite` replaces the whole list on every call: always send the full updated array of `{content, status, priority}` items, using only `pending | in_progress | completed | cancelled` statuses.\n" +
	"- The list is surfaced live in the execution UI — keep it accurate so the operator can track where you are at a glance.\n\n"

// RuntimeEnvironmentBlock is the machine-generated "## Runtime environment"
// section of the stable prompt prefix. It tells the worker the ground truth
// about its execution sandbox so it does not waste cycles empirically probing
// the container (and so it uses the rootless system-library escape hatch
// instead of hitting a wall).
func RuntimeEnvironmentBlock(image string) string {
	img := strings.TrimSpace(image)
	if img == "" {
		img = "the default Orchicon runtime base image"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n## Runtime environment\n\n")
	fmt.Fprintf(&sb, "You are running inside an ephemeral, rootless Linux container (`%s`). Everything you install is wiped when the workflow run ends, so only save durable work to the project directory.\n\n", img)
	sb.WriteString("- **Scratch directory:** `/tmp/orchicon` is the ONE place outside the project you may read and write. Put ephemeral files there (screenshots, logs, downloaded artifacts you need to inspect). It is wiped at run end — never put durable work there, and always save final outputs to the project directory.\n")
	sb.WriteString("- `/tmp` is an **exec-capable** tmpfs here: Go's default TMPDIR works, so `go test` / `make ci` run without relocating your build dir. Older runs may record a contrary \"`/tmp` is noexec\" fact — that was fixed; trust this block.\n")
	sb.WriteString("- You are **not root** and cannot become root: `sudo` is blocked and `apt-get` refuses to run without root. Do not attempt them.\n")
	sb.WriteString("- You may install tools freely into the ephemeral filesystem with the user-space package managers that ship in the image: `pip install` (PIP_BREAK_SYSTEM_PACKAGES is set), `npm install`, `mise install <tool>`, `uv`, `bun`, `curl`. These need no root and are wiped at run end.\n")
	sb.WriteString("- System packages are baked at build time; `apt-get install` will not work. If you need a system shared library that is missing (e.g. `libGL.so.1` for a GUI toolkit), fetch and extract it without root:\n\n")
	sb.WriteString("    apt-get download <pkg> && dpkg-deb -x <pkg>*.deb /tmp/libs && export LD_LIBRARY_PATH=/tmp/libs/usr/lib/x86_64-linux-gnu:$LD_LIBRARY_PATH\n\n")
	sb.WriteString("- There is no X server and usually no offscreen graphics libs. Prefer headless modes for GUI toolkits (e.g. `QT_QPA_PLATFORM=offscreen`), or install the missing libs with the pattern above.\n")
	return sb.String()
}

// StablePromptPrefix is the byte-identical shared prefix prepended to every
// worker composite prompt. llama.cpp's KV/prompt cache is prefix-based: when
// the FIRST tokens of every worker's prompt are identical, the shared prefix
// is reused across steps, roles, and runs (local-model runs pay the prefill
// cost once, not per step). It is built ONLY from shared constants — worker
// identity + safety rules + efficiency directives + runtime environment —
// never from per-worker strings. Role/worker-specific AGENTS.md content, the
// task, execution history, facts, and instructions follow AFTER the prefix.
//
// Git/branch guidance is deliberately NOT part of the prefix: it is injected
// per-run and keyed on the run's worktree_status (GitGuidanceBlock), so a
// non-repo run is never told a branch exists. Keeping the prefix git-neutral
// also preserves KV-cache sharing across repo and non-repo runs.
//
// The runtime image is per-run (all steps of a run dispatch the same work
// item, so it is constant within a run), which is what makes the prefix
// identical across the steps of a run.
func StablePromptPrefix(runtimeImage string) string {
	var sb strings.Builder
	sb.WriteString(WorkerIdentityPreamble)
	sb.WriteString(safetyBlock)
	sb.WriteString(efficiencyBlock)
	sb.WriteString(stepOutputBlock)
	sb.WriteString(todoListBlock)
	sb.WriteString(RuntimeEnvironmentBlock(runtimeImage))
	return sb.String()
}

// GitGuidanceBlock emits the per-run git/branch guidance section, keyed on
// the run's worktree_status (the same signal that drives the execution cwd).
//
//   - ready → the develop-first git discipline block: work on the branch
//     recorded for this run (never push/PR/merge to main).
//   - anything else (skipped/pending/failed/pruned, or no project) → an
//     in-place block: this run works directly in project_dir, no branch or
//     worktree, so the worker must not create branches/commit/push/PR.
//
// It is the single source of git guidance for a dispatch (replacing the old
// unconditional prefix block + canned AGENTS.md blocks), so a non-repo run is
// never instructed to work on a branch. Returns "" when the run is git-backed
// but no branch is recorded yet (ready with empty branch is not expected).
func GitGuidanceBlock(worktreeStatus, worktreeBranch, projectDir string) string {
	if worktreeStatus == domain.WorktreeReady && worktreeBranch != "" {
		return "\n## Git discipline\n" +
			"- This run is git-backed and works on the branch `" + worktreeBranch + "` recorded for it.\n" +
			"- Work on a branch created off `develop` (the integration branch where all work lands). **NEVER** commit to, push to, or open a PR into `main` or `develop` directly.\n" +
			"- Use the branch recorded for this run (`" + worktreeBranch + "`); do not create a new branch unless the previous work was on `main`.\n" +
			"- You do not open the pull request or merge it — the DevOps Engineer step creates the PR and merges into `develop` after approval.\n\n"
	}
	// Non-repo (or not-yet-git-backed) run: work in place, no branch.
	if projectDir != "" {
		return "\n## Git discipline\n" +
			"- This run works in place in `" + projectDir + "`. There is no branch or worktree for this run.\n" +
			"- Do not create branches, commit, push, or open pull requests; work directly in the project directory.\n\n"
	}
	return "\n## Git discipline\n" +
		"- This run has no git branch or worktree. Do not create branches, commit, push, or open pull requests; work in place.\n\n"
}

// WorkItemAttachmentsManifest returns a markdown block listing work item attachments for the worker prompt.
// It is the Channel B visibility (file-materialized + manifest line) per ADR-4.
func WorkItemAttachmentsManifest(attachments []WorkItemAttachmentRow, workItemID string) string {
    if len(attachments)==0 { return "" }
    var b strings.Builder
    b.WriteString("## Work item attachments\n")
    for _, a := range attachments {
        b.WriteString(fmt.Sprintf("- %s (%s, %d bytes) at .orchicon/work-item-attachments/%s/%s\n", a.Name, a.MimeType, a.SizeBytes, workItemID, a.Name))
    }
    b.WriteString("\n")
    return b.String()
}

