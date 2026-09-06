// Pure helpers + data-join for the Workflow Run Heads-Up Dashboard
// (tiled HUD default run view). No React here so every function is
// unit-testable in isolation (see headsUp.test.ts).
//
// The grid joins DAG steps (workflow_versions.steps JSON) LEFT JOIN
// step-runs (latest non-superseded row per step) LEFT JOIN worker
// executions (via workerExecutionId). Only the single live tile
// subscribes to an execution event stream; every other tile renders a
// static snapshot from the durable session transcript.

import type { WorkerExecution } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { WorkflowStepRun } from "@/api/gen/orchicon/api/v1/workflow_pb";

export type RunView = "tile" | "graph";

const VIEW_STORAGE_KEY = "orchicon:run-view";

/**
 * Coerce an unknown `?view=` value to a RunView. Anything but "graph"
 * (missing, garbage, "tile") falls back to the tile default so a bad
 * shared link can never crash the route's search validation.
 */
export function parseViewParam(value: unknown): RunView {
  return value === "graph" ? "graph" : "tile";
}

export function readStoredView(): RunView {
  try {
    if (typeof localStorage === "undefined") return "tile";
    return parseViewParam(localStorage.getItem(VIEW_STORAGE_KEY));
  } catch {
    return "tile";
  }
}

export function storeView(view: RunView): void {
  try {
    if (typeof localStorage === "undefined") return;
    localStorage.setItem(VIEW_STORAGE_KEY, view);
  } catch {
    /* private-mode / test env — persistence is best-effort */
  }
}

export const STEP_KIND_LABELS: Record<number, string> = {
  1: "worker",
  2: "conditional",
  3: "approval",
  4: "parallel",
  5: "recover",
  6: "work_item",
  7: "project",
  8: "loop_decision",
  9: "policy",
};

export const STEP_RUN_STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "ready",
  3: "running",
  4: "succeeded",
  5: "failed",
  6: "skipped",
  7: "blocked",
  8: "approval_pending",
};

export const STEP_RUN_STATUS_COLORS: Record<number, string> = {
  1: "bg-gray-200 text-gray-700", // pending
  2: "bg-yellow-100 text-yellow-800", // ready
  3: "bg-blue-100 text-blue-800", // running
  4: "bg-green-100 text-green-800", // succeeded
  5: "bg-red-100 text-red-800", // failed
  6: "bg-gray-300 text-gray-600", // skipped
  7: "bg-red-200 text-red-900", // blocked
  8: "bg-amber-100 text-amber-900", // approval_pending
};

export const EXEC_STATUS_LABELS: Record<number, string> = {
  1: "dispatching",
  2: "running",
  3: "healthy",
  4: "stalled",
  5: "unhealthy",
  6: "terminating",
  7: "terminated",
  8: "failed_to_start",
  9: "succeeded",
  10: "failed",
};

export const EXEC_STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-green-600 text-white",
  4: "bg-yellow-100 text-yellow-800",
  5: "bg-red-100 text-red-800",
  6: "bg-orange-100 text-orange-800",
  7: "bg-gray-200 text-gray-700",
  8: "bg-red-600 text-white",
  9: "bg-emerald-100 text-emerald-800",
  10: "bg-red-700 text-white",
};

/** Status → pill label + tailwind classes. Unknown statuses degrade to pending. */
export function tileStatusStyle(status: number): {
  label: string;
  className: string;
} {
  return {
    label: STEP_RUN_STATUS_LABELS[status] ?? "pending",
    className: STEP_RUN_STATUS_COLORS[status] ?? "bg-gray-200 text-gray-700",
  };
}

export interface HudStepInput {
  id: string;
  name?: string;
  kind?: string;
  ref?: string;
  depends_on?: string[];
}

/** Parse the workflow_versions.steps JSON defensively (never throws). */
export function parseSteps(stepsJson: string | undefined | null): HudStepInput[] {
  if (!stepsJson) return [];
  try {
    const parsed: unknown = JSON.parse(stepsJson);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (s): s is HudStepInput =>
        !!s && typeof s === "object" && typeof (s as { id?: unknown }).id === "string",
    );
  } catch {
    return [];
  }
}

