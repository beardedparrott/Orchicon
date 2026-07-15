// PropertiesPanel — inline editor for a single workflow step node.
//
// The selected node is shown on the right of the canvas. Editing a
// field patches the node's data via the `onChange` callback (the parent
// owns the canvas state). Fields shown depend on the step kind:
//   - all kinds: name, gate policy ref, config JSON
//   - task (1): worker ULID + worker version
//
// When no node is selected, the panel shows usage hints (drag, wire,
// click) instead of a form, so first-time users discover the canvas
// interactions.

import { useMemo } from "react";
import type { Node } from "reactflow";

import { Info } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

import { STEP_KIND_ICONS, STEP_KIND_LABELS, type StepData } from "./stepKinds";
import { useListPolicies } from "@/api/policies";

export function PropertiesPanel({
  node,
  onChange,
  readOnly,
}: {
  node: Node<StepData> | null;
  onChange: (patch: Partial<StepData>) => void;
  readOnly: boolean;
}) {
  if (!node) return <EmptyProperties />;
  const d = node.data;
  const Icon = STEP_KIND_ICONS[d.kind] ?? STEP_KIND_ICONS[1];
  const cfg = parseConfig(d.config);
  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center gap-2">
          <Icon className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-base">
            {STEP_KIND_LABELS[d.kind] ?? "step"} properties
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
        {d.kind === 1 && (
          <>
            <Field label="Worker ULID" hint="The worker that processes the upstream work item.">
              <Input
                value={d.ref}
                disabled={readOnly}
                placeholder="01H…"
                onChange={(e) => onChange({ ref: e.target.value })}
              />
            </Field>
            <Field label="Worker version" hint="0 = latest published version.">
              <Input
                type="number"
                value={d.workerVersion}
                disabled={readOnly}
                onChange={(e) => onChange({ workerVersion: Number(e.target.value) })}
              />
            </Field>
          </>
        )}
        {d.kind === 6 && (
          <Field
            label="Work item ID"
            hint="The work item that flows through this step. Drag the work item onto the canvas to bind."
          >
            <Input
              value={typeof cfg.work_item_id === "string" ? cfg.work_item_id : ""}
              disabled={readOnly}
              placeholder="01H…"
              onChange={(e) => {
                const next = { ...cfg, work_item_id: e.target.value };
                onChange({ config: JSON.stringify(next) });
              }}
            />
          </Field>
        )}
        {d.kind === 7 && (
          <Field
            label="Project ID"
            hint="The workflow run is bound to this project on first dispatch."
          >
            <Input
              value={typeof cfg.project_id === "string" ? cfg.project_id : d.ref}
              disabled={readOnly}
              placeholder="01H…"
              onChange={(e) => {
                const next = { ...cfg, project_id: e.target.value };
                onChange({ config: JSON.stringify(next) });
              }}
            />
          </Field>
        )}
        <Field
          label="Gate policy"
          hint={
            d.kind === 1
              ? "Evaluated before the worker runs. Empty = no gate."
              : "Decision steps branch on the policy's allow/deny result."
          }
        >
          <PolicyRefInput
            value={d.gatePolicyRef}
            disabled={readOnly}
            onChange={(v) => onChange({ gatePolicyRef: v })}
          />
        </Field>
        <Field
          label="Config (JSON)"
          hint="Step-specific config (work_item_id / project_id / policy_id are auto-set by drags above)."
        >
          <Textarea
            value={d.config}
            disabled={readOnly}
            rows={4}
            className="font-mono text-xs"
            onChange={(e) => onChange({ config: e.target.value })}
          />
        </Field>
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

// PolicyRefInput is a small autocomplete that lists published policies
// fetched via useListPolicies. The user can type a free-form ref (ULID)
// or pick from the dropdown. Selected policies show their name in the
// text input for readability.
function PolicyRefInput({
  value,
  disabled,
  onChange,
}: {
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const { data: policies } = useListPolicies({ status: 2 }); // 2 = PUBLISHED
  const items = useMemo(
    () =>
      (policies ?? [])
        .filter((p) => p.status === 2)
        .map((p) => ({ id: p.id, name: p.name })),
    [policies],
  );
  const matched = items.find((p) => p.id === value);
  return (
    <div className="space-y-1">
      <Input
        value={matched ? matched.name : value}
        disabled={disabled}
        placeholder="policy ULID (or pick below)"
        onChange={(e) => onChange(e.target.value)}
        className={cn(matched && "font-medium")}
        title={value}
      />
      {!disabled && items.length > 0 && (
        <div className="flex max-h-24 flex-wrap gap-1 overflow-y-auto rounded border bg-muted/40 p-1.5">
          {items.slice(0, 12).map((p) => (
            <button
              key={p.id}
              type="button"
              className={cn(
                "rounded px-2 py-0.5 text-[10px] font-medium transition-colors",
                p.id === value
                  ? "bg-primary text-primary-foreground"
                  : "bg-background hover:bg-accent",
              )}
              onClick={() => onChange(p.id)}
              title={p.id}
            >
              {p.name}
            </button>
          ))}
        </div>
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
          Drag <strong className="font-semibold text-foreground">Workers</strong>,{" "}
          <strong className="font-semibold text-foreground">Work items</strong>,{" "}
          <strong className="font-semibold text-foreground">Policies</strong>, or a step
          primitive from the palette onto the canvas.
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

// parseConfig defensively reads a step's config JSON. Returns {} for
// empty / malformed input. Used by the PropertiesPanel to render the
// per-kind inputs (work_item_id, project_id) without re-parsing JSON.
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
