// Pure filter + sort helpers for the Cost Explorer (docs/10 §11). Kept free
// of JSX so they are unit-testable in isolation. All filtering/sorting runs
// over the already-fetched page — there is no server round-trip.

import type { Timestamp } from "@bufbuild/protobuf";
import type {
  CostSummary,
  UsageRecord,
} from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type {
  WorkflowCostAggregate,
  WorkflowRunCost,
} from "@/api/gen/orchicon/api/v1/ai_gateway_service_pb";

// --- Sort state -------------------------------------------------------------

export type SortKey = "cost" | "tokens" | "name" | "finished";
export type SortDir = "asc" | "desc";

export interface SortState<K extends string = SortKey> {
  key: K;
  dir: SortDir;
}

// UsageTableSortKey is the set of sortable columns on the Recent usage
// records table (Worker, Task, Provider, Model, Tokens, Cost, When).
export type UsageSortKey =
  | "worker"
  | "task"
  | "provider"
  | "model"
  | "tokens"
  | "cost"
  | "when";

// toggleSort returns a new SortState: clicking the active key flips its
// direction, clicking a different key starts a fresh ascending sort.
export function toggleSort<K extends string>(
  prev: SortState<K>,
  key: K,
): SortState<K> {
  if (prev.key === key) {
    return { key, dir: prev.dir === "asc" ? "desc" : "asc" };
  }
  return { key, dir: "asc" };
}

// --- Numeric / string compare ------------------------------------------------

// toNum normalises a proto int64 (bigint) or number to a JS number so token
// and cost comparisons work uniformly.
export function toNum(n: number | bigint | undefined | null): number {
  if (n === undefined || n === null) return 0;
  return typeof n === "bigint" ? Number(n) : n;
}

function compareNumbers(a: number, b: number, dir: SortDir): number {
  return dir === "asc" ? a - b : b - a;
}

function compareStrings(a: string, b: string, dir: SortDir): number {
  const cmp = a.localeCompare(b);
  return dir === "asc" ? cmp : -cmp;
}

// compareFinished orders by epoch millis. A missing finish time (null) always
// sorts LAST in both directions so unknown rows never jump the list.
function compareFinished(a: number | null, b: number | null, dir: SortDir): number {
  if (a === null && b === null) return 0;
  if (a === null) return 1;
  if (b === null) return -1;
  return dir === "asc" ? a - b : b - a;
}

// --- Search ------------------------------------------------------------------

const EMPTY = "";

// matchesSearch is a case-insensitive substring match across every given
// haystack field. An empty/whitespace needle matches everything.
export function matchesSearch(
  haystacks: ReadonlyArray<string | undefined | null>,
  needle: string,
): boolean {
  const n = needle.trim().toLowerCase();
  if (!n) return true;
  return haystacks.some((h) => (h ?? EMPTY).toLowerCase().includes(n));
}

// --- CostSummary (rollup tabs: Project/Task/Execution/Model) ---------------

// finishedMs extracts the group's finish time as epoch millis, or null when
// absent (backend did not send finished_at). Computed straight off the proto
// Timestamp's seconds/nanos because the field is optional — the wire value
// may not always carry a fully-populated Timestamp object.
export function finishedMs(ts: Timestamp | undefined): number | null {
  if (!ts) return null;
  return Number(ts.seconds ?? 0n) * 1000 + Math.floor((ts.nanos ?? 0) / 1e6);
}

export function summaryFinishedMs(s: CostSummary): number | null {
  return finishedMs(s.finishedAt);
}

// filterSummaries keeps rows whose display name (resolved via nameOf) or
// group key matches the needle case-insensitively.
export function filterSummaries<T extends CostSummary>(
  rows: T[],
  needle: string,
  nameOf: (row: T) => string,
): T[] {
  if (!needle.trim()) return rows;
  return rows.filter((r) =>
    matchesSearch(
      [nameOf(r), r.groupKey, r.groupBy, r.displayName],
      needle,
    ),
  );
}

// sortSummaries sorts a rollup list by cost/tokens/name/finished. It
// returns a new array; the input is never mutated. Ties break on name for
// deterministic output.
export function sortSummaries<T extends CostSummary>(
  rows: T[],
  sort: SortState,
  nameOf: (row: T) => string,
): T[] {
  if (rows.length === 0) return rows;
  return [...rows].sort((a, b) => {
    let cmp = 0;
    switch (sort.key) {
      case "cost":
        cmp = compareNumbers(toNum(a.costUsd), toNum(b.costUsd), sort.dir);
        break;
      case "tokens":
        cmp = compareNumbers(toNum(a.totalTokens), toNum(b.totalTokens), sort.dir);
        break;
      case "name":
        cmp = compareStrings(nameOf(a), nameOf(b), sort.dir);
        break;
      case "finished":
        cmp = compareFinished(
          summaryFinishedMs(a),
          summaryFinishedMs(b),
          sort.dir,
        );
        break;
    }
    return cmp !== 0 ? cmp : nameOf(a).localeCompare(nameOf(b));
  });
}

// --- Workflow panel (By Workflow tab) --------------------------------------

// a run matches if its work-item name / run id matches, or any of its workers
// matches by worker name / id.
function runMatches(run: WorkflowRunCost, needle: string): boolean {
  if (
    matchesSearch([run.workItemName, run.workflowRunId], needle)
  ) {
    return true;
  }
  return (run.workers ?? []).some((w) =>
    matchesSearch([w.workerName, w.workerId], needle),
  );
}

