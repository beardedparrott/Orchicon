package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestWriteCappedText_Under exercises the small-input path: the body
// fits under the cap and is emitted verbatim, with a single trailing
// newline. Worker prompts rely on this guarantee for short
// descriptions — if the trailing newline disappears, downstream
// sections can smush together.
func TestWriteCappedText_Under(t *testing.T) {
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", "hello world", 1024)
	got := sb.String()
	if !strings.Contains(got, "Output:\nhello world") {
		t.Errorf("expected verbatim body, got: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got: %q", got)
	}
}

// TestWriteCappedText_Over exercises the truncation path. The body
// exceeds the cap and must be cut at a sensible boundary (last
// newline before cap, never mid-word for the chosen cut strategy)
// with a clear marker pointing the worker to the canonical summary.
func TestWriteCappedText_Over(t *testing.T) {
	body := strings.Repeat("lorem ipsum dolor sit amet\n", 500) // ~13K
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", body, 1024)
	got := sb.String()
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker, got: %q", got)
	}
	if !strings.Contains(got, "ORCHICON WORKER SUMMARY") {
		t.Errorf("truncation marker should reference the summary, got: %q", got)
	}
	// The body must end on a complete line, never mid-line, even
	// after the cut. The last data line in the output is the one
	// immediately before the marker (\n…[truncated — ...]).
	cut := strings.Index(got, "…[truncated")
	if cut < 0 {
		t.Fatal("missing truncation marker")
	}
	// Everything before the marker is body + the trailing newline
	// of the cut line. The character just before the marker is
	// therefore a newline (the cut landed on a line boundary).
	pre := got[:cut]
	if !strings.HasSuffix(pre, "\n") {
		t.Errorf("expected cut to land on a newline, got tail: %q", pre[len(pre)-40:])
	}
}

// TestWriteCappedText_NewlineBoundary verifies the cut strategy
// prefers the last newline over a hard byte-cut so we never slice
// a word in half. The input is a single line of 5000 chars (no
// newlines) and the cap is 1024: the cap must trigger, the body
// emits up to 1024 chars (no newline available), and the marker
// follows.
func TestWriteCappedText_NoNewlines(t *testing.T) {
	body := strings.Repeat("a", 5000)
	var sb strings.Builder
	r := &WorkflowReconciler{}
	r.writeCappedText(&sb, "Output", body, 1024)
	got := sb.String()
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker for huge single-line body, got: %q", got[:200])
	}
	// No-newline fallback: the cap is the hard cut. The body chunk
	// should be at most cap bytes (plus a trailing newline before
	// the marker).
	pre := strings.SplitN(got, "\n…", 2)[0]
	if len(pre) > 2000 {
		t.Errorf("body chunk unexpectedly long: %d", len(pre))
	}
}

