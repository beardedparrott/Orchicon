import { describe, expect, it } from "vitest";
import {
  buildHeadsUpTiles,
  computeQueueOrder,
  extractLastTextBlock,
  loopOutcomeTag,
  parseSteps,
  parseViewParam,
  queuePosition,
  resultSummaryLine,
  tileStatusStyle,
  type HudStepInput,
} from "./headsUp";
import type { WorkerExecution } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { WorkflowStepRun } from "@/api/gen/orchicon/api/v1/workflow_pb";

function srun(partial: Partial<WorkflowStepRun>): WorkflowStepRun {
  return {
    id: "sr-x",
    stepId: "s",
    status: 1,
    stepKind: 1,
    iteration: 0,
    supersededBy: "",
    workerExecutionId: "",
    result: "",
    stepName: "",
    ...partial,
  } as WorkflowStepRun;
}

function exec(partial: Partial<WorkerExecution>): WorkerExecution {
  return { id: "e1", workerName: "w", workerVersion: 1, ...partial } as WorkerExecution;
}

function textPart(text: string) {
  return {
    kind: "text",
    payload: new TextEncoder().encode(JSON.stringify({ part: { text } })),
  };
}

describe("parseViewParam", () => {
  it("keeps graph", () => expect(parseViewParam("graph")).toBe("graph"));
  it("defaults missing/garbage to tile", () => {
    expect(parseViewParam(undefined)).toBe("tile");
    expect(parseViewParam(null)).toBe("tile");
    expect(parseViewParam("")).toBe("tile");
    expect(parseViewParam("tile")).toBe("tile");
    expect(parseViewParam("GRAPH")).toBe("tile");
    expect(parseViewParam(42)).toBe("tile");
  });
});

describe("tileStatusStyle", () => {
  it("maps running/succeeded/failed", () => {
    expect(tileStatusStyle(3)).toEqual({
      label: "running",
      className: expect.stringContaining("bg-blue-100"),
    });
    expect(tileStatusStyle(4).label).toBe("succeeded");
    expect(tileStatusStyle(5).label).toBe("failed");
    expect(tileStatusStyle(8).label).toBe("approval_pending");
  });
  it("degrades unknown statuses to pending", () => {
    expect(tileStatusStyle(99).label).toBe("pending");
    expect(tileStatusStyle(0).label).toBe("pending");
  });
});

describe("queuePosition", () => {
  const steps: HudStepInput[] = [
    { id: "a" },
    { id: "b", depends_on: ["a"] },
    { id: "c", depends_on: ["b"] },
  ];
  it("orders a linear chain 1-2-3", () => {
    expect(queuePosition(steps, "a")).toBe(1);
    expect(queuePosition(steps, "b")).toBe(2);
    expect(queuePosition(steps, "c")).toBe(3);
  });
  it("puts deeper branches later (diamond)", () => {
    const diamond: HudStepInput[] = [
      { id: "root" },
      { id: "left", depends_on: ["root"] },
      { id: "right", depends_on: ["root"] },
      { id: "join", depends_on: ["left", "right"] },
    ];
    expect(computeQueueOrder(diamond)[0]).toBe("root");
    expect(queuePosition(diamond, "join")).toBe(4);
  });
  it("returns 0 for unknown steps and ignores missing deps", () => {
    expect(queuePosition(steps, "nope")).toBe(0);
    expect(queuePosition([{ id: "x", depends_on: ["ghost"] }], "x")).toBe(1);
  });
  it("survives dependency cycles", () => {
    const cyclic: HudStepInput[] = [
      { id: "a", depends_on: ["b"] },
      { id: "b", depends_on: ["a"] },
    ];
    expect(computeQueueOrder(cyclic)).toHaveLength(2);
  });
});