// filterWorkflowAggregates prunes the workflow tree: a workflow whose own name
// matches keeps all its runs; otherwise only the matching runs survive, and
// within a retained run only the matching workers survive.
export function filterWorkflowAggregates(
  workflows: WorkflowCostAggregate[],
  needle: string,
): WorkflowCostAggregate[] {
  if (!needle.trim()) return workflows;
  const out: WorkflowCostAggregate[] = [];
  for (const wf of workflows) {
    if (matchesSearch([wf.workflowName, wf.workflowId], needle)) {
      out.push(wf);
      continue;
    }
    const runs = (wf.runs ?? []).filter((run) => runMatches(run, needle));
    if (runs.length === 0) continue;
    out.push({
      ...wf,
      runs: runs.map((run) => {
        if (matchesSearch([run.workItemName, run.workflowRunId], needle)) {
          return run;
        }
        return {
          ...run,
          workers: (run.workers ?? []).filter((w) =>
            matchesSearch([w.workerName, w.workerId], needle),
          ),
        } as unknown as WorkflowRunCost;
      }),
    } as unknown as WorkflowCostAggregate);
  }
  return out;
}

function compareWorkflow(a: WorkflowCostAggregate, b: WorkflowCostAggregate, sort: SortState): number {
  let cmp = 0;
  switch (sort.key) {
    case "cost":
      cmp = compareNumbers(toNum(a.totalCostUsd), toNum(b.totalCostUsd), sort.dir);
      break;
    case "tokens":
      cmp = compareNumbers(toNum(a.totalTokens), toNum(b.totalTokens), sort.dir);
      break;
    case "name":
      cmp = compareStrings(a.workflowName || "", b.workflowName || "", sort.dir);
      break;
    case "finished":
      cmp = compareFinished(
        wfFinishedMs(a),
        wfFinishedMs(b),
        sort.dir,
      );
      break;
  }
  return cmp !== 0 ? cmp : a.workflowName.localeCompare(b.workflowName);
}

function compareRun(a: WorkflowRunCost, b: WorkflowRunCost, sort: SortState): number {
  let cmp = 0;
  switch (sort.key) {
    case "cost":
      cmp = compareNumbers(toNum(a.totalCostUsd), toNum(b.totalCostUsd), sort.dir);
      break;
    case "tokens":
      cmp = compareNumbers(toNum(a.totalTokens), toNum(b.totalTokens), sort.dir);
      break;
    case "name":
      cmp = compareStrings(a.workItemName || "", b.workItemName || "", sort.dir);
      break;
    case "finished":
      cmp = compareFinished(
        runFinishedMs(a),
        runFinishedMs(b),
        sort.dir,
      );
      break;
  }
  return cmp !== 0 ? cmp : a.workItemName.localeCompare(b.workItemName);
}

function wfFinishedMs(wf: WorkflowCostAggregate): number | null {
  return finishedMs(wf.finishedAt);
}

function runFinishedMs(run: WorkflowRunCost): number | null {
  return finishedMs(run.finishedAt);
}

// sortWorkflowAggregates sorts the workflow list and the runs within each
// workflow; worker rows inherit the run's order.
export function sortWorkflowAggregates(
  workflows: WorkflowCostAggregate[],
  sort: SortState,
): WorkflowCostAggregate[] {
  if (workflows.length === 0) return workflows;
  return [...workflows]
    .sort((a, b) => compareWorkflow(a, b, sort))
    .map((wf) => {
      if (!wf.runs || wf.runs.length === 0) return wf;
      return {
        ...wf,
        runs: [...wf.runs].sort((a, b) => compareRun(a, b, sort)),
      } as unknown as WorkflowCostAggregate;
    });
}

// --- Usage records table ----------------------------------------------------

export function usageRecordWorker(r: UsageRecord): string {
  return r.workerName || r.workerId || "";
}

export function usageRecordTask(r: UsageRecord): string {
  return r.taskTitle || r.taskId || "";
}

export function usageRecordWhenMs(r: UsageRecord): number | null {
  return finishedMs(r.occurredAt);
}

// filterUsageRecords keeps records matching worker/task/provider/model.
export function filterUsageRecords(
  records: UsageRecord[],
  needle: string,
): UsageRecord[] {
  if (!needle.trim()) return records;
  return records.filter((r) =>
    matchesSearch(
      [r.workerName, r.taskTitle, r.provider, r.model, r.workerId, r.taskId],
      needle,
    ),
  );
}

// sortUsageRecords sorts the usage records table by Worker/Task/Provider/
// Model/Tokens/Cost/When. Ties break deterministically by worker name then by
// when (most recent first) so the default view stays stable.
export function sortUsageRecords(
  records: UsageRecord[],
  sort: SortState<UsageSortKey>,
): UsageRecord[] {
  if (records.length === 0) return records;
  return [...records].sort((a, b) => {
    let cmp = 0;
    switch (sort.key) {
      case "worker":
        cmp = compareStrings(usageRecordWorker(a), usageRecordWorker(b), sort.dir);
        break;
      case "task":
        cmp = compareStrings(usageRecordTask(a), usageRecordTask(b), sort.dir);
        break;
      case "provider":
        cmp = compareStrings(a.provider || "", b.provider || "", sort.dir);
        break;
      case "model":
        cmp = compareStrings(a.model || "", b.model || "", sort.dir);
        break;
      case "tokens":
        cmp = compareNumbers(toNum(a.totalTokens), toNum(b.totalTokens), sort.dir);
        break;
      case "cost":
        cmp = compareNumbers(toNum(a.costUsd), toNum(b.costUsd), sort.dir);
        break;
      case "when":
        cmp = compareFinished(usageRecordWhenMs(a), usageRecordWhenMs(b), sort.dir);
        break;
    }
    return cmp !== 0
      ? cmp
      : usageRecordWorker(a).localeCompare(usageRecordWorker(b)) ||
          (usageRecordWhenMs(a) ?? 0) - (usageRecordWhenMs(b) ?? 0);
  });
}
