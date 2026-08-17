package db

import (
	"fmt"
	"strings"
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
	"- **Minimize tool output.** Prefer compact formats: `git status --short`, `git log --oneline -5`, `git branch --list`, single-line `gh` queries. When reading `.orchicon/<run>/` files, read `summary`, `facts_learned`, and `issues` in full, but only `grep` `touched_files` for paths relevant to your task — do not read every touched file wholesale.\n" +
	"- **Batch your tool calls.** Read multiple files in a single `read` call (use `limit`/`offset` or read several paths), combine related commands into one `bash` call (e.g. one `git status && git log`), and prefer `grep`/`glob` over sequential reads. Avoid micro tool calls: each call re-sends the whole conversation to the model, so fewer, larger calls are dramatically cheaper.\n" +
	"- Compact commands are still real commands: you MUST verify state with actual tool calls and never fabricate output. Batching combines commands, not results — never invent a combined result you did not observe.\n\n"

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
// The runtime image is per-run (all steps of a run dispatch the same work
// item, so it is constant within a run), which is what makes the prefix
// identical across the steps of a run.
func StablePromptPrefix(runtimeImage string) string {
	var sb strings.Builder
	sb.WriteString(WorkerIdentityPreamble)
	sb.WriteString(safetyBlock)
	sb.WriteString(efficiencyBlock)
	sb.WriteString(RuntimeEnvironmentBlock(runtimeImage))
	return sb.String()
}
