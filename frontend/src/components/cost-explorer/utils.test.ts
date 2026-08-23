import type { Timestamp } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import type { CostSummary, UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type {
  WorkflowCostAggregate,
  WorkflowRunCost,
  WorkflowWorkerCost,
} from "@/api/gen/orchicon/api/v1/ai_gateway_service_pb";
import {
  filterSummaries,
  filterUsageRecords,
  filterWorkflowAggregates,
  matchesSearch,
  sortSummaries,
  sortUsageRecords,
  sortWorkflowAggregates,
  summaryFinishedMs,
  toggleSort,
  usageRecordWhenMs,
  type SortState,
  type UsageSortKey,
} from "@/components/cost-explorer/utils";

// --- Builders ----------------------------------------------------------------

function summary(partial: Partial<CostSummary>): CostSummary {
  return {
    groupBy: "project",
    groupKey: "k",
    displayName: "",
    costUsd: 0,
    totalTokens: 0n,
    ...partial,
  } as unknown as CostSummary;
}

function ts(seconds: number): Timestamp {
  return { seconds: BigInt(seconds), nanos: 0 } as unknown as Timestamp;
}

function wf(partial: Partial<WorkflowCostAggregate>): WorkflowCostAggregate {
  return {
    workflowId: "wf",
    workflowName: "",
    totalCostUsd: 0,
    totalTokens: 0n,
    runCount: 0,
    executionCount: 0,
    runs: [],
    ...partial,
  } as unknown as WorkflowCostAggregate;
}

function worker(partial: Partial<WorkflowWorkerCost>): WorkflowWorkerCost {
  return {
    workerId: "w",
    workerName: "",
    totalCostUsd: 0,
    totalTokens: 0n,
    promptTokens: 0n,
    completionTokens: 0n,
    executionCount: 0,
    ...partial,
  } as unknown as WorkflowWorkerCost;
}

function run(partial: Partial<WorkflowRunCost>): WorkflowRunCost {
  return {
    workflowRunId: "run",
    totalCostUsd: 0,
    totalTokens: 0n,
    executionCount: 0,
    runStatus: "",
    workers: [],
    workItemName: "",
    ...partial,
  } as unknown as WorkflowRunCost;
}

function record(partial: Partial<UsageRecord>): UsageRecord {
  return {
    id: "id",
    workerName: "",
    taskTitle: "",
    provider: "",
    model: "",
    totalTokens: 0n,
    costUsd: 0,
    ...partial,
  } as unknown as UsageRecord;
}

const name = (s: CostSummary): string => s.displayName || s.groupKey;

// --- matchesSearch -------------------------------------------------------------

describe("matchesSearch", () => {
  it("matches case-insensitive substrings", () => {
    expect(matchesSearch(["Alpha Beta"], "beta")).toBe(true);
    expect(matchesSearch(["Alpha Beta"], "ALPHA")).toBe(true);
    expect(matchesSearch(["Alpha Beta"], "missing")).toBe(false);
  });

  it("matches if ANY field matches", () => {
    expect(matchesSearch(["worker", "task-title", "api-1"], "task")).toBe(true);
  });

  it("treats null/undefined fields as empty strings", () => {
    expect(matchesSearch([undefined, null, "real"], "real")).toBe(true);
    expect(matchesSearch([undefined, null], "real")).toBe(false);
  });

  it("an empty or whitespace needle matches everything", () => {
    expect(matchesSearch(["anything"], "")).toBe(true);
    expect(matchesSearch(["anything"], "   ")).toBe(true);
  });
});

describe("toggleSort", () => {
  it("flips direction when the same key is clicked", () => {
    expect(toggleSort({ key: "cost", dir: "desc" }, "cost")).toEqual({
      key: "cost",
      dir: "asc",
    });
  });

  it("starts ascending when a new key is clicked", () => {
    expect(toggleSort({ key: "cost", dir: "desc" }, "name")).toEqual({
      key: "name",
      dir: "asc",
    });
  });
});

// --- filterSummaries ---------------------------------------------------------

describe("filterSummaries", () => {
  const rows = [
    summary({ groupKey: "p1", displayName: "Payments" }),
    summary({ groupKey: "p2", displayName: "Search" }),
    summary({ groupKey: "p3", displayName: "Billing" }),
  ];

  it("filters by display name case-insensitively", () => {
    expect(filterSummaries(rows, "pay", name).map((r) => r.groupKey)).toEqual(["p1"]);
  });

  it("filters by group key when there is no display name", () => {
    expect(filterSummaries(rows, "p2", name).map((r) => r.groupKey)).toEqual(["p2"]);
  });

  it("keeps all rows for an empty needle and returns the same array reference", () => {
    expect(filterSummaries(rows, "", name)).toBe(rows);
  });

  it("preserves row identity for drill-down (groupKey unchanged)", () => {
    const filtered = filterSummaries(rows, "search", name);
    expect(filtered[0]).toBe(rows[1]);
  });
});

// --- sortSummaries -----------------------------------------------------------

describe("sortSummaries", () => {
  const rows = [
    summary({ groupKey: "b", displayName: "Beta", costUsd: 5, totalTokens: 20n }),
    summary({ groupKey: "a", displayName: "Alpha", costUsd: 3, totalTokens: 40n }),
    summary({ groupKey: "c", displayName: "Gamma", costUsd: 8, totalTokens: 10n }),
  ];

  it("sorts by cost asc/desc", () => {
    expect(sortSummaries(rows, { key: "cost", dir: "asc" }, name).map((r) => r.groupKey)).toEqual(["a", "b", "c"]);
    expect(sortSummaries(rows, { key: "cost", dir: "desc" }, name).map((r) => r.groupKey)).toEqual(["c", "b", "a"]);
  });

  it("sorts by bigint tokens (normalises int64)", () => {
    expect(sortSummaries(rows, { key: "tokens", dir: "asc" }, name).map((r) => r.groupKey)).toEqual(["c", "b", "a"]);
    expect(sortSummaries(rows, { key: "tokens", dir: "desc" }, name).map((r) => r.groupKey)).toEqual(["a", "b", "c"]);
  });

  it("sorts by name alphabetically via localeCompare", () => {
    expect(sortSummaries(rows, { key: "name", dir: "asc" }, name).map((r) => r.groupKey)).toEqual(["a", "b", "c"]);
  });

  it("does not mutate the input array", () => {
    const copy = [...rows];
    sortSummaries(rows, { key: "cost", dir: "asc" }, name);
    expect(rows).toEqual(copy);
  });

  it("breaks ties deterministically by name", () => {
    const ties = [
      summary({ groupKey: "x", displayName: "Same", costUsd: 1 }),
      summary({ groupKey: "y", displayName: "Same", costUsd: 1 }),
      summary({ groupKey: "z", displayName: "Same", costUsd: 1 }),
    ];
    expect(sortSummaries(ties, { key: "cost", dir: "asc" }, name).map((r) => r.groupKey)).toEqual(["x", "y", "z"]);
  });
});

describe("sortSummaries by finished (missing last, both directions)", () => {
  const rows = [
    summary({ groupKey: "none", displayName: "None", finishedAt: undefined }),
    summary({ groupKey: "old", displayName: "Old", finishedAt: ts(1000) }),
    summary({ groupKey: "new", displayName: "New", finishedAt: ts(99999) }),
  ];

  it("ascending orders finished last", () => {
    expect(sortSummaries(rows, { key: "finished", dir: "asc" }, name).map((r) => r.groupKey)).toEqual(["old", "new", "none"]);
  });

  it("descending also orders finished last", () => {
    expect(sortSummaries(rows, { key: "finished", dir: "desc" }, name).map((r) => r.groupKey)).toEqual(["new", "old", "none"]);
  });

  it("summaryFinishedMs returns null for absent finished_at", () => {
    expect(summaryFinishedMs(rows[0])).toBeNull();
    expect(summaryFinishedMs(rows[2])).toBe(99999000);
  });
});

// --- Workflow panel ----------------------------------------------------------

describe("filterWorkflowAggregates", () => {
  it("keeps a whole workflow when its name matches", () => {
    const workflows = [
      wf({ workflowName: "Ingest", runs: [run({ workItemName: "X" })] }),
      wf({ workflowName: "Export", runs: [run({ workItemName: "Y" })] }),
    ];
    const out = filterWorkflowAggregates(workflows, "ingest");
    expect(out).toHaveLength(1);
    expect(out[0].workflowName).toBe("Ingest");
  });

  it("prunes to matching runs and their matching workers", () => {
    const workflows = [
      wf({
        workflowName: "Export",
        runs: [
          run({ workflowRunId: "r1", workItemName: "match", workers: [worker({ workerId: "w1", workerName: "Matcher" }), worker({ workerId: "w2", workerName: "Other" })] }),
          run({ workflowRunId: "r2", workItemName: "skip", workers: [worker({ workerId: "w1", workerName: "Matcher" })] }),
        ],
      }),
    ] as WorkflowCostAggregate[];
    const out = filterWorkflowAggregates(workflows, "match");
    expect(out).toHaveLength(1);
    expect(out[0].runs).toHaveLength(2);
    // run r1 matched by name → keeps all its workers; run r2 matched only by worker → prunes
    expect(out[0].runs[0].workers).toHaveLength(2);
    expect(out[0].runs[1].workers).toHaveLength(1);
    expect(out[0].runs[1].workers[0].workerName).toBe("Matcher");
  });

  it("drops workflows with no matching run", () => {
    const workflows = [wf({ workflowName: "Alpha", runs: [run({ workItemName: "x" })] })];
    expect(filterWorkflowAggregates(workflows, "nomatch")).toHaveLength(0);
  });

  it("returns the original array for an empty needle", () => {
    const workflows = [wf({ workflowName: "Alpha" })];
    expect(filterWorkflowAggregates(workflows, "")).toBe(workflows);
  });
});

describe("sortWorkflowAggregates", () => {
  it("sorts workflows and their nested runs by cost", () => {
    const workflows = [
      wf({
        workflowId: "wf-expensive",
        workflowName: "B",
        totalCostUsd: 9,
        runs: [
          run({ workflowRunId: "r2", workItemName: "z", totalCostUsd: 20 }),
          run({ workflowRunId: "r1", workItemName: "a", totalCostUsd: 5 }),
        ],
      }),
      wf({ workflowId: "wf-cheap", workflowName: "A", totalCostUsd: 2, runs: [] }),
    ];
    const out = sortWorkflowAggregates(workflows, { key: "cost", dir: "desc" });
    expect(out.map((w) => w.workflowId)).toEqual(["wf-expensive", "wf-cheap"]);
    expect(out[0].runs.map((r) => r.workflowRunId)).toEqual(["r2", "r1"]);
  });

  it("sorts by name", () => {
    const workflows = [
      wf({ workflowName: "Zebra" }),
      wf({ workflowName: "Apple" }),
    ];
    expect(sortWorkflowAggregates(workflows, { key: "name", dir: "asc" }).map((w) => w.workflowName)).toEqual(["Apple", "Zebra"]);
  });
});

// --- Usage records -----------------------------------------------------------

describe("filterUsageRecords", () => {
  it("filters by worker/task/provider/model case-insensitively", () => {
    const recs = [
      record({ id: "1", workerName: "Ada", provider: "anthropic", model: "claude-sonnet-4" }),
      record({ id: "2", workerName: "Grace", provider: "openai", model: "gpt-4o" }),
    ];
    expect(filterUsageRecords(recs, "ada").map((r) => r.id)).toEqual(["1"]);
    expect(filterUsageRecords(recs, "claude").map((r) => r.id)).toEqual(["1"]);
    expect(filterUsageRecords(recs, "openai").map((r) => r.id)).toEqual(["2"]);
    expect(filterUsageRecords(recs, "").length).toBe(2);
  });
});

describe("sortUsageRecords", () => {
  const recs = [
    record({ id: "1", workerName: "Ada", taskTitle: "Auth", costUsd: 2, totalTokens: 30n, occurredAt: ts(1000) }),
    record({ id: "2", workerName: "Bob", taskTitle: "Billing", costUsd: 5, totalTokens: 10n, occurredAt: ts(5000) }),
    record({ id: "3", workerName: "Carl", taskTitle: "Billing", costUsd: 1, totalTokens: 20n, occurredAt: ts(9000) }),
  ] as UsageRecord[];

  it("sorts by cost asc/desc", () => {
    expect(sortUsageRecords(recs, { key: "cost", dir: "asc" }).map((r) => r.id)).toEqual(["3", "1", "2"]);
    expect(sortUsageRecords(recs, { key: "cost", dir: "desc" }).map((r) => r.id)).toEqual(["2", "1", "3"]);
  });

  it("sorts by tokens (bigint)", () => {
    expect(sortUsageRecords(recs, { key: "tokens", dir: "asc" }).map((r) => r.id)).toEqual(["2", "3", "1"]);
  });

  it("sorts by worker name", () => {
    expect(sortUsageRecords(recs, { key: "worker", dir: "asc" }).map((r) => r.id)).toEqual(["1", "2", "3"]);
  });

  it("sorts by 'when' (occurred_at) and keeps records without a time last", () => {
    const recs = [
      record({ id: "no-time", workerName: "Zed", occurredAt: undefined }),
      record({ id: "old", workerName: "A", occurredAt: ts(200) }),
      record({ id: "new", workerName: "B", occurredAt: ts(800) }),
    ] as UsageRecord[];
    expect(sortUsageRecords(recs, { key: "when", dir: "asc" }).map((r) => r.id)).toEqual(["old", "new", "no-time"]);
    expect(sortUsageRecords(recs, { key: "when", dir: "desc" }).map((r) => r.id)).toEqual(["new", "old", "no-time"]);
  });

  it("usageRecordWhenMs returns null for absent occurred_at", () => {
    expect(usageRecordWhenMs({ id: "x", occurredAt: undefined } as UsageRecord)).toBeNull();
  });
});

// --- Type-safety sanity --------------------------------------------------------

describe("usage table sort keys", () => {
  it("accepts every UsageSortKey", () => {
    const keys: UsageSortKey[] = ["worker", "task", "provider", "model", "tokens", "cost", "when"];
    const s: SortState<UsageSortKey> = { key: keys[0], dir: "asc" };
    expect(s.key).toBe("worker");
  });
});
