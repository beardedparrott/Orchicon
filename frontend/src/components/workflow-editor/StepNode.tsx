import { Handle, Position, type NodeProps } from "reactflow";
import { X } from "lucide-react";

import { cn } from "@/lib/utils";
import {
  RECOVERY_STRATEGY_LABELS,
  STEP_KIND,
  STEP_KIND_DISPLAY_LABELS,
  STEP_KIND_ICONS,
  type StepData,
} from "./stepKinds";

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
  [STEP_KIND.POLICY]:
    "border-amber-400/70 bg-amber-50 text-amber-950 dark:bg-amber-950/40 dark:text-amber-100",
};

export const stepKindHandleClasses: Record<number, string> = {
  [STEP_KIND.TASK]: "!bg-sky-500",
  [STEP_KIND.DECISION]: "!bg-amber-500",
  [STEP_KIND.APPROVAL]: "!bg-yellow-500",
  [STEP_KIND.PARALLEL]: "!bg-violet-500",
  [STEP_KIND.RECOVER]: "!bg-rose-500",
  [STEP_KIND.WORK_ITEM]: "!bg-emerald-500",
  [STEP_KIND.PROJECT]: "!bg-indigo-500",
  [STEP_KIND.POLICY]: "!bg-amber-500",
};

export function StepNode({ data, selected }: NodeProps<StepData>) {
  const kind = data.kind;
  const Icon = STEP_KIND_ICONS[kind] ?? STEP_KIND_ICONS[STEP_KIND.TASK];
  const label = STEP_KIND_DISPLAY_LABELS[kind] ?? "step";
  const cfg = parseConfig(data.config);

  const hasBinding =
    kind === STEP_KIND.TASK
      ? !!data.ref
      : kind === STEP_KIND.WORK_ITEM
        ? !!cfg.work_item_id
        : kind === STEP_KIND.PROJECT
          ? !!cfg.project_id
          : kind === STEP_KIND.POLICY
            ? !!data.gatePolicyRef
            : kind === STEP_KIND.RECOVER
              ? !!cfg.strategy
              : true;

  const needsBinding =
    kind !== STEP_KIND.DECISION &&
    kind !== STEP_KIND.APPROVAL &&
    kind !== STEP_KIND.PARALLEL;

  return (
    <div
      className={cn(
        "group relative min-w-[180px] max-w-[240px] min-h-[76px] rounded-md border px-3 py-2 shadow-sm",
        stepKindClasses[kind] ?? "border-border bg-card text-card-foreground",
        selected && "ring-2 ring-primary ring-offset-2 ring-offset-background",
        !hasBinding && needsBinding && "border-dashed border-rose-400",
      )}
    >
      <Handle
        type="target"
        id="target-left"
        position={Position.Left}
        className={cn(
          "!h-2.5 !w-2.5 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />
      <Handle
        type="target"
        id="target-top"
        position={Position.Top}
        className={cn(
          "!h-2 !w-2 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />

      <button
        type="button"
        aria-label="Delete step"
        title="Delete step"
        data-on-delete
        onClick={(e) => {
          e.stopPropagation();
          window.dispatchEvent(
            new CustomEvent("orchicon:delete-node", {
              detail: {
                id: (e.currentTarget as HTMLElement)
                  .closest(".react-flow__node")
                  ?.getAttribute("data-id"),
              },
            }),
          );
        }}
        className="absolute -right-2 -top-2 hidden h-5 w-5 items-center justify-center rounded-full border bg-background text-muted-foreground shadow-sm hover:bg-rose-100 hover:text-rose-700 group-hover:flex dark:hover:bg-rose-950/60"
      >
        <X className="h-3 w-3" />
      </button>

      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide opacity-80">
          <Icon className="h-3 w-3" />
          {label}
        </div>
        {!hasBinding && needsBinding && (
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

      {kind === STEP_KIND.TASK && data.ref && (
        <div className="mt-0.5 truncate font-mono text-[10px] opacity-70">
          {data.ref.slice(0, 16)}...
        </div>
      )}

      {kind === STEP_KIND.RECOVER && typeof cfg.strategy === "string" && (
        <div className="mt-1 flex items-center gap-1 truncate rounded px-1 py-0.5 text-[10px] font-medium text-rose-600 dark:text-rose-300">
          <span>{RECOVERY_STRATEGY_LABELS[cfg.strategy] ?? cfg.strategy}</span>
          {(typeof cfg.max_retries === "number" || typeof cfg.retry_delay_seconds === "number") && (
            <span className="ml-1 opacity-70">
              · {cfg.max_retries ?? 5}× × {cfg.retry_delay_seconds ?? 10}s
            </span>
          )}
        </div>
      )}

      {data.gatePolicyRef && kind !== STEP_KIND.POLICY && (
        <div className="mt-1 flex items-center gap-1 truncate rounded bg-black/10 px-1 py-0.5 text-[9px] font-medium uppercase dark:bg-white/10">
          <span className="opacity-70">gate</span>
          <span className="font-mono normal-case opacity-90">
            {data.gatePolicyRef.slice(0, 16)}
            {data.gatePolicyRef.length > 16 ? "…" : ""}
          </span>
        </div>
      )}

      {kind === STEP_KIND.POLICY && data.gatePolicyRef && (
        <div className="mt-0.5 truncate font-mono text-[10px] opacity-70">
          {data.gatePolicyRef.slice(0, 16)}...
        </div>
      )}

      <Handle
        type="source"
        id="source-right"
        position={Position.Right}
        className={cn(
          "!h-2.5 !w-2.5 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />
      <Handle
        type="source"
        id="source-bottom"
        position={Position.Bottom}
        className={cn(
          "!h-2 !w-2 !border-2 !border-background",
          stepKindHandleClasses[kind],
        )}
      />
    </div>
  );
}

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