const KIND_STR_TO_NUM: Record<string, number> = {
  task: 1,
  decision: 2,
  approval: 3,
  parallel: 4,
  recover: 5,
  work_item: 6,
  project: 7,
  loop_decision: 8,
  policy: 9,
};

export function stepKindNum(kind: string | undefined): number {
  if (kind && KIND_STR_TO_NUM[kind] !== undefined) return KIND_STR_TO_NUM[kind];
  return 1;
}

/**
 * Order step ids by DAG depth (longest depends_on chain), ties broken by
 * canvas order. Depth = queue position driver: depth 0 steps run first.
 * Unknown deps are ignored; dependency cycles are guarded (treated as
 * depth 0) so a corrupt DAG can never hang the view.
 */
export function computeQueueOrder(steps: HudStepInput[]): string[] {
  const byId = new Map(steps.map((s) => [s.id, s]));
  const memo = new Map<string, number>();
  const visiting = new Set<string>();
  const depth = (id: string): number => {
    const hit = memo.get(id);
    if (hit !== undefined) return hit;
    if (visiting.has(id)) return 0; // cycle guard
    visiting.add(id);
    let d = 0;
    for (const dep of byId.get(id)?.depends_on ?? []) {
      if (!byId.has(dep)) continue;
      d = Math.max(d, depth(dep) + 1);
    }
    visiting.delete(id);
    memo.set(id, d);
    return d;
  };
  return steps
    .map((s, i) => ({ id: s.id, d: depth(s.id), i }))
    .sort((a, b) => a.d - b.d || a.i - b.i)
    .map((e) => e.id);
}

/** 1-based queue position of a step in DAG order; 0 when unknown. */
export function queuePosition(steps: HudStepInput[], stepId: string): number {
  const idx = computeQueueOrder(steps).indexOf(stepId);
  return idx < 0 ? 0 : idx + 1;
}

/**
 * Loop-decision outcome tag, mirroring the graph RunStepNode loopTag
 * logic so both views read identically (re-ask / loop→X / decision).
 */
export function loopOutcomeTag(
  sr: Pick<WorkflowStepRun, "result" | "status" | "stepKind"> | null | undefined,
): string | undefined {
  if (!sr || sr.stepKind !== 8) return undefined;
  try {
    const parsed = sr.result ? JSON.parse(sr.result) : null;
    if (parsed && typeof parsed === "object") {
      if (parsed.loop === "re-ask") return "re-ask reviewer";
      if (parsed.loop) return `loop → ${String(parsed.loop).slice(0, 16)}`;
      if (parsed.decision) return `decision: ${String(parsed.decision)}`;
      if (sr.status === 4) return "decision made";
      if (sr.status === 3) return "evaluating…";
      if (sr.status === 5) return "max iterations reached";
      return undefined;
    }
    if (sr.status === 4) return "decision made";
    if (sr.status === 5) return "max iterations reached";
    return undefined;
  } catch {
    return undefined;
  }
}

export interface SessionPartLike {
  kind: string;
  payload: Uint8Array;
}

/**
 * Last durable `kind:text` block from the execution session transcript
 * (execution_session_parts). Tiles use this — NEVER raw streaming chunks,
 * which arrive fragmented at ~40 chars and read as noise when shrunk.
 */
export function extractLastTextBlock(
  parts: SessionPartLike[] | undefined,
  maxChars = 2000,
): string {
  if (!parts?.length) return "";
  const decode = new TextDecoder();
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i];
    if (p.kind !== "text") continue;
    try {
      const pl = JSON.parse(decode.decode(p.payload)) as {
        part?: { text?: unknown };
      };
      const text = pl?.part?.text;
      if (typeof text === "string" && text.trim()) {
        const trimmed = text.trim();
        return trimmed.length > maxChars
          ? trimmed.slice(0, maxChars) + "…"
          : trimmed;
      }
    } catch {
      /* unparseable part — keep scanning older blocks */
    }
  }
  return "";
}

/** Fallback one-liner from the step-run result JSON (`_summary` head). */
export function resultSummaryLine(
  sr: Pick<WorkflowStepRun, "result"> | null | undefined,
): string {
  if (!sr?.result) return "";
  try {
    const r = JSON.parse(sr.result) as { _summary?: unknown };
    if (typeof r._summary === "string" && r._summary) {
      return r._summary.split("\n")[0].slice(0, 160);
    }
  } catch {
    /* not JSON — no summary */
  }
  return "";
}