// TestStepKindLabel covers the human-readable mapping for every kind
// the workflow editor exposes. A regression here would change every
// timeline header in every worker prompt.
func TestStepKindLabel(t *testing.T) {
	cases := map[string]string{
		"task":      "task",
		"decision":  "decision",
		"approval":  "approval",
		"parallel":  "parallel",
		"recover":   "recovery",
		"work_item": "work item",
		"project":   "project",
		"unknown":   "unknown",
	}
	for in, want := range cases {
		if got := stepKindLabel(in); got != want {
			t.Errorf("stepKindLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRuntimeEnvironmentBlock verifies the machine-generated "## Runtime
// environment" section: it names the resolved image, states the rootless
// no-apt ground truth, and documents the rootless system-library escape
// hatch so workers stop empirically probing the container.
func TestRuntimeEnvironmentBlock(t *testing.T) {
	got := runtimeEnvironmentBlock("pyside6-gui:latest")
	for _, want := range []string{
		"## Runtime environment",
		"pyside6-gui:latest",
		"**not root**",
		"apt-get download",
		"dpkg-deb -x",
		"LD_LIBRARY_PATH",
		"QT_QPA_PLATFORM=offscreen",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runtimeEnvironmentBlock missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<prefix>") {
		t.Errorf("runtimeEnvironmentBlock leaked placeholder text")
	}
}

// TestRuntimeEnvironmentBlockEmptyImage falls back to the default base
// image label when the work item has no runtime_image stamped.
func TestRuntimeEnvironmentBlockEmptyImage(t *testing.T) {
	got := runtimeEnvironmentBlock("")
	if !strings.Contains(got, "default Orchicon runtime base image") {
		t.Errorf("expected base-image fallback, got:\n%s", got)
	}
}

// TestStablePromptPrefixSameImageIdentical verifies the stable prompt prefix
// is built ONLY from shared constants (identity + safety + efficiency +
// runtime env): two prefixes for the same runtime image must be byte-identical,
// and a different image only changes the image label.
func TestStablePromptPrefixSameImageIdentical(t *testing.T) {
	a := db.StablePromptPrefix("orchicon-dev:latest")
	b := db.StablePromptPrefix("orchicon-dev:latest")
	if a != b {
		t.Errorf("stable prefix must be byte-identical for the same image")
	}
	if a == db.StablePromptPrefix("orchicon-base:latest") {
		t.Errorf("stable prefix must vary with the runtime image")
	}
}

// TestCompositeStablePrefixSharedAcrossWorkers verifies the acceptance
// criterion for prompt caching: two different workers' composite prompts share
// a common prefix of at least ~1k tokens (~4 chars/token), built from the
// shared identity/safety/efficiency/runtime-env blocks, with the role-specific
// content strictly AFTER the prefix.
func TestCompositeStablePrefixSharedAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	// No ProjectID/ContextFiles → buildCompositePrompt never touches the tx
	// (verified: the only DB reads are gated on those fields), so tx may be nil.
	item := db.WorkItemRow{Title: "Shared prefix", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	swe := db.WorkerVersionRow{
		Role:     "You are a senior full-stack engineer who ships Go and React daily.",
		Skills:   "Go • React • Postgres",
		Behavior: "Write tests alongside implementation.",
		AgentsMD: "## Git workflow\ncommit early and often.\n",
	}
	arch := db.WorkerVersionRow{
		Role:     "You are a principal architect who owns system design.",
		Skills:   "System design • Security • ADRs",
		Behavior: "Think holistically; document trade-offs.",
		AgentsMD: "## Standards\nWrite ADRs for significant decisions.\n",
	}
	r := &WorkflowReconciler{}
	a, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, swe, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, arch, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Common prefix length in bytes.
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	const wantChars = 1000 * 4 // ~1k tokens at the ~4 chars/token heuristic
	if n < wantChars {
		t.Errorf("common prefix = %d chars, want >= %d (~1k tokens); a[:200]=%q b[:200]=%q", n, wantChars, a[:200], b[:200])
	}
	prefix := a[:n]
	for _, want := range []string{
		"## Safety rules (HARD limits)",
		"## Efficiency — minimize tool output and tool calls",
		"Minimize tool output.",
		"Batch your tool calls.",
		"## Runtime environment",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("shared prefix missing %q", want)
		}
	}
	// Role-specific content must come strictly AFTER the stable prefix.
	if i := strings.Index(a, "senior full-stack engineer"); i < n {
		t.Errorf("role text must come after the stable prefix (found at %d, prefix %d)", i, n)
	}
	if i := strings.Index(b, "principal architect"); i < n {
		t.Errorf("role text must come after the stable prefix (found at %d, prefix %d)", i, n)
	}
}

// TestCompositePromptTodoListDirectives verifies every worker's composite
// prompt carries the Todo list guidance block in the stable prefix — the
// todowrite tool is available in the opencode runtime toolset, and the
// prompt must tell the worker to use it proactively for multi-step work
// (acceptance: proactive use for 3+ steps, one in_progress at a time,
// immediate completion, full-list replacement semantics).
func TestCompositePromptTodoListDirectives(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Todo directives", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	worker := db.WorkerVersionRow{Role: "Engineer"}
	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Todo list",
		"todowrite",
		"3+ steps",
		"`in_progress` at a time",
		"`completed`",
		"`cancelled`",
		"pending | in_progress | completed | cancelled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite prompt missing %q; got:\n%s", want, out)
		}
	}

	// The standalone (non-workflow) dispatch path must carry the same block
	// via the shared stable prefix.
	standalone := buildStandaloneComposite(nil, db.ExecutionRow{}, item, worker, "", "")
	if !strings.Contains(standalone, "## Todo list") {
		t.Errorf("standalone composite missing the Todo list block")
	}
}

