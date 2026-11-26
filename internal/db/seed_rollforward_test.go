package db

// seed_rollforward_test.go — A PROMPT CHANGE THAT NEVER REACHES A WORKER IS NOT A CHANGE.

import (
	"os"
	"strings"
	"testing"
)

// quickCanned returns the seeded Quick Software Engineer definition.
func quickCanned(t *testing.T) cannedWorker {
	t.Helper()
	for _, w := range CannedWorkers() {
		if w.ID == "01M1ERH9921YS9S1QWGZV8D1VM" {
			return w
		}
	}
	t.Fatal("the Quick Software Engineer is not in the canned set")
	return cannedWorker{}
}

// storedWithEverythingButTheFingerprint renders what an EXISTING install holds: every legacy marker present,
// and the content-fingerprint comment absent (it did not exist before this change).
func storedWithEverythingButTheFingerprint(w cannedWorker) string {
	md := seedAgentsMD(w)
	// Strip the fingerprint comment and append each legacy marker so the old predicate would have been
	// satisfied — this is precisely the state that hid the dead fix.
	md = strings.ReplaceAll(md, cannedSeedFingerprintMarker(w), "")
	for _, m := range []string{seedMarkerComment, sandboxPlaneBlock} {
		if !strings.Contains(md, m) {
			md += m
		}
	}
	if w.RollMarker != "" && !strings.Contains(md, w.RollMarker) {
		md += w.RollMarker
	}
	return md
}

// TestAnOrdinaryPromptEditRollsForward is the regression assertion.
func TestAnOrdinaryPromptEditRollsForward(t *testing.T) {
	w := quickCanned(t)
	stored := storedWithEverythingButTheFingerprint(w)

	// Sanity: the old predicate would NOT have synced this — which is the bug.
	if !strings.Contains(stored, seedSafetyMarker) || !strings.Contains(stored, sandboxPlaneMarker) ||
		!strings.Contains(stored, w.RollMarker) {
		t.Fatal("the fixture does not carry every legacy marker, so it does not model the reported state")
	}

	if !cannedSeedNeedsSync(w, stored, false) {
		t.Fatal("a stored version that predates the seed content was left alone: the prompt change never " +
			"reaches a worker on any existing install, with no log line and no error — this is the reported " +
			"failure, and testing the prompt text cannot catch it")
	}
}

// TestTheSameContentDoesNotRollAgain — the sync must be idempotent, or every boot would mint a version.
func TestTheSameContentDoesNotRollAgain(t *testing.T) {
	w := quickCanned(t)
	if cannedSeedNeedsSync(w, seedAgentsMD(w), false) {
		t.Fatal("a version already carrying this seed's content was re-synced — every boot would mint a new " +
			"published version")
	}
}

// TestAChangedFieldChangesTheKey — the mechanism, not just the one worker.
func TestAChangedFieldChangesTheKey(t *testing.T) {
	w := quickCanned(t)
	stored := seedAgentsMD(w)

	for _, mutate := range []struct {
		name string
		fn   func(*cannedWorker)
	}{
		{"agents_md", func(x *cannedWorker) { x.AgentsMD += "\nan extra instruction\n" }},
		{"behavior", func(x *cannedWorker) { x.Behavior += " and also do this" }},
		{"skills", func(x *cannedWorker) { x.Skills += " • Terraform" }},
		{"role", func(x *cannedWorker) { x.Role += " Be terse." }},
	} {
		edited := w
		mutate.fn(&edited)
		if !cannedSeedNeedsSync(edited, stored, false) {
			t.Errorf("changing %s did not roll the worker forward — that field is synced by the seeder, so an "+
				"edit to it must reach a live worker", mutate.name)
		}
	}
}

// TestADraftIsNeverSyncedHere — the promote block owns publishing a draft current version.
func TestADraftIsNeverSyncedHere(t *testing.T) {
	w := quickCanned(t)
	if cannedSeedNeedsSync(w, "anything at all", true) {
		t.Error("a DRAFT current version must not be synced here — the promote path owns publishing it")
	}
}

// TestDroppedSafetyStillRollsForward — the legacy markers keep doing their job: a stored version whose safety
// or sandbox contract was edited away is re-synced even when the fingerprint still matches.
func TestDroppedSafetyStillRollsForward(t *testing.T) {
	w := quickCanned(t)
	stripped := strings.ReplaceAll(seedAgentsMD(w), seedSafetyMarker, "removed by an edit")
	if !cannedSeedNeedsSync(w, stripped, false) {
		t.Error("a version with the safety marker stripped was left alone — the fingerprint alone cannot detect " +
			"an edit elsewhere in the text")
	}
}