/**
 * HeadsUpTileData — one tile per DAG step.
 *
 * TUI CONTRACT: this is the row model the future TUI default renders.
 * The TUI consumes stepId/stepName/stepKindLabel/queueIndex/isActive/
 * isUpcoming + the summary string rules documented on the tile (live
 * tail only when isActive, durable last-text-block otherwise, upcoming
 * line when there is no execution) — keep these fields stable.
 */
export interface HeadsUpTileData {
  stepId: string;
  stepName: string;
  stepKind: number;
  stepKindLabel: string;
  /** 1-based position in DAG (depends_on depth, then canvas) order. */
  queueIndex: number;
  /** Longest depends_on chain above this step (0 = runs first). */
  depth: number;
  /** Latest non-superseded step run, or null when never dispatched. */
  stepRun: WorkflowStepRun | null;
  /** Joined worker execution, or null when not yet dispatched. */
  execution: WorkerExecution | null;
  /** stepRun.status === RUNNING (3). */
  isActive: boolean;
  /** No worker execution yet (pending/ready, waiting for dispatch). */
  isUpcoming: boolean;
  /** loop_decision outcome tag (mirror of the graph loopTag). */
  loopOutcome?: string;
}

/**
 * Build one tile per DAG step: steps LEFT JOIN latest step-run LEFT JOIN
 * execution. A loop re-ask that superseded the prior row must not shadow
 * the new run's status, so superseded rows are skipped and the highest
 * iteration wins (mirrors RunViewInner's latestByStep).
 */
export function buildHeadsUpTiles(
  stepsJson: string | undefined | null,
  stepRuns: readonly WorkflowStepRun[] | undefined,
  execs: readonly WorkerExecution[] | undefined,
): HeadsUpTileData[] {
  const steps = parseSteps(stepsJson);
  const order = computeQueueOrder(steps);
  const queueIndexById = new Map(order.map((id, i) => [id, i + 1]));
  const byId = new Map(steps.map((s) => [s.id, s]));
  const depthMemo = new Map<string, number>();
  const visiting = new Set<string>();
  const depthOf = (id: string): number => {
    const hit = depthMemo.get(id);
    if (hit !== undefined) return hit;
    if (visiting.has(id)) return 0;
    visiting.add(id);
    let d = 0;
    for (const dep of byId.get(id)?.depends_on ?? []) {
      if (!byId.has(dep)) continue;
      d = Math.max(d, depthOf(dep) + 1);
    }
    visiting.delete(id);
    depthMemo.set(id, d);
    return d;
  };
  const latestByStep = new Map<string, WorkflowStepRun>();
  for (const srun of [...(stepRuns ?? [])].sort((a, b) => b.iteration - a.iteration)) {
    if (srun.supersededBy) continue;
    if (!latestByStep.has(srun.stepId)) latestByStep.set(srun.stepId, srun);
  }
  const execById = new Map((execs ?? []).map((e) => [e.id, e]));
  const tiles = steps.map((s) => {
    const kind = stepKindNum(s.kind);
    const srun = latestByStep.get(s.id) ?? null;
    const execId = srun?.workerExecutionId || "";
    const execution = execId ? (execById.get(execId) ?? null) : null;
    return {
      stepId: s.id,
      stepName: s.name || s.id,
      stepKind: kind,
      stepKindLabel: STEP_KIND_LABELS[kind] ?? "step",
      queueIndex: queueIndexById.get(s.id) ?? 0,
      depth: depthOf(s.id),
      stepRun: srun,
      execution,
      isActive: (srun?.status ?? 1) === 3,
      isUpcoming: !srun?.workerExecutionId,
      loopOutcome: loopOutcomeTag(srun),
    } satisfies HeadsUpTileData;
  });
  // Output in DAG (queue) order, not raw steps-JSON canvas order —
  // queueIndex is unique 1..n so this sort is deterministic.
  tiles.sort((a, b) => a.queueIndex - b.queueIndex);
  return tiles;
}