// TestCompositePromptEfficiencyAndBatchingDirectives verifies every worker's
// composite prompt carries the tool-output-discipline and tool-call-batching
// directives (acceptance: explicit minimize-tool-output + batch-tool-calls
// instructions, and the "Verify, don't assume" guardrail survives).
func TestCompositePromptEfficiencyAndBatchingDirectives(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Efficiency directives", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	worker := db.WorkerVersionRow{Role: "Engineer"}
	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Minimize tool output.",
		"git status --short",
		"git log --oneline -5",
		"`grep` `touched_files`",
		"Batch your tool calls.",
		"fewer, larger calls are dramatically cheaper",
		"you MUST verify state with actual tool calls and never fabricate output",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite prompt missing %q; got:\n%s", want, out)
		}
	}
}

// TestCompositePromptStepOutputDiscipline verifies every worker's composite
// prompt carries the step-output contract in the stable prefix: gather
// context in one batched pass, deliver the decision + delta (not the
// verification narrative), and verify once then stop. This is the
// turns-to-success lever — each tool call re-sends the whole conversation, so
// the number of turns dominates cost more than output size.
func TestCompositePromptStepOutputDiscipline(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Step output discipline", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	worker := db.WorkerVersionRow{Role: "Engineer"}
	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Step output — deliver the decision and the delta, not the journey",
		"Gather context in one pass.",
		"Deliver the decision + delta, not the verification.",
		"Verify once, then stop.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite prompt missing %q; got:\n%s", want, out)
		}
	}
}

// TestCompositePromptNoEmbeddedFactsBlock verifies the embedded
// "## Facts learned (this run)" prompt block is gone — the
// .orchicon/<run>/facts_learned file is now the single source of established
// facts, and the "read facts_learned" instruction is retained.
func TestCompositePromptNoEmbeddedFactsBlock(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Facts dedup", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	// A completed prior step carrying facts, so the old block WOULD have rendered.
	runs := map[string]db.WorkflowStepRunRow{
		"step1": {
			ID:        "sr1",
			Status:    domain.StepRunSucceeded,
			Iteration: 0,
			Result:    []byte(`{"_summary":"Did work.\nFACTS LEARNED: the sandbox plane is up.\n"}`),
		},
	}
	steps := []workflow.StepWire{{ID: "step1", Name: "DevOps Engineer", Kind: "task"}}
	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{Role: "Engineer"}, steps, runs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "## Facts learned (this run)") {
		t.Errorf("embedded facts block must be removed; got:\n%s", out)
	}
	// The file-reading instruction must survive.
	if !strings.Contains(out, "facts_learned") {
		t.Errorf("the read-facts_learned instruction must be retained")
	}
}

