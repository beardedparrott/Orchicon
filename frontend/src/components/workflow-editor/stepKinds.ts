// Step kinds + step payload types shared by the workflow editor and the
// workflow run view. The five kinds mirror docs/02 §2.4:
//   1 task      — dispatches a Worker via the TaskReconciler
//   2 decision  — evaluates a Rego query, routes the next step
//   3 approval  — blocks until a human approves
//   4 parallel  — fan-out: all downstream branches run concurrently
//   5 recover   — on failure, triggers the recovery workflow engine
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
} as const;

export const STEP_KIND_LABELS: Record<number, string> = {
  1: "task",
  2: "decision",
  3: "approval",
  4: "parallel",
  5: "recover",
};

export const STEP_KIND_TO_ENUM: Record<number, StepKind> = {
  1: StepKind.TASK,
  2: StepKind.DECISION,
  3: StepKind.APPROVAL,
  4: StepKind.PARALLEL,
  5: StepKind.RECOVER,
};

// `kindStrToNum` parses a StepWire.kind back into the numeric enum used
// by the editor. Unknown / future kinds default to "task".
export const STR_TO_KIND: Record<string, number> = {
  task: 1,
  decision: 2,
  approval: 3,
  parallel: 4,
  recover: 5,
};

export const KIND_TO_STR = (k: number): string => STEP_KIND_LABELS[k] ?? "task";

// Lucide icon per kind. The component never imports these directly — the
// palette uses them and the StepNode uses them so a single import is
// memoized at module scope.
export const STEP_KIND_ICONS: Record<number, LucideIcon> = {
  1: Bot,
  2: GitBranch,
  3: ShieldCheck,
  4: GitFork,
  5: LifeBuoy,
};

export const WORKER_ICON: LucideIcon = Bot;
export const WORKITEM_ICON: LucideIcon = FileText;
export const POLICY_ICON: LucideIcon = ShieldCheck;

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
// Exactly one of `workerId`, `workItemId`, or `policyId` is set, in
// addition to `kind`. `kind` is the StepKind enum value (1-5) — task
// steps are created from worker and work item drops; the step primitive
// tiles (decision/approval/parallel/recover) leave the ref fields empty.
export interface PaletteDropPayload {
  kind: number;
  name?: string;
  ref?: string;
  workerId?: string;
  workItemId?: string;
  policyId?: string;
}

// The dataTransfer mime key the palette uses. Namespaced with
// `application/x-orchicon-` so it never collides with browser or other
// library types (we previously used `application/x-workflow-step`, which
// works but is not uniquely identifying).
export const PALETTE_MIME = "application/x-orchicon-workflow-step";