// TestTheQuickProfileDoesNotAssertThePRFlowUnconditionally — the profile is prompt text.
func TestTheQuickProfileDoesNotAssertThePRFlowUnconditionally(t *testing.T) {
	w := quickCanned(t)
	profile := w.Description + "\n" + w.Purpose + "\n" + w.Behavior
	for _, banned := range []string{
		"pushes to origin, and opens + merges the PR",              // Description
		"commit to the run branch, push to origin, then open a PR", // Behavior
	} {
		if strings.Contains(profile, banned) {
			t.Errorf("the Quick worker's profile still asserts the PR flow unconditionally (%q). The profile is "+
				"rendered into the prompt, so it contradicts the mode-aware shipping section on a project with "+
				"no remote.\nprofile:\n%s", banned, profile)
		}
	}
	// And it must say what to do instead, in BOTH modes — the same rule the delivery block states.
	for _, want := range []string{"git strategy", "git_strategy=local"} {
		if !strings.Contains(profile, want) {
			t.Errorf("the profile does not mention %q, so it names no alternative to pushing", want)
		}
	}
}

// ---------------------------------------------------------------------------
// THE MODEL-COST RULE (v25)
//
// The rule used to be "Free models ONLY … you cannot use it for testing", which made a whole class of
// acceptance criterion permanently unsatisfiable: an AC that names a live conversation on a paid model
// could not be met, so the worker correctly reported `failure` and the item reported failure on EVERY
// run no matter how good the code was. It is now a cheapest-model rule plus an explicit prohibition on
// spending tokens to TEST a model — the prohibition is the half that actually bounds cost.
//
// These pin the TEXT, because it is prompt-only: nothing in the platform enforces a model allowlist at
// dispatch (there is no free-model gate anywhere in internal/), so the prompt IS the whole mechanism.
// ---------------------------------------------------------------------------

// TestTheModelRuleNoLongerForbidsPaidModels — the hard prohibition is what made criteria unusatisfiable.
func TestTheModelRuleNoLongerForbidsPaidModels(t *testing.T) {
	for _, banned := range []string{
		"Free models ONLY",             // the absolute rule that was removed
		"cannot use it for testing",    // the clause that made a paid-model AC impossible
		"ABSOLUTELY never use a cloud", // its emphatic form
	} {
		if strings.Contains(safetyBlock, banned) {
			t.Errorf("safetyBlock still carries the old absolute prohibition (%q). It makes any acceptance "+
				"criterion naming a paid model permanently unsatisfiable, which is how an item reports "+
				"failure on every run regardless of the code.", banned)
		}
	}
}

// TestTheModelRuleStatesBothHalves — cheapest model AND no model testing. Dropping either half breaks it:
// without the first the worker reaches for a premium model, without the second it burns tokens proving a
// model works.
func TestTheModelRuleStatesBothHalves(t *testing.T) {
	// THE RULE WAS CORRECTED, NOT WEAKENED. It still has both halves, but the second half used to
	// read "NEVER spend tokens testing a model", which a QA worker correctly read as a prohibition
	// on the very live runs its acceptance criteria named — so it marked three ACs NOT VERIFIED and
	// was graded as failing. The correct shape distinguishes "don't burn tokens showing off" from
	// "do produce the evidence your criteria require":
	//
	//	cheapest capable model, ANY adapter (orchicon/opencode/claude/agy), small prompts, few calls
	//	+ a live run an AC names IS the evidence, and must be performed
	//	+ NOT VERIFIED only when genuinely out of reach, with the blocker named
	for _, want := range []string{
		"Prefer a free model",
		"LOWEST-COST", // the escalation rule
		"cheapest capable model",
		"WHEN AN ACCEPTANCE CRITERION NAMES A LIVE RUN",
		"NOT VERIFIED", // what to do with a criterion genuinely beyond reach
	} {
		if !strings.Contains(safetyBlock, want) {
			t.Errorf("safetyBlock does not state %q. The rule has two halves — cheapest model, and never "+
				"spend tokens testing one — and a missing half is a rule that permits what it exists to stop.",
				want)
		}
	}
}

