// Custom React Flow node for workflow steps. Renders a compact card with
// the step kind icon + name + ref (worker or work item). The wrapper
// className uses a kind-keyed color set so light/dark themes can be
// supported via CSS custom properties (see stepKindClasses).
//
// React Flow's Handle is the connection target — `target` on the left
// for incoming edges (= dependencies), `source` on the right for
// outgoing edges. Style must be set so Handles are visible.

import { Handle, Position, type NodeProps } from "reactflow";

import { cn } from "@/lib/utils";
import {
  STEP_KIND_ICONS,
  STEP_KIND_LABELS,
  type StepData,
} from "./stepKinds";

// Per-kind visual treatment. Background uses `hsl(var(--kind-...))` so
// the theme provider can override it; ring uses the kind's accent so the
// selected outline is visible on both light and dark themes.
export const stepKindClasses: Record<number, string> = {
  1: "border-sky-400/70 bg-sky-50 text-sky-950 dark:bg-sky-950/40 dark:text-sky-100",
  2: "border-amber-400/70 bg-amber-50 text-amber-950 dark:bg-amber-950/40 dark:text-amber-100",
  3: "border-yellow-500/70 bg-yellow-50 text-yellow-950 dark:bg-yellow-950/40 dark:text-yellow-100",
  4: "border-violet-400/70 bg-violet-50 text-violet-950 dark:bg-violet-950/40 dark:text-violet-100",
  5: "border-rose-400/70 bg-rose-50 text-rose-950 dark:bg-rose-950/40 dark:text-rose-100",
};

export const stepKindHandleClasses: Record<number, string> = {
  1: "!bg-sky-500",
  2: "!bg-amber-500",
  3: "!bg-yellow-500",
  4: "!bg-violet-500",
  5: "!bg-rose-500",
};

export function StepNode({ data, selected }: NodeProps<StepData>) {
  const kind = data.kind;
  const Icon = STEP_KIND_ICONS[kind] ?? STEP_KIND_ICONS[1];
  const label = STEP_KIND_LABELS[kind] ?? "step";
  // `ref` carries the worker ULID for task steps. The properties panel
  // shows the full ref; the node card truncates it to keep the card
  // small.
  const refShort = data.ref ? data.ref.slice(0, 14) + (data.ref.length > 14 ? "…" : "") : "";
  return (
    <div
      className={cn(
        "min-w-[160px] max-w-[220px] rounded-md border px-3 py-2 shadow-sm",
        stepKindClasses[kind] ?? "border-border bg-card text-card-foreground",
        selected && "ring-2 ring-primary ring-offset-2 ring-offset-background",
      )}
    >
      <Handle
        type="target"
        position={Position.Left}
        className={cn("!h-2.5 !w-2.5 !border-2 !border-background", stepKindHandleClasses[kind])}
      />
      <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide opacity-80">
        <Icon className="h-3 w-3" />
        {label}
      </div>
      <div className="mt-1 truncate text-sm font-semibold" title={data.name}>
        {data.name || <span className="italic opacity-60">untitled</span>}
      </div>
      {refShort && (
        <div className="mt-0.5 truncate font-mono text-[10px] opacity-70" title={data.ref}>
          {refShort}
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
        className={cn("!h-2.5 !w-2.5 !border-2 !border-background", stepKindHandleClasses[kind])}
      />
    </div>
  );
}
