import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// RunBlockageCard — the state contract, pinned.
//
// There is no DOM test harness in this repo (no @testing-library/react), so
// these assert STRUCTURE, in the same idiom as RuntimeImageFailureAlert.test.tsx.
// They are aimed at the regressions that matter — the ways this card could
// quietly stop doing its job — rather than at styling.
//
// The Go classifier behind it (buildRunDiagnosis) carries the real behavioural
// coverage in internal/workflow/diagnosis_test.go; what is worth pinning HERE
// is how the card is allowed to present that answer.
const cardSrc = fs.readFileSync(path.join(__dirname, "RunBlockageCard.tsx"), "utf8");
const routeSrc = fs.readFileSync(
  path.join(__dirname, "..", "..", "routes", "workflows_.$id_.runs.$runId.tsx"),
  "utf8",
);

describe("RunBlockageCard", () => {
  it("renders NOTHING for a run that is not blocked", () => {
    // A diagnostic panel that shows up on healthy runs is noise, and noise is
    // what teaches people to stop reading the panel that matters.
    expect(cardSrc).toContain("if (!diagnosis || !diagnosis.blocked) return null;");
  });

  it("reports a FAILED diagnosis read instead of vanishing", () => {
    // Silence is the defect this feature exists to remove. A panel that
    // disappears on error teaches the operator to trust its absence.
    expect(cardSrc).toContain("isError");
    expect(cardSrc).toContain("Could not read this run");
  });

  it("offers the release action ONLY when there is something to release", () => {
    // On a full cap with no dead executions the slots are held by LIVE work and
    // the honest answer is "wait"; an always-present button would be a trap.
    expect(cardSrc).toContain("{diagnosis.clearable && (");
  });

  it("says out loud that live work is never touched", () => {
    // The button must not read as "cancel everything", because it does not.
    expect(cardSrc).toContain("never touched");
  });

  it("treats an empty release as success, not failure", () => {
    // Zero released means every slot holder is genuinely alive. Reporting that
    // as an error would send the operator hunting for a problem that is not
    // there.
    expect(cardSrc).toContain("none were dead, so nothing was released");
    expect(cardSrc).toContain("releasedIds.length > 0");
  });

  it("surfaces the blocker as an accessible live region", () => {
    expect(cardSrc).toContain('role="status"');
    expect(cardSrc).toContain('aria-live="polite"');
    // The action needs an accessible name of its own: "Release" alone is
    // meaningless out of context.
    expect(cardSrc).toContain("aria-label={`Release ${stuck.length} stuck execution");
  });

  it("shows the capacity as the gate's own numbers, and only when a cap exists", () => {
    // "0/0" would read as a limit of zero rather than no limit at all.
    expect(cardSrc).toContain("{limit > 0 && (");
    expect(cardSrc).toContain("diagnosis.activeExecutions");
    expect(cardSrc).toContain("diagnosis.dispatchLimit");
  });

  it("names the cause with the machine-readable kind, not just prose", () => {
    expect(cardSrc).toContain("diagnosis.blockageKind");
    expect(cardSrc).toContain("diagnosis.detail");
  });

  it("lists the stuck executions with their age, because age is the point", () => {
    // A three-week-old unhealthy execution and a two-minute-old one are
    // different situations.
    expect(cardSrc).toContain("stuckExecutions");
    expect(cardSrc).toContain("ageSeconds");
  });
});

describe("the run view renders the blockage card in BOTH views", () => {
  it("places the card ABOVE the view switch", () => {
    // THE INVARIANT: a stuck run is not a per-view concern. The cause is
    // usually run-EXTERNAL (the project's dispatch cap), so an operator who
    // switched to the graph tab must not lose the one panel that explains it.
    // Rendering it inside either branch would silently break that.
    const cardIdx = routeSrc.indexOf("<RunBlockageCard");
    const switchIdx = routeSrc.indexOf('{view === "tile" ? (');
    expect(cardIdx).toBeGreaterThan(-1);
    expect(switchIdx).toBeGreaterThan(-1);
    expect(cardIdx).toBeLessThan(switchIdx);
  });

  it("does not render it inside a single view branch", () => {
    const switchIdx = routeSrc.indexOf('{view === "tile" ? (');
    expect(routeSrc.indexOf("<RunBlockageCard", switchIdx)).toBe(-1);
  });

  it("polls only while the run is live, since a terminal run cannot block", () => {
    expect(routeSrc).toContain("<RunBlockageCard runId={runId} enabled={run.status === WorkflowRunStatus.RUNNING} />");
  });
});
