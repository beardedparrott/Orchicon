import { useRef, useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  FileText,
  GitFork,
  Info,
  LifeBuoy,
  type LucideIcon,
} from "lucide-react";

import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

import {
  PALETTE_MIME,
  POLICY_ICON,
  PROJECT_ICON,
  STEP_KIND,
  STEP_KIND_ICONS,
  WORKER_ICON,
  WORKITEM_ICON,
  type PaletteDropPayload,
} from "./stepKinds";

export function Palette({ readOnly }: { readOnly: boolean }) {
  return (
    <div className="space-y-3">
      <Section
        title="Project"
        icon={PROJECT_ICON}
        subtitle="Bind the run to a project"
      >
        <DraggableTile
          label="Project"
          sublabel="Assign a project on the right"
          icon={PROJECT_ICON}
          kindAccent="indigo"
          payload={{ kind: STEP_KIND.PROJECT, name: "Project" }}
          description="Binds the workflow run to a project. Select the project in the properties panel."
          readOnly={readOnly}
        />
      </Section>
      <Section
        title="Work Item"
        icon={WORKITEM_ICON}
        subtitle="Reference a work item"
      >
        <DraggableTile
          label="Work Item"
          sublabel="Pick a work item on the right"
          icon={FileText}
          kindAccent="emerald"
          payload={{ kind: STEP_KIND.WORK_ITEM, name: "Work Item" }}
          description="A passive marker for a work item. The connected worker step processes this work item."
          readOnly={readOnly}
        />
      </Section>
      <Section
        title="Worker"
        icon={WORKER_ICON}
        subtitle="The actor that processes work"
      >
        <DraggableTile
          label="Worker"
          sublabel="Pick a worker on the right"
          icon={WORKER_ICON}
          kindAccent="sky"
          payload={{ kind: STEP_KIND.TASK, name: "Worker" }}
          description="A worker that processes the upstream work item."
          readOnly={readOnly}
        />
      </Section>
      <Section
        title="Policy"
        icon={POLICY_ICON}
        subtitle="Attach a Rego gate rule"
      >
        <DraggableTile
          label="Policy"
          sublabel="Pick a policy on the right"
          icon={POLICY_ICON}
          kindAccent="amber"
          payload={{ kind: STEP_KIND.POLICY, name: "Policy" }}
          description="A policy gate evaluated before the step runs. Select the policy in the properties panel."
          readOnly={readOnly}
        />
      </Section>
      <Section
        title="Parallel"
        icon={GitFork}
        subtitle="Fan out to multiple downstream steps"
      >
        <DraggableTile
          label="Parallel"
          sublabel="Runs all direct downstream steps concurrently"
          icon={GitFork}
          kindAccent="violet"
          payload={{ kind: STEP_KIND.PARALLEL, name: "Parallel" }}
          description="Fans out to every directly-connected downstream step, running them concurrently. Steps further downstream still wait for their own dependencies."
          readOnly={readOnly}
        />
      </Section>
      <Section
        title="Recovery"
        icon={LifeBuoy}
        subtitle="What to do when a worker fails"
      >
        <DraggableTile
          label="Recovery"
          sublabel="Set strategy in properties"
          icon={STEP_KIND_ICONS[STEP_KIND.RECOVER]}
          kindAccent="rose"
          payload={{ kind: STEP_KIND.RECOVER, name: "Recovery" }}
          description="Triggers recovery on upstream failure. Choose the strategy in the properties panel."
          readOnly={readOnly}
        />
      </Section>
      <PaletteFooter />
    </div>
  );
}

function Section({
  title,
  subtitle,
  icon: Icon,
  children,
  defaultOpen = true,
}: {
  title: string;
  subtitle?: string;
  icon: LucideIcon;
  children?: React.ReactNode;
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
          {children}
        </div>
      )}
    </div>
  );
}

function DraggableTile({
  label,
  sublabel,
  icon: Icon,
  kindAccent,
  payload,
  description,
  readOnly,
}: {
  label: string;
  sublabel?: string;
  icon: LucideIcon;
  kindAccent: string;
  payload: PaletteDropPayload;
  description: string;
  readOnly: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const setData = (e: React.DragEvent) => {
    e.dataTransfer.setData(PALETTE_MIME, JSON.stringify(payload));
    e.dataTransfer.setData("text/plain", label);
    e.dataTransfer.effectAllowed = "copyMove";
    if (ref.current) {
      const ghost = ref.current.cloneNode(true) as HTMLElement;
      ghost.style.position = "absolute";
      ghost.style.top = "-1000px";
      ghost.style.left = "-1000px";
      ghost.style.width = `${ref.current.offsetWidth}px`;
      ghost.style.opacity = "0.85";
      document.body.appendChild(ghost);
      e.dataTransfer.setDragImage(ghost, 12, 12);
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
  indigo: "border-indigo-300/70 dark:border-indigo-800/60",
};

const ACCENT_BG: Record<string, string> = {
  sky: "bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-300",
  amber: "bg-amber-100 text-amber-700 dark:bg-amber-950/60 dark:text-amber-300",
  emerald: "bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300",
  rose: "bg-rose-100 text-rose-700 dark:bg-rose-950/60 dark:text-rose-300",
  yellow: "bg-yellow-100 text-yellow-700 dark:bg-yellow-950/60 dark:text-yellow-300",
  violet: "bg-violet-100 text-violet-700 dark:bg-violet-950/60 dark:text-violet-300",
  indigo: "bg-indigo-100 text-indigo-700 dark:bg-indigo-950/60 dark:text-indigo-300",
};
