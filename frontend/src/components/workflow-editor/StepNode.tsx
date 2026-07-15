// Custom React Flow node for workflow steps. Renders a compact card
// with the step kind icon + name + ref (worker or work item) + a gate
// badge if a policy is attached. The wrapper className uses a
// kind-keyed color set so light/dark themes can be supported via CSS
// custom properties.
//
// Visual distinction between the three first-class PR A node types:
//   - TASK (1)    — worker. Shows the worker's name + model.
//   - WORK_ITEM (6) — passive marker. Shows the work item's title +
//                     kind + status badge.
//   - PROJECT (7)  — passive marker. Shows the project name + status.
//   - DECISION/APPROVAL/PARALLEL/RECOVER — control flow (unchanged).
//
// React Flow's Handle is the connection target — `target` on the left
// for incoming edges (= dependencies), `source` on the right for
// outgoing edges. Style must be set so Handles are visible.

import { Handle, Position, type NodeProps } from "reactflow";

import { cn } from "@/lib/utils";
import {
  STEP_KIND,
  STEP_KIND_ICONS,
  STEP_KIND_LABELS,
  type StepData,
} from "./stepKinds";

// Per-kind visual treatment. Background uses `hsl(var(--kind-...))` so
// the theme provider can override it; ring uses the kind's accent so the
// selected outline is visible on both light and dark themes.
export const stepKindClasses: Record<number, string> = {
  [STEP_KIND.TASK]: "border-sky-400/70 bg-sky-50 text-sky-950 dark:bg-sky-950/40 dark:text-sky-100",
  [STEP_KIND.DECISION]: "border-amber-400/70 bg-amber-50 text-amber-950 dark:bg-amber-950/40 dark:text-amber-100",
  [STEP_KIND.APPROVAL]: "border-yellow-500/70 bg-yellow-50 text-yellow-950 dark:bg-yellow-950/40 dark:text-yellow-100",
  [STEP_KIND.PARALLEL]: "border-violet-400/70 bg-violet-50 text-violet-950 dark:bg-violet-950/40 dark:text-violet-100",
  [STEP_KIND.RECOVER]: "border-rose-400/70 bg-rose-50 text-rose-950 dark:bg-rose-950/40 dark:text-rose-100",
  [STEP_KIND.WORK_ITEM]:
    "border-emerald-400/70 bg-emerald-50 text-emerald-950 dark:bg-emerald-950/40 dark:text-emerald-100",
  [STEP_KIND.PROJECT]:
    "border-indigo-400/70 bg-indigo-50 text-indigo-950 dark:bg-indigo-950/40 dark:text-indigo-100",
};

export const stepKindHandleClasses: Record<number, string> = {
  [STEP_KIND.TASK]: "!bg-sky-500",
  [STEP_KIND.DECISION]: "!bg-amber-500",
  [STEP_KIND.APPROVAL]: "!bg-yellow-500",
  [STEP_KIND.PARALLEL]: "!bg-violet-500",
  [STEP_KIND.RECOVER]: "!bg-rose-500",
  [STEP_KIND.WORK_ITEM]: "!bg-emerald-500",
  [STEP_KIND.PROJECT]: "!bg-indigo-500",
};

export function StepNode({ data, selected }: NodeProps<StepData>) {
  const kind = data.kind;
  const Icon = STEP_KIND_ICONS[kind] ?? STEP_KIND_ICONS[STEP_KIND.TASK];
  const label = STEP_KIND_LABELS[kind] ?? "step";
  // `ref` carries the worker ULID for task steps. The properties panel
  // shows the full ref; the node card truncates it to keep the card
  // small.
  const refShort = data.ref ? data.ref.slice(0, 14) + (data.ref.length > 14 ? "…" : "") : "";
  // For work item / project nodes, prefer the display name from config
  // (set by the palette when the tile is dragged). Falls back to ref
  // for older workflows.
  const config = parseConfig(data.config);
  const ctxRef = (config.work_item_id as string) || (config.project_id as string) || "";
  const ctxShort = ctxRef
    ? ctxRef.slice(0, 14) + (ctxRef.length > 14 ? "…" : "")
    : "";
  const hasBinding = kind === STEP_KIND.TASK ? !!data.ref : !!ctxRef;
  return (
    <div
      className={cn(
        "min-w-[180px] max-w-[240px] rounded-md border px-3 py-2 shadow-sm",
        stepKindClasses[kind] ?? "border-border bg-card text-card-foreground",
        selected && "ring-2 ring-primary ring-offset-2 ring-offset-background",
        // Incomplete node: red dashed border if a binding is missing
        // (task without worker ref, work_item without id, project
        // without id). Visually nudges the author to fill the gap.
        !hasBinding &&
          kind !== STEP_KIND.DECISION &&
          kind !== STEP_KIND.APPROVAL &&
          kind !== STEP_KIND.PARALLEL &&
          kind !== STEP_KIND.RECOVER &&
          "border-dashed border-rose-400",
      )}
    >
      <Handle
        type="target"
        position={Position.Left}
        className={cn(
          "!h-2.5 !w-2.5 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide opacity-80">
          <Icon className="h-3 w-3" />
          {label}
        </div>
        {!hasBinding &&
          kind !== STEP_KIND.DECISION &&
          kind !== STEP_KIND.APPROVAL &&
          kind !== STEP_KIND.PARALLEL &&
          kind !== STEP_KIND.RECOVER && (
            <span
              className="rounded-full bg-rose-200 px-1.5 py-0.5 text-[8px] font-semibold uppercase tracking-wide text-rose-900 dark:bg-rose-900/60 dark:text-rose-100"
              title="Missing required binding"
            >
              incomplete
            </span>
          )}
      </div>
      <div className="mt-1 truncate text-sm font-semibold" title={data.name}>
        {data.name || <span className="italic opacity-60">untitled</span>}
      </div>
      {/* For task steps, show the worker's ref. For work_item / project
          steps, show the bound entity id from config. */}
      {kind === STEP_KIND.TASK && refShort && (
        <div className="mt-0.5 truncate font-mono text-[10px] opacity-70" title={data.ref}>
          {refShort}
        </div>
      )}
      {(kind === STEP_KIND.WORK_ITEM || kind === STEP_KIND.PROJECT) && ctxShort && (
        <div
          className="mt-0.5 truncate font-mono text-[10px] opacity-70"
          title={ctxRef}
        >
          {ctxShort}
        </div>
      )}
      {data.gatePolicyRef && (
        <div className="mt-1 flex items-center gap-1 truncate rounded bg-black/10 px-1 py-0.5 text-[9px] font-medium uppercase dark:bg-white/10">
          <span className="opacity-70">gate</span>
          <span className="font-mono normal-case opacity-90">
            {data.gatePolicyRef.slice(0, 16)}
            {data.gatePolicyRef.length > 16 ? "…" : ""}
          </span>
        </div>
      )}
      <Handle
        type="source"
        position={Position.Right}
        className={cn(
          "!h-2.5 !w-2.5 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />
    </div>
  );
}

// parseConfig defensively reads the step's config JSON. Returns {} for
// empty / malformed input.
function parseConfig(config: string): Record<string, unknown> {
  if (!config) return {};
  try {
    const parsed = JSON.parse(config);
    if (parsed && typeof parsed === "object") return parsed as Record<string, unknown>;
  } catch {
    /* fall through */
  }
  return {};
}
