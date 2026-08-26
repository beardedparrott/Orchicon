import { describe, it, expect } from "vitest";
import {
  formatRelativeTime,
  auditEventToNotification,
  mapKind,
  buildHref,
  buildTitle,
  workflowRunToNotification,
  executionToNotification,
  recurringItemToNotification,
  mergeNotifications,
} from "./useNotifications";
import { Timestamp } from "@bufbuild/protobuf";
import { WorkflowRunStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { ExecutionStatus } from "@/api/gen/orchicon/api/v1/execution_pb";

function ts(secAgo: number): Timestamp {
  const sec = BigInt(Math.floor(Date.now() / 1000) - secAgo);
  return new Timestamp({ seconds: sec, nanos: 0 });
}

function mkEvent(overrides: Partial<{ id: string; action: string; targetType: string; targetId: string; after: string; occurredAt: Timestamp }>) {
  return {
    id: overrides.id ?? "ev1",
    action: overrides.action ?? "project.created",
    targetType: overrides.targetType ?? "project",
    targetId: overrides.targetId ?? "proj1",
    after: overrides.after ?? "{}",
    occurredAt: overrides.occurredAt ?? ts(10),
  } as unknown as Parameters<typeof auditEventToNotification>[0];
}

describe("formatRelativeTime", () => {
  it("returns Now for <60s", () => {
    const d = new Date(Date.now() - 10_000);
    expect(formatRelativeTime(d)).toBe("Now");
  });
  it("returns Xm ago", () => {
    const d = new Date(Date.now() - 5 * 60_000);
    expect(formatRelativeTime(d)).toBe("5m ago");
  });
  it("returns Xh ago", () => {
    const d = new Date(Date.now() - 2 * 3600_000);
    expect(formatRelativeTime(d)).toBe("2h ago");
  });
  it("returns Xd ago", () => {
    const d = new Date(Date.now() - 3 * 86400_000);
    expect(formatRelativeTime(d)).toBe("3d ago");
  });
  it("returns future as Now", () => {
    const d = new Date(Date.now() + 10_000);
    expect(formatRelativeTime(d)).toBe("Now");
  });
});

describe("mapKind precise", () => {
  it("workflow.run_started -> kicked", () => {
    expect(mapKind(mkEvent({ action: "workflow.run_started", targetType: "workflow_run" }))).toBe("workflow.kicked");
  });
  it("workflow.run_completed/run_failed/run_aborted -> finished", () => {
    expect(mapKind(mkEvent({ action: "workflow.run_completed", targetType: "workflow" }))).toBe("workflow.finished");
    expect(mapKind(mkEvent({ action: "workflow.run_failed", targetType: "workflow" }))).toBe("workflow.finished");
    expect(mapKind(mkEvent({ action: "workflow.run_aborted", targetType: "workflow" }))).toBe("workflow.finished");
  });
  it("workflow.run_retried / step_retried / force_progressed -> generic", () => {
    expect(mapKind(mkEvent({ action: "workflow.run_retried", targetType: "workflow" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "workflow.step_retried", targetType: "workflow" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "workflow.run_force_progressed", targetType: "workflow" }))).toBe("generic");
  });
  it("workflow CRUD -> generic", () => {
    expect(mapKind(mkEvent({ action: "workflow.created", targetType: "workflow" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "workflow.deleted", targetType: "workflow" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "workflow.published", targetType: "workflow" }))).toBe("generic");
  });
  it("execution terminal -> succeeded/failed, paused/resumed -> generic", () => {
    expect(mapKind(mkEvent({ action: "execution.succeeded", targetType: "execution" }))).toBe("execution.succeeded");
    expect(mapKind(mkEvent({ action: "execution.failed", targetType: "execution" }))).toBe("execution.failed");
    expect(mapKind(mkEvent({ action: "execution.failed_to_start", targetType: "execution" }))).toBe("execution.failed");
    expect(mapKind(mkEvent({ action: "execution.cancelled", targetType: "execution" }))).toBe("execution.failed");
    expect(mapKind(mkEvent({ action: "execution.terminated", targetType: "execution" }))).toBe("execution.failed");
    expect(mapKind(mkEvent({ action: "execution.paused", targetType: "execution" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "execution.resumed", targetType: "execution" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "execution.checkpointed", targetType: "execution" }))).toBe("generic");
    expect(mapKind(mkEvent({ action: "execution.dispatching", targetType: "execution" }))).toBe("generic");
  });
  it("recovery.triggered and recovery target", () => {
    expect(mapKind(mkEvent({ action: "recovery.triggered", targetType: "recovery" }))).toBe("recovery.triggered");
    expect(mapKind(mkEvent({ action: "recovery.task_marked_succeeded", targetType: "recovery" }))).toBe("recovery.triggered");
    expect(mapKind(mkEvent({ action: "work_item.updated", targetType: "recovery", targetId: "r1" }))).toBe("recovery.triggered");
  });
  it("approval created/requires_action", () => {
    expect(mapKind(mkEvent({ action: "approval.step_approved", targetType: "approval" }))).toBe("approval.created");
    expect(mapKind(mkEvent({ action: "approval.step_rejected", targetType: "approval" }))).toBe("approval.created");
    expect(mapKind(mkEvent({ action: "work_item.updated", targetType: "approval" }))).toBe("approval.requires_action");
  });
  it("schedule/recurring -> schedule.started", () => {
    expect(mapKind(mkEvent({ action: "schedule.started", targetType: "schedule" }))).toBe("schedule.started");
    expect(mapKind(mkEvent({ action: "work_item.created", targetType: "schedule" }))).toBe("schedule.started");
    expect(mapKind(mkEvent({ action: "work_item.recurring_created", targetType: "work_item" }))).toBe("schedule.started");
    expect(mapKind(mkEvent({ action: "work_item.started", targetType: "work_item" }))).toBe("schedule.started");
    expect(mapKind(mkEvent({ action: "work_item.sequence_started", targetType: "work_item" }))).toBe("schedule.started");
  });
  it("unknown -> generic", () => {
    expect(mapKind(mkEvent({ action: "project.created", targetType: "project" }))).toBe("generic");
  });
});

describe("buildHref route-aware", () => {
  it("workflow.kicked -> /workflows/:id", () => {
    const ev = mkEvent({ action: "workflow.run_started", targetType: "workflow", targetId: "wf123" });
    expect(buildHref(ev, "workflow.kicked")).toBe("/workflows/wf123");
  });
  it("execution.succeeded -> /executions/:id", () => {
    const ev = mkEvent({ action: "execution.succeeded", targetType: "execution", targetId: "ex1" });
    expect(buildHref(ev, "execution.succeeded")).toBe("/executions/ex1");
  });
  it("recovery.triggered -> /recovery/:id", () => {
    const ev = mkEvent({ action: "recovery.triggered", targetType: "recovery", targetId: "rec1" });
    expect(buildHref(ev, "recovery.triggered")).toBe("/recovery/rec1");
  });
  it("schedule.started with id -> /recurring-items/:id", () => {
    const ev = mkEvent({ action: "work_item.started", targetType: "work_item", targetId: "wi1" });
    expect(buildHref(ev, "schedule.started")).toBe("/recurring-items/wi1");
  });
  it("schedule.started without id -> /recurring-items", () => {
    const ev = mkEvent({ action: "schedule.started", targetType: "schedule", targetId: "" });
    expect(buildHref(ev, "schedule.started")).toBe("/recurring-items");
  });
  it("generic fallback maps known types correctly", () => {
    expect(buildHref(mkEvent({ action: "project.created", targetType: "project", targetId: "p1" }), "generic")).toBe("/projects/p1");
    expect(buildHref(mkEvent({ action: "work_item.updated", targetType: "work_item", targetId: "wi1" }), "generic")).toBe("/work-items/wi1");
    expect(buildHref(mkEvent({ action: "worker.created", targetType: "worker", targetId: "w1" }), "generic")).toBe("/workers/w1");
    expect(buildHref(mkEvent({ action: "policy.updated", targetType: "policy", targetId: "pol1" }), "generic")).toBe("/policies/pol1");
    expect(buildHref(mkEvent({ action: "runtime_image.created", targetType: "runtime_image", targetId: "ri1" }), "generic")).toBe("/runtime-images/ri1");
  });
  it("never emits /<targetType>/<id> for unregistered types -> /admin fallback", () => {
    expect(buildHref(mkEvent({ action: "foo.bar", targetType: "api_key", targetId: "ak1" }), "generic")).toBe("/admin");
    expect(buildHref(mkEvent({ action: "foo.bar", targetType: "identity", targetId: "id1" }), "generic")).toBe("/admin");
    expect(buildHref(mkEvent({ action: "foo.bar", targetType: "conversation", targetId: "c1" }), "generic")).toBe("/ask-orchicon");
    expect(buildHref(mkEvent({ action: "foo.bar", targetType: "settings", targetId: "" }), "generic")).toBe("/settings");
    expect(buildHref(mkEvent({ action: "foo.bar", targetType: "unknown_type", targetId: "x1" }), "generic")).toBe("/admin");
  });
});

describe("buildTitle", () => {
  it("includes after name suffix", () => {
    const ev = mkEvent({ action: "workflow.run_started", targetType: "workflow", targetId: "wf123", after: JSON.stringify({ name: "My Workflow" }) });
    expect(buildTitle(ev, "workflow.kicked")).toContain("My Workflow");
  });
});

describe("auditEventToNotification", () => {
  it("maps workflow.run_started to kicked kind", () => {
    const ev = mkEvent({ id: "01", action: "workflow.run_started", targetType: "workflow", targetId: "wf123", after: "{}", occurredAt: ts(10) });
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("workflow.kicked");
    expect(n.href).toBe("/workflows/wf123");
    expect(n.unread).toBe(true);
  });
  it("marks read when occurredAt <= lastReadAt", () => {
    const ev = mkEvent({ id: "02", action: "execution.succeeded", targetType: "execution", targetId: "ex1", after: "{}", occurredAt: ts(100) });
    const lastRead = new Date(Date.now() - 10 * 1000);
    const n = auditEventToNotification(ev, lastRead);
    expect(n.unread).toBe(false);
  });
  it("maps recovery.triggered", () => {
    const ev = mkEvent({ id: "03", action: "recovery.triggered", targetType: "recovery", targetId: "rec1", after: "{}", occurredAt: ts(5) });
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("recovery.triggered");
    expect(n.href).toBe("/recovery/rec1");
  });
  it("generic fallback", () => {
    const ev = mkEvent({ id: "04", action: "project.created", targetType: "project", targetId: "proj1", after: "{}", occurredAt: ts(20) });
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("generic");
  });
});

describe("workflowRunToNotification", () => {
  it("RUNNING -> workflow.kicked", () => {
    const n = workflowRunToNotification({ id: "r1", workflowId: "wf1", status: WorkflowRunStatus.RUNNING, updatedAt: new Date().toISOString() }, null);
    expect(n?.kind).toBe("workflow.kicked");
    expect(n?.id).toBe("syn:wr:r1");
    expect(n?.href).toBe("/workflows/wf1");
  });
  it("COMPLETED/FAILED/ABORTED -> workflow.finished", () => {
    expect(workflowRunToNotification({ id: "r2", workflowId: "wf2", status: WorkflowRunStatus.COMPLETED, updatedAt: new Date().toISOString() }, null)?.kind).toBe("workflow.finished");
    expect(workflowRunToNotification({ id: "r3", workflowId: "wf3", status: WorkflowRunStatus.FAILED, updatedAt: new Date().toISOString() }, null)?.kind).toBe("workflow.finished");
    expect(workflowRunToNotification({ id: "r4", workflowId: "wf4", status: WorkflowRunStatus.ABORTED, updatedAt: new Date().toISOString() }, null)?.kind).toBe("workflow.finished");
  });
  it("PENDING/other -> null", () => {
    expect(workflowRunToNotification({ id: "r5", workflowId: "wf5", status: WorkflowRunStatus.PENDING, updatedAt: new Date().toISOString() }, null)).toBeNull();
  });
});

describe("executionToNotification", () => {
  it("SUCCEEDED -> execution.succeeded", () => {
    const n = executionToNotification({ id: "e1", status: ExecutionStatus.SUCCEEDED, updatedAt: new Date().toISOString() }, null);
    expect(n?.kind).toBe("execution.succeeded");
    expect(n?.href).toBe("/executions/e1");
  });
  it("FAILED/FAILED_TO_START/TERMINATED -> execution.failed", () => {
    expect(executionToNotification({ id: "e2", status: ExecutionStatus.FAILED, updatedAt: new Date().toISOString() }, null)?.kind).toBe("execution.failed");
    expect(executionToNotification({ id: "e3", status: ExecutionStatus.FAILED_TO_START, updatedAt: new Date().toISOString() }, null)?.kind).toBe("execution.failed");
    expect(executionToNotification({ id: "e4", status: ExecutionStatus.TERMINATED, updatedAt: new Date().toISOString() }, null)?.kind).toBe("execution.failed");
  });
  it("RUNNING/DISPATCHING -> null", () => {
    expect(executionToNotification({ id: "e5", status: ExecutionStatus.RUNNING, updatedAt: new Date().toISOString() }, null)).toBeNull();
    expect(executionToNotification({ id: "e6", status: ExecutionStatus.DISPATCHING, updatedAt: new Date().toISOString() }, null)).toBeNull();
    expect(executionToNotification({ id: "e7", status: ExecutionStatus.UNSPECIFIED, updatedAt: new Date().toISOString() }, null)).toBeNull();
  });
});

describe("recurringItemToNotification", () => {
  it("recurring item -> schedule.started", () => {
    const item = {
      id: "wi1",
      title: "Daily sync",
      recurringSchedule: { frequency: "DAILY" },
      updatedAt: new Date().toISOString(),
    } as unknown as Parameters<typeof recurringItemToNotification>[0];
    const n = recurringItemToNotification(item, null);
    expect(n?.kind).toBe("schedule.started");
    expect(n?.href).toBe("/recurring-items/wi1");
    expect(n?.title).toContain("Daily sync");
    expect(n?.id).toBe("syn:rec:wi1");
  });
  it("non-recurring -> null", () => {
    const item = { id: "wi2", title: "Plain", updatedAt: new Date().toISOString() } as unknown as Parameters<typeof recurringItemToNotification>[0];
    expect(recurringItemToNotification(item, null)).toBeNull();
  });
  it("marks read correctly", () => {
    const item = {
      id: "wi3",
      title: "Weekly",
      recurringSchedule: { frequency: "WEEKLY" },
      updatedAt: new Date(Date.now() - 100_000).toISOString(),
    } as unknown as Parameters<typeof recurringItemToNotification>[0];
    const lastRead = new Date();
    expect(recurringItemToNotification(item, lastRead)?.unread).toBe(false);
  });
});

describe("mergeNotifications", () => {
  it("merges audit + workflow + execution + recurring, sorts newest first, dedups", () => {
    const auditEv = mkEvent({ id: "a1", action: "workflow.run_started", targetType: "workflow", targetId: "wf1", occurredAt: ts(60) });
    const runs = [{ id: "r1", workflowId: "wf99", status: WorkflowRunStatus.RUNNING, updatedAt: new Date().toISOString() }];
    const execs = [{ id: "e1", status: ExecutionStatus.SUCCEEDED, updatedAt: new Date(Date.now() - 5000).toISOString() }];
    const rec = [{ id: "wi-rec", title: "Rec", recurringSchedule: { frequency: "DAILY" }, updatedAt: new Date(Date.now() - 2000).toISOString() } as unknown as any];
    const merged = mergeNotifications([auditEv], runs, execs, rec, null);
    expect(merged.length).toBe(4);
    expect(merged[0].id).toBe("syn:wr:r1");
    const dupAudit = [auditEv, auditEv];
    const deduped = mergeNotifications(dupAudit, [], [], [], null);
    expect(deduped.length).toBe(1);
  });
  it("caps at 50", () => {
    const many = Array.from({ length: 60 }, (_, i) => mkEvent({ id: `id${i}`, action: "project.created", targetType: "project", targetId: `p${i}`, occurredAt: ts(i) }));
    const merged = mergeNotifications(many, [], [], [], null);
    expect(merged.length).toBe(50);
  });
});
