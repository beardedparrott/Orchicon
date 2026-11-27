/**
 * RENDER-LEVEL TESTS FOR THE START/RESTART LABEL.
 *
 * WHY THIS FILE EXISTS, TWICE OVER.
 *
 * Round 1: the call site had an inverted negation, so every pending item said "Restart". The tests
 * asserted `restartLabel(false)` / `restartLabel(true)` DIRECTLY and never asked what the component
 * computed — green suite, wrong screen.
 *
 * Round 2: I fixed that by extracting `controlLabelFor` and asserting it. But the component computed
 * the label, passed it to `aria-label`, and then rendered `meta.label` — the static ACTION_META word
 * — as the visible text. So the fix was correct in the accessibility tree and invisible on screen,
 * and the helper-level tests could not tell.
 *
 * The lesson both times: a helper returning the right value proves nothing about the pixels. These
 * tests render the component and read the TEXT NODE, which is the only assertion that would have
 * caught either bug.
 *
 * `renderToStaticMarkup` is enough (the repo ships no jsdom by design) — but SequenceControls calls
 * hooks (useControlSequence, useToast), so it is rendered inside a QueryClientProvider and the hooks
 * degrade to no-ops without a live API. The label is computed from props alone, which is the point.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { WorkItemStatus, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { SequenceControls } from "./sequence-controls";

function item(status: WorkItemStatus, workflowRunId = ""): WorkItem {
  return {
    id: "01M8TEST",
    title: "a work item",
    status,
    workflowRunId,
    projectId: "p1",
    kind: 3,
  } as unknown as WorkItem;
}

function render(status: WorkItemStatus, workflowRunId = ""): string {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <SequenceControls item={item(status, workflowRunId)} hasChildren={false} />
    </QueryClientProvider>,
  );
}

/** Strip tags so assertions read the TEXT the operator sees, not markup or attributes. */
const textOf = (html: string) => html.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim();

describe("SequenceControls visible text", () => {
  it("renders the word Start for a pending item that has never run", () => {
    const text = textOf(render(WorkItemStatus.PENDING));
    expect(text).toContain("Start");
    expect(text).not.toContain("Restart");
  });

  it("renders the word Start even when a run id is bound, because status decides", () => {
    const text = textOf(render(WorkItemStatus.PENDING, "01M8CSC92YWEZSC2FTRWC2K5A7"));
    expect(text).toContain("Start");
    expect(text).not.toContain("Restart");
  });

  it("renders Restart for a failed item", () => {
    const text = textOf(render(WorkItemStatus.FAILED));
    expect(text).toContain("Restart");
    expect(text).toContain("Resume");
  });

  it("renders Restart for a cancelled item", () => {
    expect(textOf(render(WorkItemStatus.CANCELLED))).toContain("Restart");
  });

  it("renders Restart for a succeeded item", () => {
    expect(textOf(render(WorkItemStatus.SUCCEEDED))).toContain("Restart");
  });
});
