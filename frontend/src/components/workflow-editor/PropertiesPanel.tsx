import { useState } from "react";
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
import { WorkItemKind } from "@/api/gen/orchicon/api/v1/work_item_pb";

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
  const { data: projects } = useListProjects();
  const { data: workItems } = useListWorkItems(projectId || "", {});
  const { data: workers } = useListWorkers();
  const { data: policies } = useListPolicies({ status: PolicyStatus.PUBLISHED });

  if (!node) return <EmptyProperties />;
  const d = node.data;
  const Icon = STEP_KIND_ICONS[d.kind] ?? STEP_KIND_ICONS[1];
  const cfg = parseConfig(d.config);

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
          <WorkItemSelector
            projectId={projectId}
            currentWid={typeof cfg.work_item_id === "string" ? cfg.work_item_id : ""}
            disabled={readOnly}
            workItems={workItems ?? []}
            onSelect={(wi) => {
              updateNameAndConfig(wi.title, {
                work_item_id: wi.id,
                work_item_title: wi.title,
              });
            }}
          />
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

        {d.kind === STEP_KIND.APPROVAL && (
          <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
            <p className="font-medium">Approval gate</p>
            <p className="mt-1">
              This step blocks until a human approves. The approval wiring (who approves,
              notification channels, SLA, escalation) will be wired in a follow-up.
            </p>
          </div>
        )}

        {d.kind === STEP_KIND.PARALLEL && (
          <Field label="Behavior" hint="All directly-connected downstream steps run concurrently. Steps two hops away still wait for their own dependency chain.">
            <p className="text-xs text-muted-foreground">
              Steps wired to this Parallel node&apos;s output will all be dispatched simultaneously.
            </p>
          </Field>
        )}

        {d.kind === STEP_KIND.LOOP_DECISION && (
          <>
          <Field label="Max iterations" hint="How many times to loop back before failing the run. Once exhausted, recovery engages (invariant #8).">
            <input
              type="number"
              min={1}
              max={100}
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={typeof cfg.max_iterations === "number" ? cfg.max_iterations : 3}
              disabled={readOnly}
              onChange={(e) => {
                const maxIter = Math.max(1, Math.min(100, parseInt(e.target.value, 10) || 3));
                const next = { ...cfg, max_iterations: maxIter };
                onChange({ config: JSON.stringify(next) });
              }}
            />
          </Field>
          <Field label="Loop branch" hint="The step id to loop back to on failure. Create an edge from this node's loop outlet to the target step.">
            <p className="text-xs text-muted-foreground">
              {typeof cfg.loop_branch === "string" && cfg.loop_branch
                ? `Loop target: ${cfg.loop_branch}`
                : "Connect the loop outlet (bottom handle) to a topologically-prior step to set the loop branch."}
            </p>
          </Field>
          <Field label="Success branch" hint="The step id to continue to when the upstream succeeds.">
            <p className="text-xs text-muted-foreground">
              {typeof cfg.success_branch === "string" && cfg.success_branch
                ? `Success target: ${cfg.success_branch}`
                : "Connect the success outlet (right handle) to the next step."}
            </p>
          </Field>
          </>
        )}

        {d.kind === STEP_KIND.RECOVER && (
          <>
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
          {/* Retry config only applies to non-"stop" strategies.
              "Stop" means abandon cleanly — no retries needed. */}
          {(typeof cfg.strategy !== "string" || cfg.strategy !== "stop") && (
            <>
            <Field label="Max retries" hint="How many times to retry before escalating to human (L3).">
              <input
                type="number"
                min={1}
                max={100}
                className="h-9 w-full rounded-md border bg-background px-2 text-sm"
                value={typeof cfg.max_retries === "number" ? cfg.max_retries : 5}
                disabled={readOnly}
                onChange={(e) => {
                  const maxRetries = Math.max(1, Math.min(100, parseInt(e.target.value, 10) || 5));
                  const next = { ...cfg, max_retries: maxRetries };
                  onChange({ config: JSON.stringify(next) });
                }}
              />
            </Field>
            <Field label="Retry delay (seconds)" hint="Time to wait between retries.">
              <input
                type="number"
                min={0}
                max={3600}
                className="h-9 w-full rounded-md border bg-background px-2 text-sm"
                value={typeof cfg.retry_delay_seconds === "number" ? cfg.retry_delay_seconds : 10}
                disabled={readOnly}
                onChange={(e) => {
                  const delay = Math.max(0, Math.min(3600, parseInt(e.target.value, 10) || 10));
                  const next = { ...cfg, retry_delay_seconds: delay };
                  onChange({ config: JSON.stringify(next) });
                }}
              />
            </Field>
            </>
          )}
          </>
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

const WORK_ITEM_KIND_LABELS: Record<number, string> = {
  [WorkItemKind.EPIC]: "Epic",
  [WorkItemKind.FEATURE]: "Feature",
  [WorkItemKind.TASK]: "Task",
  [WorkItemKind.SUBTASK]: "Subtask",
};

const ANCESTOR_CHAIN: Record<number, number[]> = {
  [WorkItemKind.EPIC]: [],
  [WorkItemKind.FEATURE]: [WorkItemKind.EPIC],
  [WorkItemKind.TASK]: [WorkItemKind.EPIC, WorkItemKind.FEATURE],
  [WorkItemKind.SUBTASK]: [WorkItemKind.EPIC, WorkItemKind.FEATURE, WorkItemKind.TASK],
};

function WorkItemSelector({
  projectId,
  currentWid,
  disabled,
  workItems,
  onSelect,
}: {
  projectId?: string;
  currentWid: string;
  disabled: boolean;
  workItems: { id: string; title: string; kind: WorkItemKind; parentId?: string }[];
  onSelect: (wi: { id: string; title: string; kind: WorkItemKind }) => void;
}) {
  const [typeFilter, setTypeFilter] = useState<number | null>(null);
  const [selections, setSelections] = useState<Record<string, string>>({});

  const currentItem = workItems.find((w) => w.id === currentWid);

  function setLevel(kind: number, id: string) {
    const key = String(kind);
    setSelections((prev) => {
      const next = { ...prev, [key]: id };
      const chain = ANCESTOR_CHAIN[typeFilter ?? 0] ?? [];
      const idx = chain.indexOf(kind);
      if (idx >= 0) {
        for (let i = idx + 1; i < chain.length; i++) {
          delete next[String(chain[i])];
        }
      }
      return next;
    });
  }

  const ancestorKinds = typeFilter != null ? ANCESTOR_CHAIN[typeFilter] ?? [] : [];

  const topLevelTypes = [
    { value: null, label: "All types" },
    { value: WorkItemKind.EPIC, label: "Epic" },
    { value: WorkItemKind.FEATURE, label: "Feature" },
    { value: WorkItemKind.TASK, label: "Task" },
    { value: WorkItemKind.SUBTASK, label: "Subtask" },
  ];

  return (
    <div className="space-y-2">
      <Field label="Type" hint="Narrow down by work item type.">
        <select
          className="h-9 w-full rounded-md border bg-background px-2 text-sm"
          value={typeFilter ?? ""}
          disabled={disabled || !projectId}
          onChange={(e) => {
            setTypeFilter(e.target.value ? Number(e.target.value) : null);
            setSelections({});
          }}
        >
          {topLevelTypes.map((t) => (
            <option key={t.label} value={t.value ?? ""}>
              {t.label}
            </option>
          ))}
        </select>
      </Field>

      {ancestorKinds.map((ancestorKind) => {
        const selectedAncestorId = selections[String(ancestorKind)] ?? "";
        const items = workItems.filter((w) => w.kind === ancestorKind);
        return (
          <Field
            key={ancestorKind}
            label={WORK_ITEM_KIND_LABELS[ancestorKind] ?? "Parent"}
            hint={`Filter by ${WORK_ITEM_KIND_LABELS[ancestorKind]?.toLowerCase() ?? "parent"}.`}
          >
            <select
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
              value={selectedAncestorId}
              disabled={disabled || !projectId}
              onChange={(e) => setLevel(ancestorKind, e.target.value)}
            >
              <option value="">All {WORK_ITEM_KIND_LABELS[ancestorKind]?.toLowerCase() ?? "items"}</option>
              {items.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.title}
                </option>
              ))}
            </select>
          </Field>
        );
      })}

      <Field label="Work Item" hint="The work item that flows through this step.">
        <select
          className="h-9 w-full rounded-md border bg-background px-2 text-sm"
          value={currentWid}
          disabled={disabled || !projectId}
          onChange={(e) => {
            const wi = workItems.find((w) => w.id === e.target.value);
            if (wi) onSelect(wi);
          }}
        >
          <option value="">-- Select a work item --</option>
          {workItems
            .filter((w) => {
              if (typeFilter != null && w.kind !== typeFilter) return false;
              // Apply all ancestor filters — an item matches if its parentId
              // chain leads to the selected ancestor (or ancestor is unset).
              for (const ak of ancestorKinds) {
                const sel = selections[String(ak)];
                if (!sel) continue;
                // Walk up the parent chain to check if this item descends from sel.
                let cur: (typeof workItems)[number] | undefined = w;
                let found = false;
                while (cur) {
                  if (cur.id === sel) { found = true; break; }
                  cur = cur.parentId ? workItems.find((x) => x.id === cur!.parentId) : undefined;
                }
                if (!found) return false;
              }
              return true;
            })
            .map((w) => (
              <option key={w.id} value={w.id}>
                {WORK_ITEM_KIND_LABELS[w.kind] ?? "?"}: {w.title}
              </option>
            ))}
        </select>
        {!projectId && (
          <p className="text-[10px] text-amber-600 dark:text-amber-400">
            Assign a project to this workflow to see work items.
          </p>
        )}
      </Field>

      {currentItem && currentItem.parentId && (
        <p className="text-[10px] text-muted-foreground">
          Child of {workItems.find((w) => w.id === currentItem.parentId)?.title ?? currentItem.parentId.slice(0, 12)}
        </p>
      )}
    </div>
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