// TestEveryWorkerCarriesTheModelRule — and the mechanism is NOT the AGENTS.md.
//
// seedAgentsMDBase REPLACES safetyBlock with the bare marker comment, so the rules are delivered once,
// through the stable prompt prefix (StablePromptPrefix in prompt.go) rather than duplicated into every
// worker's AGENTS.md. This pins BOTH halves of that: the rule rides safetyBlock (so every adapter's
// system prompt carries it), and the seeded AGENTS.md carries the MARKER (so roll-forward reaches an
// existing published version). Asserting the rule's text inside the AGENTS.md would be asserting a
// duplicate the design deliberately does not have — which is what this test originally got wrong.
func TestEveryWorkerCarriesTheModelRule(t *testing.T) {
	if !strings.Contains(safetyBlock, "DO NOT SPEND TOKENS PROVING A MODEL IS GOOD") {
		t.Error("safetyBlock does not carry the model-spend rule, so no adapter's system prompt does — " +
			"the rule reaches every execution ONLY through the stable prompt prefix")
	}
	for _, w := range CannedWorkers() {
		agents := seedAgentsMD(w)
		// The safety block must NOT be duplicated into the AGENTS.md (that is the de-duplication).
		if strings.Contains(agents, safetyBlock) {
			t.Errorf("%s (%s) duplicates the whole safety block into its AGENTS.md — the rules are delivered "+
				"via the prompt prefix, and a second copy is prompt weight that can drift", w.Name, w.ID)
		}
		// The marker must be there, or the seeder won't roll this worker forward to the new rules.
		if !strings.Contains(agents, seedSafetyMarker) {
			t.Errorf("%s (%s) does not carry the current safety marker %q — the seeder will not roll an "+
				"existing published version forward to this content", w.Name, w.ID, seedSafetyMarker)
		}
	}
}

// TestTheMarkerWasBumpedForThisChange is the anti-drift witness for the bump itself. Editing safetyBlock
// without bumping seedSafetyMarker is the failure mode: the content fingerprint may still roll it
// forward, but the marker is what a truncated or hand-edited stored copy is detected by, and it is the
// version an operator reads to know which rules a worker has.
func TestTheMarkerWasBumpedForThisChange(t *testing.T) {
	if seedSafetyMarker == "orchicon.safety=v24" {
		t.Fatal("seedSafetyMarker is still v24 after safetyBlock's text changed. Bump it — a rule edited " +
			"under an unchanged marker is a rule whose rollout cannot be read")
	}
	if !strings.Contains(seedAgentsMD(quickCanned(t)), seedSafetyMarker) {
		t.Fatalf("the seeded AGENTS.md does not contain the marker %q", seedSafetyMarker)
	}
}

// ---------------------------------------------------------------------------
// MODE-CORRECTNESS OF THE PROMPT TEXT (v26)
//
// A canned worker's AGENTS.md is seeded ONCE and can never vary by execution mode — the mode is only
// known per RUN, and only RuntimeEnvironmentBlock receives it. So any static block that asserts
// container facts is a block that LIES to every local-mode run.
//
// THAT IS NOT HYPOTHETICAL. The QA Engineer carried playwrightBlock, which instructed it to use the
// :orchicon-dev image, look in /ms-playwright and call require("playwright"). A local-mode QA run read
// a recipe for a container it did not have, tried Node require(), got a throw, and reported that
// visual verification was impossible — "no GUI to screenshot" — on a host where headless Chromium
// works. The item was recorded as a failure for a defect that existed only in its instructions.
//
// These pin both directions: the static text must be MODE-NEUTRAL (or explicitly scoped), and the
// per-run block must carry the host-true replacement.
// ---------------------------------------------------------------------------

// TestNoStaticBlockAssertsAContainerUnconditionally is the sweep. It runs over EVERY canned worker, so a
// new worker (or a new block) that hard-codes container-only facts is caught here rather than in a run.
func TestNoStaticBlockAssertsAContainerUnconditionally(t *testing.T) {
	// Phrases that were only true inside a container, and were stated as absolutes.
	banned := []string{
		"You ONLY work inside the runtime container",
		"You run inside an isolated workflow runtime container",
	}
	for _, w := range CannedWorkers() {
		agents := seedAgentsMD(w)
		for _, b := range banned {
			if strings.Contains(agents, b) {
				t.Errorf("%s (%s) asserts %q unconditionally. A local-mode run has no container, and this "+
					"text cannot vary by mode (it is seeded once) — say \"in a runtime-mode run\" instead.",
					w.Name, w.ID, b)
			}
		}
		// The old local X-server absolute is gone from the per-run block too.
	}
	if strings.Contains(localEnvironmentBlock(), "There is no X server") {
		t.Error("the local runtime block still claims there is no X server. A launch was observed on a " +
			"host with DISPLAY=:0 and a live X socket — say a display may or may not exist.")
	}
}