// TestCompositePromptGitGuidanceForBareWorker verifies the per-run git/branch
// guidance gating (the non-repo in-place fallback): a run with no worktree
// (worktree_status absent/empty) gets the in-place block and is NEVER told a
// branch exists; a git-backed run (ready + branch) gets the develop-first
// branch block naming its recorded branch. A bare custom worker (empty
// Role/Skills/Behavior/AgentsMD) must not be told to work on a branch when
// there is none — in BOTH the workflow composite and the standalone composite.
func TestCompositePromptGitGuidanceForBareWorker(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Branch discipline", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	bare := db.WorkerVersionRow{} // nothing — a custom worker with no git content

	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, bare, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No worktree → the in-place block, no branch assumption.
	for _, want := range []string{
		"no git branch or worktree",
		"work in place",
		"Do not create branches, commit, push, or open pull requests",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("workflow composite missing %q for a non-repo bare worker; got:\n%s", want, out)
		}
	}
	for _, forbid := range []string{
		"branch created off `develop`",
		"Use the branch recorded for this run",
		"work on the branch",
	} {
		if strings.Contains(out, forbid) {
			t.Errorf("workflow composite must not assume a branch for a non-repo run; found %q", forbid)
		}
	}

	// Standalone (non-workflow) dispatch path must carry the same in-place floor.
	standalone := buildStandaloneComposite(nil, db.ExecutionRow{}, item, bare, "", "")
	for _, want := range []string{
		"no git branch or worktree",
		"Do not create branches, commit, push, or open pull requests",
	} {
		if !strings.Contains(standalone, want) {
			t.Errorf("standalone composite missing %q for a bare worker; got:\n%s", want, standalone)
		}
	}
}

// TestCompositePromptGitGuidanceGitBacked verifies the git-backed branch:
// a run with worktree_status=ready and a recorded branch gets the
// develop-first discipline block naming that branch — never an in-place note.
func TestCompositePromptGitGuidanceGitBacked(t *testing.T) {
	// Simulate a git-backed run: worktree_status=ready, a recorded branch,
	// and a project dir. The block is keyed purely on these params, so a
	// direct call with the params set exercises the git path.
	out := db.GitGuidanceBlock(domain.WorktreeReady, "feat/my-branch", "/tmp/proj")
	for _, want := range []string{
		"## Git discipline",
		"`feat/my-branch`",
		"branch created off `develop`",
		"NEVER** commit to, push to, or open a PR into `main` or `develop`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("git guidance missing %q for a git-backed run; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "work in place") {
		t.Errorf("git-backed run must not get the in-place block; got:\n%s", out)
	}
}

// TestOrchiconLocationNote verifies the helper that tells workers where
// the .orchicon/ directory lives: with a projectDir it interpolates the
// absolute path; without it falls back to a generic note.
func TestOrchiconLocationNote(t *testing.T) {
	got := orchiconLocationNote("")
	for _, want := range []string{
		".orchicon/",
		"project root",
		"not inside the worktree",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("orchiconLocationNote(\"\") missing %q; got %q", want, got)
		}
	}
	got = orchiconLocationNote("/home/user/myproject")
	if !strings.Contains(got, "/home/user/myproject/.orchicon/") {
		t.Errorf("orchiconLocationNote with path missing interpolated dir; got %q", got)
	}
	if !strings.Contains(got, "not inside the worktree") {
		t.Errorf("orchiconLocationNote with path must still say 'not inside the worktree'; got %q", got)
	}
}

// TestCompositePromptOrchiconLocationNote asserts that the composite
// prompt's Instructions section contains the clarifying note that
// .orchicon/ lives at the project root, not the worktree, and that the
// old "from the working directory" phrase is gone.
func TestCompositePromptOrchiconLocationNote(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{Title: "Location note", Status: "pending", RuntimeImage: "orchicon-dev:latest"}
	runs := map[string]db.WorkflowStepRunRow{
		"step1": {ID: "sr1", Status: domain.StepRunSucceeded},
	}
	steps := []workflow.StepWire{{ID: "step1", Name: "Step One", Kind: "task"}}
	r := &WorkflowReconciler{}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{Role: "Engineer"}, steps, runs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"project root", "not inside the worktree", ".orchicon/"} {
		if !strings.Contains(out, want) {
			t.Errorf("composite prompt missing %q in hasPriorSteps block; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "from the working directory") {
		t.Errorf("composite prompt must not contain old phrase 'from the working directory'; got:\n%s", out)
	}
}
