import {
  Bot,
  FileText,
  GitBranch,
  GitFork,
  LifeBuoy,
  Repeat2,
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
  POLICY: 8,
  LOOP_DECISION: 9,
} as const;

// Wire-format kind strings — must match Go domain constants exactly.
// These are what gets stored in workflow_versions.steps JSON and read
// by the WorkflowReconciler's switch statement on the backend.
export const STEP_KIND_WIRE: Record<number, string> = {
  [STEP_KIND.TASK]: "task",
  [STEP_KIND.DECISION]: "decision",
  [STEP_KIND.APPROVAL]: "approval",
  [STEP_KIND.PARALLEL]: "parallel",
  [STEP_KIND.RECOVER]: "recover",
  [STEP_KIND.WORK_ITEM]: "work_item",
  [STEP_KIND.PROJECT]: "project",
  [STEP_KIND.POLICY]: "policy",
  [STEP_KIND.LOOP_DECISION]: "loop_decision",
};

// Display labels (capitalized, user-facing).
export const STEP_KIND_DISPLAY_LABELS: Record<number, string> = {
  1: "Worker",
  2: "Conditional",
  3: "Approval",
  4: "Parallel",
  5: "Recovery",
  6: "Work Item",
  7: "Project",
  8: "Policy",
  9: "Loop Decision",
};

// Backward-compatible short labels for the run view and legacy use.
export const STEP_KIND_LABELS: Record<number, string> = {
  1: "worker",
  2: "conditional",
  3: "approval",
  4: "parallel",
  5: "recover",
  6: "work_item",
  7: "project",
  8: "policy",
  9: "loop_decision",
};

export const STEP_KIND_TO_ENUM: Record<number, StepKind> = {
  1: StepKind.TASK,
  2: StepKind.DECISION,
  3: StepKind.APPROVAL,
  4: StepKind.PARALLEL,
  5: StepKind.RECOVER,
  6: StepKind.WORK_ITEM,
  7: StepKind.PROJECT,
  9: StepKind.LOOP_DECISION,
};

export const STR_TO_KIND: Record<string, number> = {
  task: 1,
  decision: 2,
  approval: 3,
  parallel: 4,
  recover: 5,
  work_item: 6,
  project: 7,
  policy: 8,
  loop_decision: 9,
};

export const KIND_TO_STR = (k: number): string => STEP_KIND_WIRE[k] ?? "task";

export const STEP_KIND_ICONS: Record<number, LucideIcon> = {
  1: Bot,
  2: GitBranch,
  3: ShieldCheck,
  4: GitFork,
  5: LifeBuoy,
  6: FileText,
  7: FileText,
  8: ShieldCheck,
  9: Repeat2,
};

export const KIND_ACCENT: Record<number, string> = {
  [STEP_KIND.TASK]: "sky",
  [STEP_KIND.DECISION]: "amber",
  [STEP_KIND.APPROVAL]: "yellow",
  [STEP_KIND.PARALLEL]: "violet",
  [STEP_KIND.RECOVER]: "rose",
  [STEP_KIND.WORK_ITEM]: "emerald",
  [STEP_KIND.PROJECT]: "indigo",
  [STEP_KIND.POLICY]: "amber",
  [STEP_KIND.LOOP_DECISION]: "cyan",
};

export const ACCENT_STROKE: Record<string, string> = {
  sky: "stroke-sky-400",
  amber: "stroke-amber-400",
  yellow: "stroke-yellow-500",
  violet: "stroke-violet-400",
  rose: "stroke-rose-400",
  emerald: "stroke-emerald-400",
  indigo: "stroke-indigo-400",
  cyan: "stroke-cyan-400",
};

export const RECOVERY_STRATEGY_LABELS: Record<string, string> = {
  summarize_restart: "Summarize + restart",
  stop: "Stop",
  human_escalation: "Human escalation",
  retry_n: "Retry N",
};

export const RECOVERY_STRATEGY_OPTIONS = [
  { value: "summarize_restart", label: "Summarize + restart", summary: "Default 6-step flow (capture → summarize → preserve → review → plan → resume)." },
  { value: "stop", label: "Stop", summary: "Abandon the workflow cleanly." },
  { value: "human_escalation", label: "Human escalation", summary: "Block at L3 until a human approves." },
  { value: "retry_n", label: "Retry N", summary: "Requeue immediately, bypass capture/summarize." },
] as const;

export interface StepData {
  kind: number;
  name: string;
  ref: string;
  workerVersion: number;
  gatePolicyRef: string;
  config: string;
}

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
  edge_handles?: Record<string, { sourceHandle?: string; targetHandle?: string }>;
}

export interface PaletteDropPayload {
  kind: number;
  name?: string;
}

export const PALETTE_MIME = "application/x-orchicon-workflow-step";

export const WORKER_ICON: LucideIcon = Bot;
export const WORKITEM_ICON: LucideIcon = FileText;
export const PROJECT_ICON: LucideIcon = FileText;
export const POLICY_ICON: LucideIcon = ShieldCheck;