describe("loopOutcomeTag", () => {
  it("mirrors the graph loopTag logic", () => {
    expect(loopOutcomeTag(srun({ stepKind: 8, result: JSON.stringify({ loop: "re-ask" }) }))).toBe(
      "re-ask reviewer",
    );
    expect(loopOutcomeTag(srun({ stepKind: 8, result: JSON.stringify({ loop: "reviewer" }) }))).toBe(
      "loop → reviewer",
    );
    expect(
      loopOutcomeTag(srun({ stepKind: 8, result: JSON.stringify({ decision: "ship" }) })),
    ).toBe("decision: ship");
    expect(loopOutcomeTag(srun({ stepKind: 8, status: 4, result: "" }))).toBe("decision made");
    expect(loopOutcomeTag(srun({ stepKind: 8, status: 5, result: "{}" }))).toBe(
      "max iterations reached",
    );
  });
  it("returns undefined off loop_decision / bad JSON / null", () => {
    expect(loopOutcomeTag(srun({ stepKind: 1, status: 4, result: "" }))).toBeUndefined();
    expect(loopOutcomeTag(srun({ stepKind: 8, result: "not-json{{{" }))).toBeUndefined();
    expect(loopOutcomeTag(null)).toBeUndefined();
  });
});

describe("extractLastTextBlock", () => {
  it("returns the newest durable text block, skipping tool/result parts", () => {
    const parts = [
      textPart("first"),
      { kind: "tool_use", payload: new TextEncoder().encode("{}") },
      textPart("second"),
    ];
    expect(extractLastTextBlock(parts)).toBe("second");
  });
  it("returns empty when there is no text", () => {
    expect(extractLastTextBlock([])).toBe("");
    expect(extractLastTextBlock(undefined)).toBe("");
    expect(
      extractLastTextBlock([{ kind: "step_start", payload: new Uint8Array() }]),
    ).toBe("");
  });
  it("clamps long blocks", () => {
    expect(extractLastTextBlock([textPart("x".repeat(5000))], 100)).toHaveLength(101);
  });
});

describe("resultSummaryLine", () => {
  it("takes the first _summary line", () => {
    expect(
      resultSummaryLine(srun({ result: JSON.stringify({ _summary: "did X\nmore" }) })),
    ).toBe("did X");
  });
  it("returns empty on missing/bad result", () => {
    expect(resultSummaryLine(srun({ result: "" }))).toBe("");
    expect(resultSummaryLine(null)).toBe("");
  });
});

describe("parseSteps", () => {
  it("returns [] on bad input", () => {
    expect(parseSteps("")).toEqual([]);
    expect(parseSteps(undefined)).toEqual([]);
    expect(parseSteps("{{{")).toEqual([]);
    expect(parseSteps("{}")).toEqual([]);
  });
});

describe("buildHeadsUpTiles", () => {
  const stepsJson = JSON.stringify([
    { id: "a", name: "Build", kind: "task", depends_on: [] },
    { id: "b", name: "Review", kind: "approval", depends_on: ["a"] },
  ]);
  it("joins step-run → execution and marks upcoming", () => {
    const tiles = buildHeadsUpTiles(
      stepsJson,
      [srun({ id: "sr-a", stepId: "a", status: 4, workerExecutionId: "e1" })],
      [exec({ id: "e1" })],
    );
    expect(tiles).toHaveLength(2);
    expect(tiles[0].queueIndex).toBe(1);
    expect(tiles[0].execution?.id).toBe("e1");
    expect(tiles[0].isUpcoming).toBe(false);
    expect(tiles[1].isUpcoming).toBe(true);
    expect(tiles[1].queueIndex).toBe(2);
    expect(tiles[1].stepKindLabel).toBe("approval");
  });
  it("skips superseded rows and keeps the newest iteration", () => {
    const tiles = buildHeadsUpTiles(
      stepsJson,
      [
        srun({ id: "old", stepId: "a", status: 4, iteration: 0, workerExecutionId: "e-old" }),
        srun({ id: "new", stepId: "a", status: 3, iteration: 1, workerExecutionId: "e-new" }),
        srun({ id: "sup", stepId: "a", status: 5, iteration: 2, supersededBy: "new" }),
      ],
      [exec({ id: "e-new" })],
    );
    expect(tiles[0].stepRun?.id).toBe("new");
    expect(tiles[0].isActive).toBe(true);
  });
});
