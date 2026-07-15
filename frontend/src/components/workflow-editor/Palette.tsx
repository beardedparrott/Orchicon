// Palette — the left column of the workflow editor.
//
// Four sections (each collapsible), each tile is a draggable card. The
// dataTransfer payload is a PaletteDropPayload (see stepKinds.ts) and
// the editor's onDrop converts it into a StepData node.
//
// Sections:
//   1. Workers    — published workers → task step (kind=1) with ref
//   2. Work items — recent project work items → task step (kind=1) with
//                   config.work_item_id; title/desc/AC pre-populate the
//                   step's metadata
//   3. Policies   — published policies → any step kind, with
//                   gate_policy_ref set
//   4. Primitives — Decision / Approval / Parallel / Recover
//
// All tile text is rendered through the shared tooltip helper so users
// see a one-liner + example on hover (docs/10 §11 — discoverability).

import { useMemo, useRef, useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  Info,
  Search,
  Workflow as WorkflowIcon,
  X,
  type LucideIcon,
} from "lucide-react";

import { Input } from "@/components/ui/input";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

import { useListWorkers } from "@/api/workers";
import { useListWorkItems } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListPolicies } from "@/api/policies";
import { PolicyStatus } from "@/api/gen/orchicon/api/v1/policy_pb";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";

import {
  PALETTE_MIME,
  POLICY_ICON,
  STEP_KIND,
  STEP_KIND_ICONS,
  STEP_KIND_LABELS,
  WORKER_ICON,
  WORKITEM_ICON,
  type PaletteDropPayload,
} from "./stepKinds";

// Step descriptions shown on hover. Each entry is one short line + an
// example payload. The tooltip is the "what does this do" entry-point
// for first-time users.
const STEP_KIND_DESCRIPTIONS: Record<number, { summary: string; example: string }> = {
  1: {
    summary: "Runs a Worker on the task.",
    example: "Ref: worker ULID, version (0 = latest)",
  },
  2: {
    summary: "Branches based on a Rego policy.",
    example: "Wire the chosen branch downstream.",
  },
  3: {
    summary: "Blocks until a human approves.",
    example: "Resolution: approve / deny / comment.",
  },
  4: {
    summary: "Fans out to every downstream step.",
    example: "All children run in parallel.",
  },
  5: {
    summary: "Triggers recovery on upstream failure.",
    example: "Summarize + retry or escalate.",
  },
};

const WORKER_DESCRIPTION =
  "Creates a task step that dispatches this worker. Drag it onto the canvas to add it as a runnable step.";
const WORKITEM_DESCRIPTION =
  "Creates a task step pre-populated from this work item's title, description, and acceptance criteria. The work item ID is linked so the runtime can pull the latest metadata.";
const POLICY_DESCRIPTION =
  "Adds this policy as the gate for a step. Drag it onto an existing step to attach it, or onto the canvas to add a new step with this gate.";

