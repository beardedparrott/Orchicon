// RunFailureCard.notice.test.tsx — a NOTICE must not render as a FAILURE.
//
// REPORTED: "I never initiated a git repo on the directory so it claimed the run had failed because of that but
// then the worker kept working." The run was SKIPPED — the supported "works in place, no branch" path — but the
// skip's reason was written to workflow_runs.error_message, and this card renders anything in that column as a
// red failure. The operator aborted a run that was fine.
//
// So the card keys on the KIND, not the presence of text: `worktree_notice` is the platform saying "here is what
// happened and what I did about it; nothing failed".
import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { RunFailureCard } from "./RunFailureCard";

describe("RunFailureCard", () => {
  it("renders a worktree notice as a neutral note, not a failure", () => {
    const html = renderToStaticMarkup(
      createElement(RunFailureCard, {
        errorMessage:
          "project dir /tmp/x is not a git work tree\n\nThis project is ephemeral (git_strategy=none), so the run works in place in the project directory with no branch — exactly as configured.",
        errorKind: "worktree_notice",
      }),
    );
    expect(html).toContain("nothing failed");
    // The wording must not claim a failure, and it must not wear the failure card's identity.
    expect(html.toLowerCase()).not.toContain("run failed");
    // Both paragraphs render: what happened, and what it means.
    expect(html).toContain("not a git work tree");
    expect(html).toContain("works in place");
  });

  it("keeps a genuine preflight failure on the failure card", () => {
    const html = renderToStaticMarkup(
      createElement(RunFailureCard, {
        errorMessage: "this repository has no valid HEAD to branch from",
        errorKind: "worktree_no_base_ref",
      }),
    );
    expect(html).toContain("no valid HEAD");
    expect(html).not.toContain("nothing failed");
  });

  it("renders nothing when no message was recorded", () => {
    const html = renderToStaticMarkup(createElement(RunFailureCard, {}));
    expect(html).toBe("");
  });
});
