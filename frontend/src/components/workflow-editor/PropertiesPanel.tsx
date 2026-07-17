import type { Node } from "reactflow";

import { Info } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { useListPolicies } from "@/api/policies";
import { useListProjects } from "@/api/projects";
import { useListWorkItems } from "@/api/workItems";
import { useListWorkers } from "@/api/workers";
import { WorkerStatus } from "@/api/gen/orchicon/api/v1/worker_pb";
import { PolicyStatus } from "@/api/gen/orchicon/api/v1/policy_pb";

import {
  RECOVERY_STRATEGY_OPTIONS,
  STEP_KIND,
  STEP_KIND_DISPLAY_LABELS,
  STEP_KIND_ICONS,
  type StepData,
} from "./stepKinds";

export function PropertiesPanel({
  node,
  onChange,
  readOnly,
  projectId,
}: {
  node: Node<StepData> | null;
  onChange: (patch: Partial<StepData>) => void;
  readOnly: boolean;
  projectId?: string;
}) {
  if (!node) return <EmptyProperties />;
  const d = node.data;
  const Icon = STEP_KIND_ICONS[d.kind] ?? STEP_KIND_ICONS[1];
  const cfg = parseConfig(d.config);

  const { data: projects } = useListProjects();
  const { data: workItems } = useListWorkItems(projectId || "", {});
  const { data: workers } = useListWorkers();
  const { data: policies } = useListPolicies({ status: PolicyStatus.PUBLISHED });

  const publishedWorkers = (workers ?? []).filter(
    (w) => w.status === WorkerStatus.PUBLISHED,
  );

  const updateNameAndConfig = (name: string, configPatch: Record<string, unknown>) => {
    const next = { ...cfg, ...configPatch };
    onChange({ name, config: JSON.stringify(next) });
  };

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center gap-2">
          <Icon className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-base">
            {STEP_KIND_DISPLAY_LABELS[d.kind] ?? "Step"} properties
          </CardTitle>
        </div>
        <CardDescription className="font-mono text-xs">{node.id}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <Field label="Name">
          <Input
            value={d.name}
            disabled={readOnly}
            placeholder="A short label for this step"
            onChange={(e) => onChange({ name: e.target.value })}
          />
        </Field>

        {d.kind === STEP_KIND.PROJECT && (
          <Field label="Project" hint="The project that scopes this workflow run.">
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={typeof cfg.project_id === "string" ? cfg.project_id : ""}
              disabled={readOnly}
              onChange={(e) => {
                const pid = e.target.value;
                const project = projects?.find((p) => p.id === pid);
                if (project) {
                  updateNameAndConfig(project.name, { project_id: pid, project_title: project.name });
                }
              }}
            >
              <option value="">-- Select a project --</option>
              {(projects ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </Field>
        )}

        {d.kind === STEP_KIND.WORK_ITEM && (
          <Field label="Work Item" hint="The work item that flows through this step.">
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={typeof cfg.work_item_id === "string" ? cfg.work_item_id : ""}
              disabled={readOnly || !projectId}
              onChange={(e) => {
                const wid = e.target.value;
                const wi = workItems?.find((w) => w.id === wid);
                if (wi) {
                  updateNameAndConfig(wi.title, {
                    work_item_id: wid,
                    work_item_title: wi.title,
                  });
                }
              }}
            >
              <option value="">-- Select a work item --</option>
              {(workItems ?? []).map((w) => (
                <option key={w.id} value={w.id}>
                  {w.title}
                </option>
              ))}
            </select>
            {!projectId && (
              <p className="text-[10px] text-amber-600 dark:text-amber-400">
                Assign a project to this workflow to see work items.
              </p>
            )}
          </Field>
        )}

        {d.kind === STEP_KIND.TASK && (
          <Field label="Worker" hint="The worker that processes the upstream work item.">
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={d.ref}
              disabled={readOnly}
              onChange={(e) => {
                const wid = e.target.value;
                const worker = publishedWorkers.find((w) => w.id === wid);
                if (worker) {
                  onChange({ name: worker.name, ref: wid, workerVersion: 0 });
                }
              }}
            >
              <option value="">-- Select a worker --</option>
              {publishedWorkers.map((w) => (
                <option key={w.id} value={w.id}>
                  {w.name}
                </option>
              ))}
            </select>
          </Field>
        )}

        {d.kind === STEP_KIND.POLICY && (
          <Field label="Policy" hint="The Rego policy evaluated as a gate for this step.">
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={d.gatePolicyRef}
              disabled={readOnly}
              onChange={(e) => {
                const pid = e.target.value;
                const policy = policies?.find((p) => p.id === pid);
                if (policy) {
                  const next = { ...cfg, policy_title: policy.name };
                  onChange({
                    name: policy.name,
                    gatePolicyRef: pid,
                    config: JSON.stringify(next),
                  });
                }
              }}
            >
              <option value="">-- Select a policy --</option>
              {(policies ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </Field>
        )}

        {d.kind === STEP_KIND.PARALLEL && (
          <Field label="Behavior" hint="All directly-connected downstream steps run concurrently. Steps two hops away still wait for their own dependency chain.">
            <p className="text-xs text-muted-foreground">
              Steps wired to this Parallel node&apos;s output will all be dispatched simultaneously.
            </p>
          </Field>
        )}

        {d.kind === STEP_KIND.RECOVER && (
          <Field label="Recovery strategy" hint="What happens when the upstream worker fails.">
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={typeof cfg.strategy === "string" ? cfg.strategy : "summarize_restart"}
              disabled={readOnly}
              onChange={(e) => {
                const strategy = e.target.value;
                const opt = RECOVERY_STRATEGY_OPTIONS.find((s) => s.value === strategy);
                if (opt) {
                  const next = { ...cfg, strategy };
                  onChange({ name: opt.label, config: JSON.stringify(next) });
                }
              }}
            >
              {RECOVERY_STRATEGY_OPTIONS.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </Field>
        )}

        {d.gatePolicyRef && d.kind !== STEP_KIND.POLICY && (
          <div className="rounded-md border bg-muted/40 p-2">
            <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
              Gate policy
            </div>
            <p className="font-mono text-[10px] text-foreground/80">
              {d.gatePolicyRef.slice(0, 16)}...
            </p>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <Label className="text-xs">{label}</Label>
      {children}
      {hint && (
        <p className="flex items-start gap-1 text-[10px] leading-snug text-muted-foreground">
          <Info className="mt-0.5 h-2.5 w-2.5 shrink-0" />
          {hint}
        </p>
      )}
    </div>
  );
}

function EmptyProperties() {
  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-base">Properties</CardTitle>
        <CardDescription>Select a step to edit its properties.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-2 text-xs text-muted-foreground">
        <p className="flex items-start gap-1.5">
          <span className="mt-0.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-sky-500" />
          Drag a connector tile from the palette onto the canvas.
        </p>
        <p className="flex items-start gap-1.5">
          <span className="mt-0.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
          Drag from a step&apos;s right edge to another step&apos;s left edge to make
          the second step depend on the first.
        </p>
        <p className="flex items-start gap-1.5">
          <span className="mt-0.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-violet-500" />
          Click a step to edit its properties, or press{" "}
          <kbd className="rounded border bg-muted px-1 font-mono text-[9px]">Del</kbd> to
          remove it.
        </p>
        <p className="flex items-start gap-1.5">
          <span className="mt-0.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
          <kbd className="rounded border bg-muted px-1 font-mono text-[9px]">
            Ctrl+Z
          </kbd>{" "}
          undo,{" "}
          <kbd className="rounded border bg-muted px-1 font-mono text-[9px]">
            Ctrl+Shift+Z
          </kbd>{" "}
          redo.
        </p>
      </CardContent>
    </Card>
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
