// Step kinds + step payload types shared by the workflow editor and the
// workflow run view. The seven kinds mirror docs/02 §2.4 and the PR A
// "work item as the unit" model:
//   1 task      — a worker node. Processes the work item(s) connected
//                 to its input edge; captures the output summary at
//                 its output edge.
//   2 decision  — evaluates a Rego query, routes the next step
//   3 approval  — blocks until a human approves
//   4 parallel  — fan-out: all downstream branches run concurrently
//   5 recover   — on failure, triggers the recovery workflow engine
//   6 work_item — a passive marker for a work item. Holds the work
//                 item's metadata as context for the downstream worker.
//   7 project   — a passive marker for the project that scopes the
//                 downstream work items. Sets workflow.project_id.
//
// StepData is the shape stored on every React Flow node in the editor's
// canvas. It is serialized into StepWire (workflow_versions.steps JSON)
// on Save and back into StepData on Load.

import {
  Bot,
  FileText,
  GitBranch,
  GitFork,
  LifeBuoy,
  ShieldCheck,
  type LucideIcon,
} from "lucide-react";

import { StepKind } from "@/api/gen/orchicon/api/v1/workflow_pb";

export const STEP_KIND = {
  TASK: 1,
  DECISION: 2,
  APPROVAL: 3,
  PARALLEL: 4,
  RECOVER: 5,
  WORK_ITEM: 6,
  PROJECT: 7,
} as const;

export const STEP_KIND_LABELS: Record<number, string> = {
  1: "task",
  2: "decision",
  3: "approval",
  4: "parallel",
  5: "recover",
  6: "work_item",
  7: "project",
};

export const STEP_KIND_TO_ENUM: Record<number, StepKind> = {
  1: StepKind.TASK,
  2: StepKind.DECISION,
  3: StepKind.APPROVAL,
  4: StepKind.PARALLEL,
  5: StepKind.RECOVER,
  6: StepKind.WORK_ITEM,
  7: StepKind.PROJECT,
};

// `kindStrToNum` parses a StepWire.kind back into the numeric enum used
// by the editor. Unknown / future kinds default to "task".
export const STR_TO_KIND: Record<string, number> = {
  task: 1,
  decision: 2,
  approval: 3,
  parallel: 4,
  recover: 5,
  work_item: 6,
  project: 7,
};

export const KIND_TO_STR = (k: number): string => STEP_KIND_LABELS[k] ?? "task";

// Lucide icon per kind. The component never imports these directly — the
// palette uses them and the StepNode uses them so a single import is
// memoized at module scope.
export const STEP_KIND_ICONS: Record<number, LucideIcon> = {
  1: Bot, // task / worker
  2: GitBranch, // decision
  3: ShieldCheck, // approval
  4: GitFork, // parallel
  5: LifeBuoy, // recover
  6: FileText, // work item
  7: FileText, // project (same icon family as work item; palette labels distinguish)
};

// Accent name per step kind, shared by palette tiles, handle colors,
// and edge stroke colors.
export const KIND_ACCENT: Record<number, string> = {
  [STEP_KIND.TASK]: "sky",
  [STEP_KIND.DECISION]: "amber",
  [STEP_KIND.APPROVAL]: "yellow",
  [STEP_KIND.PARALLEL]: "violet",
  [STEP_KIND.RECOVER]: "rose",
  [STEP_KIND.WORK_ITEM]: "emerald",
  [STEP_KIND.PROJECT]: "indigo",
};

// Tailwind stroke classes per accent name, used to color edges by source
// step kind so the workflow flow is visually apparent.
export const ACCENT_STROKE: Record<string, string> = {
  sky: "stroke-sky-400",
  amber: "stroke-amber-400",
  yellow: "stroke-yellow-500",
  violet: "stroke-violet-400",
  rose: "stroke-rose-400",
  emerald: "stroke-emerald-400",
  indigo: "stroke-indigo-400",
};

export const WORKER_ICON: LucideIcon = Bot;
export const WORKITEM_ICON: LucideIcon = FileText;
export const PROJECT_ICON: LucideIcon = FileText;
export const POLICY_ICON: LucideIcon = ShieldCheck;

// Human-readable labels for recovery strategies tracked in config.strategy.
// Used by the StepNode and PropertiesPanel to show what type of recovery
// action the step will perform.
export const RECOVERY_STRATEGY_LABELS: Record<string, string> = {
  summarize_restart: "Summarize + restart",
  stop: "Stop",
  human_escalation: "Human escalation",
  retry_n: "Retry N",
};

// StepData is the per-node payload kept in React Flow's nodes state.
// Fields are persisted into StepWire (workflow_versions.steps JSON) by
// canvasToSteps. Unknown fields flow through `config` as JSON.
export interface StepData {
  kind: number;
  name: string;
  ref: string;
  workerVersion: number;
  gatePolicyRef: string;
  config: string;
}

// StepWire is the wire shape stored in workflow_versions.steps (docs/09
// §10). The editor's canvas serializes back and forth between StepData
// and StepWire on save/load.
export interface StepWire {
  id: string;
  name: string;
  kind: string;
  ref: string;
  worker_version: number;
  depends_on: string[];
  gate_policy_ref: string;
  config: string;
  position_x: number;
  position_y: number;
}

// PaletteDropPayload is what the palette writes into the dataTransfer on
// dragstart. The editor's onDrop reads it back, converts to a StepData
// node, and adds it to the canvas.
//
// Exactly one of `workerId`, `workItemId`, `projectId`, or `policyId`
// is set, in addition to `kind`. `kind` is the StepKind enum value
// (1-7). The step primitives (decision/approval/parallel/recover)
// leave the ref fields empty.
//
// `recoveryStrategy` (PR D) is set by the recovery palette tiles and
// stored in the step's config.strategy. The recovery engine reads
// this on dispatch to choose between the 4 strategies.
export interface PaletteDropPayload {
  kind: number;
  name?: string;
  ref?: string;
  workerId?: string;
  workItemId?: string;
  projectId?: string;
  policyId?: string;
  recoveryStrategy?: string;
}

// The dataTransfer mime key the palette uses. Namespaced with
// `application/x-orchicon-` so it never collides with browser or other
// library types (we previously used `application/x-workflow-step`, which
// works but is not uniquely identifying).
export const PALETTE_MIME = "application/x-orchicon-workflow-step";