export function Palette({
  projectId,
  readOnly,
}: {
  projectId: string;
  readOnly: boolean;
}) {
  const { data: workers } = useListWorkers();
  const { data: projects } = useListProjects();
  const { data: workItems } = useListWorkItems(projectId || "", {
    status: WorkItemStatus.READY,
  });
  const { data: allWorkItems } = useListWorkItems(projectId || "", {});
  const { data: policies } = useListPolicies({ status: PolicyStatus.PUBLISHED });

  const published = (workers ?? []).filter((w) => w.status === 2);
  // Merge ready + recent: ready first, then any other status up to a cap.
  const workItemList = useMemo(() => {
    const ready = (workItems ?? []).slice(0, 12);
    const others = (allWorkItems ?? [])
      .filter((w) => !ready.some((r) => r.id === w.id))
      .slice(0, 8);
    return [...ready, ...others];
  }, [workItems, allWorkItems]);
  const policyList = (policies ?? []).filter((p) => p.status === PolicyStatus.PUBLISHED).slice(0, 12);

  const projectName = (projects ?? []).find((p) => p.id === projectId)?.name ?? "—";

  const [search, setSearch] = useState("");
  const filter = (s: string) =>
    search.trim() ? s.toLowerCase().includes(search.trim().toLowerCase()) : true;

  return (
    <div className="space-y-3">
      <PaletteSearch value={search} onChange={setSearch} />
      <Section
        title="Workers"
        icon={WORKER_ICON}
        subtitle="Tasks that run a worker"
        empty={published.length === 0 ? "No published workers yet." : undefined}
      >
        {published.filter((w) => filter(w.name) || filter(w.slug)).map((w) => (
          <DraggableTile
            key={w.id}
            label={w.name}
            sublabel={w.slug}
            icon={WORKER_ICON}
            kindAccent="sky"
            payload={{
              kind: STEP_KIND.TASK,
              name: w.name,
              ref: w.id,
              workerId: w.id,
            }}
            description={WORKER_DESCRIPTION}
            readOnly={readOnly}
          />
        ))}
      </Section>
      <Section
        title="Work items"
        icon={WORKITEM_ICON}
        subtitle={`From project: ${projectName}`}
        empty={
          !projectId
            ? "Tenant-template workflow — assign a project to see its work items."
            : workItemList.length === 0
              ? "No work items in this project yet."
              : undefined
        }
      >
        {workItemList
          .filter((w) => filter(w.title) || filter(w.description))
          .map((w) => (
            <DraggableTile
              key={w.id}
              label={w.title}
              sublabel={`${KIND_LABEL[w.kind] ?? "task"} · ${STATUS_LABEL[w.status] ?? "—"}`}
              icon={WORKITEM_ICON}
              kindAccent="emerald"
              payload={{
                kind: STEP_KIND.TASK,
                name: w.title,
                workItemId: w.id,
              }}
              description={WORKITEM_DESCRIPTION}
              readOnly={readOnly}
            />
          ))}
      </Section>
      <Section
        title="Policies"
        icon={POLICY_ICON}
        subtitle="Rego gate rules"
        empty={policyList.length === 0 ? "No published policies." : undefined}
      >
        {policyList.filter((p) => filter(p.name)).map((p) => (
          <DraggableTile
            key={p.id}
            label={p.name}
            sublabel={`gate · v${p.currentVersion}`}
            icon={POLICY_ICON}
            kindAccent="amber"
            payload={{
              kind: STEP_KIND.TASK,
              name: p.name,
              policyId: p.id,
            }}
            description={POLICY_DESCRIPTION}
            readOnly={readOnly}
          />
        ))}
      </Section>
      <Section
        title="Step primitives"
        icon={WorkflowIcon}
        subtitle="Control flow building blocks"
      >
        {[
          STEP_KIND.DECISION,
          STEP_KIND.APPROVAL,
          STEP_KIND.PARALLEL,
          STEP_KIND.RECOVER,
        ].map((kind) => {
          const Icon = STEP_KIND_ICONS[kind];
          const meta = STEP_KIND_DESCRIPTIONS[kind];
          return (
            <DraggableTile
              key={kind}
              label={STEP_KIND_LABELS[kind]}
              sublabel={meta.summary}
              icon={Icon}
              kindAccent={KIND_ACCENT[kind]}
              payload={{ kind }}
              description={meta.summary}
              example={meta.example}
              readOnly={readOnly}
            />
          );
        })}
      </Section>
      <PaletteFooter />
    </div>
  );
}

function PaletteSearch({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="relative">
      <Search
        className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
        aria-hidden
      />
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="Filter palette…"
        className="h-7 pl-7 pr-7 text-xs"
        aria-label="Filter palette"
      />
      {value && (
        <button
          type="button"
          onClick={() => onChange("")}
          className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-muted-foreground hover:bg-muted"
          aria-label="Clear search"
        >
          <X className="h-3 w-3" />
        </button>
      )}
    </div>
  );
}

function Section({
  title,
  subtitle,
  icon: Icon,
  children,
  empty,
  defaultOpen = true,
}: {
  title: string;
  subtitle?: string;
  icon: LucideIcon;
  children?: React.ReactNode;
  empty?: string;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div className="rounded-md border bg-card/40">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left"
        aria-expanded={open}
      >
        {open ? (
          <ChevronDown className="h-3 w-3 text-muted-foreground" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 text-muted-foreground" aria-hidden />
        )}
        <Icon className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
        <span className="text-xs font-semibold uppercase tracking-wide text-foreground">
          {title}
        </span>
        {subtitle && (
          <span className="ml-auto truncate text-[10px] text-muted-foreground">{subtitle}</span>
        )}
      </button>
      {open && (
        <div className="space-y-1.5 px-2 pb-2">
          {empty ? (
            <p className="px-1 py-2 text-[10px] italic text-muted-foreground">{empty}</p>
          ) : (
            children
          )}
        </div>
      )}
    </div>
  );
}