// TestTheQAWorkerScopesItsBrowserBlockToRuntimeMode — the playwrightBlock is container truth; it must say
// so, because the QA worker is the one that carries it and the one that was misled.
func TestTheQAWorkerScopesItsBrowserBlockToRuntimeMode(t *testing.T) {
	var qa cannedWorker
	var found bool
	for _, w := range CannedWorkers() {
		if w.ID == "w_se_qa_engineer" {
			qa, found = w, true
			break
		}
	}
	if !found {
		t.Fatal("the QA Engineer is not in the canned set")
	}
	agents := seedAgentsMD(qa)
	if !strings.Contains(agents, "runtime`-mode") && !strings.Contains(agents, "runtime mode only") {
		t.Error("the QA worker's browser block does not scope itself to runtime mode, so a local run reads " +
			"/ms-playwright and require(\"playwright\") as instructions that apply to it")
	}
}

// TestTheLocalBlockCarriesTheHostBrowserPath — the replacement must be REAL and match what was measured
// on the host: Python Playwright present with browsers downloaded, Node absent.
func TestTheLocalBlockCarriesTheHostBrowserPath(t *testing.T) {
	block := localEnvironmentBlock()
	for _, want := range []string{
		"PYTHON Playwright",      // the route that works
		"playwright.sync_api",    // the concrete import
		"--no-sandbox",           // the flag that was verified
		"~/.cache/ms-playwright", // where host browsers actually are
		"require(",               // names the dead end explicitly
		"8091",                   // PROD, named so a worker can avoid it
		"5432",                   // PROD's database, likewise
		"8080",                   // DEV, reachable and writable from a local run
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the local runtime block does not mention %q. A local run needs the route that "+
				"actually works on the host, or it will report visual verification as impossible again.",
				want)
		}
	}
	// And it must not SEND a local run to container paths. NAMING them to deny them is the point (a
	// local worker may also have read the static block), so this checks the instruction forms rather
	// than mere mention.
	for _, banned := range []string{
		"Use the `:orchicon-dev`",
		"PLAYWRIGHT_BROWSERS_PATH=/ms-playwright",
		"require(\"playwright\") from any directory",
	} {
		if strings.Contains(block, banned) {
			t.Errorf("the LOCAL runtime block instructs an in-process run with %q, which does not exist "+
				"without a container", banned)
		}
	}
}

// TestRuntimeModeStillGetsTheContainerRecipe — the fix must not strip the container instructions that
// runtime-mode workers genuinely need.
func TestRuntimeModeStillGetsTheContainerRecipe(t *testing.T) {
	block := containerEnvironmentBlock("orchicon-dev:latest")
	for _, want := range []string{"ephemeral, rootless Linux container", "not root", "/tmp/orchicon"} {
		if !strings.Contains(block, want) {
			t.Errorf("the container runtime block lost %q — the local fix must not degrade runtime mode", want)
		}
	}
	if strings.Contains(block, "PYTHON Playwright") {
		t.Error("the CONTAINER block now carries the local host recipe — the two must stay distinct")
	}
}

// TestWorkerMDStatesTheBaselineIsRuntimeModeTruth — worker.md is injected into EVERY adapter's worker, so
// a wrong baseline there misleads the whole population, not just one role.
func TestWorkerMDStatesTheBaselineIsRuntimeModeTruth(t *testing.T) {
	body, err := os.ReadFile("../../worker.md")
	if err != nil {
		t.Fatalf("read worker.md: %v", err)
	}
	md := string(body)
	if !strings.Contains(md, "runtime`-mode facts, not host facts") {
		t.Error("worker.md's environment baseline does not say it is runtime-mode truth. It is injected " +
			"into every adapter's worker, so an unscoped container fact is wrong for every local run.")
	}
	if !strings.Contains(md, "PYTHON Playwright") {
		t.Error("worker.md never mentions the host (Python) Playwright route, so a local-mode worker that " +
			"reads only the baseline concludes there is no browser at all")
	}
}