// DraggableTile is a single palette entry. The element ref is used for
// the drag image (a translucent clone that follows the cursor during
// drag), giving a more polished drag feel than the default browser
// snapshot.
function DraggableTile({
  label,
  sublabel,
  icon: Icon,
  kindAccent,
  payload,
  description,
  example,
  readOnly,
}: {
  label: string;
  sublabel?: string;
  icon: LucideIcon;
  kindAccent: string;
  payload: PaletteDropPayload;
  description: string;
  example?: string;
  readOnly: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const setData = (e: React.DragEvent) => {
    e.dataTransfer.setData(PALETTE_MIME, JSON.stringify(payload));
    e.dataTransfer.setData("text/plain", label);
    // `copyMove` permits both copy and move effectAllowed so any
    // dropEffect the canvas uses is accepted by the browser.
    e.dataTransfer.effectAllowed = "copyMove";
    if (ref.current) {
      // Use a hidden clone as the drag image so the cursor preview
      // matches the tile's appearance rather than the OS default.
      const ghost = ref.current.cloneNode(true) as HTMLElement;
      ghost.style.position = "absolute";
      ghost.style.top = "-1000px";
      ghost.style.left = "-1000px";
      ghost.style.width = `${ref.current.offsetWidth}px`;
      ghost.style.opacity = "0.85";
      document.body.appendChild(ghost);
      e.dataTransfer.setDragImage(ghost, 12, 12);
      // Detach after the browser has snapshotted the drag image.
      setTimeout(() => ghost.remove(), 0);
    }
  };
  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>
          <div
            ref={ref}
            draggable={!readOnly}
            onDragStart={readOnly ? undefined : setData}
            className={cn(
              "group flex cursor-grab items-start gap-2 rounded-md border bg-background p-2 text-xs shadow-sm transition-colors",
              "hover:border-foreground/30 hover:bg-accent/40 active:cursor-grabbing",
              readOnly && "cursor-not-allowed opacity-50",
              ACCENT_BORDER[kindAccent],
            )}
            role="button"
            aria-label={`Drag ${label}`}
            tabIndex={readOnly ? -1 : 0}
          >
            <span
              className={cn(
                "mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded",
                ACCENT_BG[kindAccent],
              )}
              aria-hidden
            >
              <Icon className="h-3 w-3" />
            </span>
            <span className="min-w-0 flex-1">
              <span className="block truncate font-medium" title={label}>
                {label}
              </span>
              {sublabel && (
                <span className="block truncate text-[10px] text-muted-foreground">
                  {sublabel}
                </span>
              )}
            </span>
          </div>
        </TooltipTrigger>
        <TooltipContent side="right" className="max-w-[240px]">
          <p className="font-medium">{label}</p>
          <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
          {example && (
            <p className="mt-1 font-mono text-[10px] text-muted-foreground">{example}</p>
          )}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

function PaletteFooter() {
  return (
    <p className="flex items-start gap-1.5 px-1 pt-1 text-[10px] leading-snug text-muted-foreground">
      <Info className="mt-0.5 h-3 w-3 shrink-0" aria-hidden />
      <span>
        Draw an edge <span className="font-mono">A → B</span> to make{" "}
        <span className="font-mono">B</span> depend on{" "}
        <span className="font-mono">A</span> (A runs first).
      </span>
    </p>
  );
}

const ACCENT_BORDER: Record<string, string> = {
  sky: "border-sky-300/70 dark:border-sky-800/60",
  amber: "border-amber-300/70 dark:border-amber-800/60",
  emerald: "border-emerald-300/70 dark:border-emerald-800/60",
  rose: "border-rose-300/70 dark:border-rose-800/60",
  yellow: "border-yellow-300/70 dark:border-yellow-800/60",
  violet: "border-violet-300/70 dark:border-violet-800/60",
};

const ACCENT_BG: Record<string, string> = {
  sky: "bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-300",
  amber: "bg-amber-100 text-amber-700 dark:bg-amber-950/60 dark:text-amber-300",
  emerald: "bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300",
  rose: "bg-rose-100 text-rose-700 dark:bg-rose-950/60 dark:text-rose-300",
  yellow: "bg-yellow-100 text-yellow-700 dark:bg-yellow-950/60 dark:text-yellow-300",
  violet: "bg-violet-100 text-violet-700 dark:bg-violet-950/60 dark:text-violet-300",
};

const KIND_ACCENT: Record<number, string> = {
  1: "sky", // task
  2: "amber", // decision
  3: "yellow", // approval
  4: "violet", // parallel
  5: "rose", // recover
};

const KIND_LABEL: Record<number, string> = {
  1: "task",
  2: "feature",
  3: "subtask",
  4: "epic",
};

const STATUS_LABEL: Record<number, string> = {
  1: "pending",
  2: "ready",
  3: "assigned",
  4: "running",
  6: "succeeded",
  7: "failed",
};
