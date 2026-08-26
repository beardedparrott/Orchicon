import { useMemo, useCallback, useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSession } from "@/auth/auth";
import { authClient, executionClient, workflowClient, projectClient, workItemClient } from "@/api/clients";
import { useNotificationCursor } from "./useNotificationCursor";
import type { AuditEvent } from "@/api/gen/orchicon/api/v1/auth_service_pb";
import { WorkflowRunStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { ExecutionStatus } from "@/api/gen/orchicon/api/v1/execution_pb";
import { RecurringFilter } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";

export type NotificationKind =
  | "workflow.kicked"
  | "workflow.finished"
  | "schedule.started"
  | "execution.succeeded"
  | "execution.failed"
  | "recovery.triggered"
  | "approval.created"
  | "approval.requires_action"
  | "generic";

export type NotificationItem = {
  id: string;
  kind: NotificationKind;
  title: string;
  occurredAt: Date;
  href: string;
  unread: boolean;
  action: string;
  targetType: string;
  targetId: string;
  raw: AuditEvent | unknown;
};

export function formatRelativeTime(d: Date | string, nowMs = Date.now()): string {
  const ms = typeof d === "string" ? new Date(d).getTime() : d.getTime();
  if (Number.isNaN(ms)) return "";
  const diff = nowMs - ms;
  if (diff < 0) return "Now";
  if (diff < 60_000) return "Now";
  const minutes = Math.floor(diff / 60_000);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}d ago`;
  return new Date(ms).toLocaleDateString();
}

function humanizeAction(action: string): string {
  if (!action) return "Event";
  return action.replace(/[_\.]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

export function mapKind(event: AuditEvent): NotificationKind {
  const a = event.action;
  const t = event.targetType;
  if (a === "workflow.run_started") return "workflow.kicked";
  if (a === "workflow.run_aborted") return "workflow.finished";
  if (a === "workflow.run_completed" || a === "workflow.run_failed") return "workflow.finished";
  if (a === "workflow.step_retried" || a === "workflow.run_retried" || a === "workflow.run_force_progressed") return "generic";
  if (a.startsWith("workflow.run_")) return "generic";
  if (a === "workflow.version_deleted" || a === "workflow.deleted" || a === "workflow.created" || a === "workflow.published" || a === "workflow.updated" || a === "workflow.deprecated") return "generic";
  if (a.startsWith("workflow.")) return "generic";
  if (a === "execution.succeeded") return "execution.succeeded";
  if (a === "execution.failed" || a === "execution.failed_to_start") return "execution.failed";
  if (a === "execution.cancelled" || a === "execution.terminated") return "execution.failed";
  if (a === "execution.paused" || a === "execution.resumed" || a === "execution.checkpointed" || a === "execution.checkpoint" || a === "execution.ready" || a === "execution.dispatching" || a === "execution.running") return "generic";
  if (t === "execution" && a.startsWith("execution.")) {
    if (a.includes("succeeded")) return "execution.succeeded";
    if (a.includes("failed") || a.includes("cancelled") || a.includes("terminated")) return "execution.failed";
    return "generic";
  }
  if (a === "recovery.triggered" || a === "recovery.task_marked_succeeded" || a === "recovery.continuation_plan_approved") return "recovery.triggered";
  if (t === "recovery") return "recovery.triggered";
  if (a === "approval.step_approved" || a === "approval.step_rejected") return "approval.created";
  if (t === "approval") return "approval.requires_action";
  if (a.includes("schedule") || a.includes("recurring") || t.includes("schedule")) return "schedule.started";
  if (t === "work_item" && a === "work_item.started") return "schedule.started";
  if (t === "work_item" && a === "work_item.sequence_started") return "schedule.started";
  return "generic";
}

export function buildTitle(event: AuditEvent, kind: NotificationKind): string {
  const a = event.action;
  let afterName = "";
  try {
    const parsed = JSON.parse(event.after || "{}");
    afterName = parsed.name ?? parsed.title ?? parsed.display_name ?? "";
  } catch {}
  const suffix = afterName ? ` — ${afterName}` : "";
  switch (kind) {
    case "workflow.kicked":
      return `Workflow kicked off${suffix || (event.targetId ? ` — ${event.targetId.slice(0, 8)}` : "")}`;
    case "workflow.finished":
      if (a.includes("aborted") || a.includes("failed")) return `Workflow run ended${suffix}`;
      return `Workflow finished${suffix}`;
    case "schedule.started":
      return `Recurring schedule started${suffix}`;
    case "execution.succeeded":
      return `Execution succeeded${suffix}`;
    case "execution.failed":
      return `Execution failed${suffix}`;
    case "recovery.triggered":
      return `Recovery triggered${suffix}`;
    case "approval.requires_action":
      return `Approval requires action${suffix}`;
    case "approval.created":
      return `Approval ${a.includes("rejected") ? "rejected" : "approved"}${suffix}`;
    default:
      return humanizeAction(a) + suffix;
  }
}

export function buildHref(event: AuditEvent, kind: NotificationKind): string {
  const tid = event.targetId;
  const ttype = event.targetType;
  switch (kind) {
    case "workflow.kicked":
    case "workflow.finished":
      if ((ttype === "workflow" || ttype === "workflow_run") && tid) return `/workflows/${tid}`;
      return "/workflows";
    case "execution.succeeded":
    case "execution.failed":
      if (tid) return `/executions/${tid}`;
      return "/executions";
    case "recovery.triggered":
      if (tid) return `/recovery/${tid}`;
      return "/recovery";
    case "approval.created":
    case "approval.requires_action":
      return "/approvals";
    case "schedule.started":
      if (tid) return `/recurring-items/${tid}`;
      return "/recurring-items";
    default:
      if (ttype === "work_item" && tid) return `/work-items/${tid}`;
      if (ttype === "project" && tid) return `/projects/${tid}`;
      if ((ttype === "workflow" || ttype === "workflow_run") && tid) return `/workflows/${tid}`;
      if (ttype === "execution" && tid) return `/executions/${tid}`;
      if (ttype === "recovery" && tid) return `/recovery/${tid}`;
      if (ttype === "worker" && tid) return `/workers/${tid}`;
      if (ttype === "policy" && tid) return `/policies/${tid}`;
      if (ttype === "runtime_image" && tid) return `/runtime-images/${tid}`;
      if ((ttype === "webhook_subscription" || ttype === "webhook") && tid) return "/webhooks";
      if (ttype === "adapter" && tid) return "/adapters";
      if (ttype === "conversation" && tid) return "/ask-orchicon";
      if (ttype === "schedule" || ttype === "recurring_schedule") return "/schedules";
      if (ttype === "settings") return "/settings";
      if (["identity", "role", "role_binding", "api_key", "tenant", "audit"].includes(ttype)) return "/admin";
      return "/admin";
  }
}

export function toDate(ts: AuditEvent["occurredAt"]): Date {
  if (!ts) return new Date(0);
  const anyTs = ts as unknown as { seconds: bigint | number; nanos: number };
  const sec = Number(anyTs.seconds ?? 0);
  const nanos = Number(anyTs.nanos ?? 0);
  return new Date(sec * 1000 + nanos / 1_000_000);
}

export function toDateAny(ts: unknown): Date {
  if (!ts) return new Date(0);
  const anyTs = ts as { seconds?: bigint | number; nanos?: number };
  if (anyTs.seconds !== undefined) {
    const sec = Number(anyTs.seconds ?? 0);
    const nanos = Number(anyTs.nanos ?? 0);
    return new Date(sec * 1000 + nanos / 1_000_000);
  }
  const d = new Date(ts as string);
  return Number.isNaN(d.getTime()) ? new Date(0) : d;
}

export function auditEventToNotification(event: AuditEvent, lastReadAt: Date | null): NotificationItem {
  const kind = mapKind(event);
  const occurredAt = toDate(event.occurredAt);
  const unread = lastReadAt ? occurredAt.getTime() > lastReadAt.getTime() : true;
  return {
    id: event.id,
    kind,
    title: buildTitle(event, kind),
    occurredAt,
    href: buildHref(event, kind),
    unread,
    action: event.action,
    targetType: event.targetType,
    targetId: event.targetId,
    raw: event,
  };
}

export function workflowRunToNotification(run: any, lastReadAt: Date | null): NotificationItem | null {
  const status: number = run.status;
  let kind: NotificationKind | null = null;
  let title = "";
  if (status === WorkflowRunStatus.RUNNING) {
    kind = "workflow.kicked";
    title = `Workflow kicked off — ${String(run.workflowId).slice(0, 8)}`;
  } else if (status === WorkflowRunStatus.COMPLETED || status === WorkflowRunStatus.FAILED || status === WorkflowRunStatus.ABORTED) {
    kind = "workflow.finished";
    title = status === WorkflowRunStatus.FAILED ? "Workflow run failed" : status === WorkflowRunStatus.ABORTED ? "Workflow run aborted" : "Workflow finished";
    if (run.workflowId) title += ` — ${String(run.workflowId).slice(0, 8)}`;
  } else {
    return null;
  }
  const occurredAt = toDateAny(run.updatedAt ?? run.endedAt ?? run.startedAt ?? run.createdAt);
  const unread = lastReadAt ? occurredAt.getTime() > lastReadAt.getTime() : true;
  const href = run.workflowId ? `/workflows/${run.workflowId}` : "/workflows";
  return {
    id: `syn:wr:${run.id}`,
    kind,
    title,
    occurredAt,
    href,
    unread,
    action: status === WorkflowRunStatus.RUNNING ? "workflow.run_started" : status === WorkflowRunStatus.FAILED ? "workflow.run_failed" : status === WorkflowRunStatus.ABORTED ? "workflow.run_aborted" : "workflow.run_completed",
    targetType: "workflow",
    targetId: run.workflowId || run.id,
    raw: run,
  };
}

export function executionToNotification(exec: any, lastReadAt: Date | null): NotificationItem | null {
  const status: number = exec.status;
  let kind: NotificationKind | null = null;
  let title = "";
  let action = "";
  if (status === ExecutionStatus.SUCCEEDED) {
    kind = "execution.succeeded";
    title = "Execution succeeded";
    action = "execution.succeeded";
  } else if (status === ExecutionStatus.FAILED || status === ExecutionStatus.FAILED_TO_START) {
    kind = "execution.failed";
    title = "Execution failed";
    action = "execution.failed";
  } else if (status === ExecutionStatus.TERMINATED) {
    kind = "execution.failed";
    title = "Execution terminated";
    action = "execution.cancelled";
  } else {
    return null;
  }
  if (exec.id) title += ` — ${String(exec.id).slice(0, 8)}`;
  const occurredAt = toDateAny(exec.updatedAt ?? exec.endedAt ?? exec.createdAt);
  const unread = lastReadAt ? occurredAt.getTime() > lastReadAt.getTime() : true;
  return {
    id: `syn:exec:${exec.id}`,
    kind,
    title,
    occurredAt,
    href: `/executions/${exec.id}`,
    unread,
    action,
    targetType: "execution",
    targetId: exec.id,
    raw: exec,
  };
}

export function recurringItemToNotification(item: WorkItem, lastReadAt: Date | null): NotificationItem | null {
  const hasSchedule = Boolean((item as any).recurringSchedule ?? (item as any).recurring_schedule);
  if (!hasSchedule) return null;
  const occurredAt = toDateAny((item as any).updatedAt ?? (item as any).updated_at ?? (item as any).createdAt ?? (item as any).created_at);
  if (occurredAt.getTime() === 0) return null;
  const unread = lastReadAt ? occurredAt.getTime() > lastReadAt.getTime() : true;
  const title = item.title ? `Recurring schedule started — ${item.title}` : "Recurring schedule started";
  return {
    id: `syn:rec:${item.id}`,
    kind: "schedule.started",
    title,
    occurredAt,
    href: `/recurring-items/${item.id}`,
    unread,
    action: "work_item.recurring_created",
    targetType: "work_item",
    targetId: item.id,
    raw: item,
  };
}

export function mergeNotifications(
  auditEvents: AuditEvent[],
  workflowRuns: unknown[],
  executions: unknown[],
  recurringItems: WorkItem[],
  lastReadAt: Date | null,
): NotificationItem[] {
  const mapped = auditEvents.slice(0, 50).map((e) => auditEventToNotification(e, lastReadAt));
  const wrSynthetic: NotificationItem[] = [];
  for (const r of workflowRuns ?? []) {
    const n = workflowRunToNotification(r as any, lastReadAt);
    if (n) wrSynthetic.push(n);
  }
  const execSynthetic: NotificationItem[] = [];
  for (const ex of executions ?? []) {
    const n = executionToNotification(ex as any, lastReadAt);
    if (n) execSynthetic.push(n);
  }
  const recSynthetic: NotificationItem[] = [];
  for (const it of recurringItems ?? []) {
    const n = recurringItemToNotification(it as WorkItem, lastReadAt);
    if (n) recSynthetic.push(n);
  }
  const all = [...mapped, ...wrSynthetic, ...execSynthetic, ...recSynthetic];
  all.sort((a, b) => b.occurredAt.getTime() - a.occurredAt.getTime());
  const seen = new Set<string>();
  const deduped: NotificationItem[] = [];
  for (const it of all) {
    if (seen.has(it.id)) continue;
    seen.add(it.id);
    deduped.push(it);
    if (deduped.length >= 50) break;
  }
  return deduped;
}

export function useNotifications(opts?: { enabled?: boolean }) {
  const session = useSession();
  const enabled = (opts?.enabled ?? true) && !!session.authenticated;
  const { lastReadAt, markRead } = useNotificationCursor();

  const auditQuery = useQuery({
    queryKey: ["notifications", "audit", session.tenant_id ?? "", session.identity_id ?? ""],
    queryFn: async () => {
      const res = await authClient.listAuditEvents({ pageSize: 50 });
      return (res.events ?? []) as AuditEvent[];
    },
    enabled,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 10_000,
    retry: 1,
  });

  const workflowRunsQuery = useQuery({
    queryKey: ["notifications", "workflowRuns", session.tenant_id ?? "", session.identity_id ?? ""],
    queryFn: async () => {
      const res = await workflowClient.listWorkflowRuns({ pageSize: 50, sortBy: "updated_at", sortOrder: "desc" });
      return (res.runs ?? []) as unknown[];
    },
    enabled,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 10_000,
    retry: 1,
  });

  const executionsQuery = useQuery({
    queryKey: ["notifications", "executions", session.tenant_id ?? "", session.identity_id ?? ""],
    queryFn: async () => {
      const res = await executionClient.listExecutions({ pageSize: 50, sortBy: "created_at", sortOrder: "desc" });
      return (res.executions ?? []) as unknown[];
    },
    enabled,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 10_000,
    retry: 1,
  });

  const projectsQuery = useQuery({
    queryKey: ["notifications", "projects", session.tenant_id ?? ""],
    queryFn: async () => {
      const res = await projectClient.listProjects({ pageSize: 20 });
      return (res.projects ?? []) as { id: string }[];
    },
    enabled,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 20_000,
    retry: 1,
  });

  const recurringQuery = useQuery({
    queryKey: ["notifications", "recurring", session.tenant_id ?? "", (projectsQuery.data ?? []).map((p) => p.id).join(",")],
    queryFn: async () => {
      const projects = projectsQuery.data ?? [];
      if (projects.length === 0) return [] as WorkItem[];
      const all: WorkItem[] = [];
      await Promise.all(
        projects.map(async (p) => {
          try {
            const res = await workItemClient.listWorkItems({
              projectId: p.id,
              recurringFilter: RecurringFilter.ONLY_RECURRING,
              pageSize: 20,
              sortBy: "updated_at",
              sortOrder: "desc",
            });
            all.push(...((res.workItems ?? []) as WorkItem[]));
          } catch {}
        }),
      );
      all.sort((a: any, b: any) => {
        const at = (a.updatedAt ?? (a as any).updated_at ?? a.createdAt) as unknown;
        const bt = (b.updatedAt ?? (b as any).updated_at ?? b.createdAt) as unknown;
        return toDateAny(bt).getTime() - toDateAny(at).getTime();
      });
      return all.slice(0, 20);
    },
    enabled: enabled && !!projectsQuery.data && projectsQuery.data.length > 0,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 10_000,
    retry: 1,
  });

  const [, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((x) => x + 1), 30_000);
    return () => clearInterval(id);
  }, []);

  const items: NotificationItem[] = useMemo(() => {
    return mergeNotifications(
      (auditQuery.data ?? []) as AuditEvent[],
      (workflowRunsQuery.data ?? []) as unknown[],
      (executionsQuery.data ?? []) as unknown[],
      (recurringQuery.data ?? []) as WorkItem[],
      lastReadAt,
    );
  }, [auditQuery.data, workflowRunsQuery.data, executionsQuery.data, recurringQuery.data, lastReadAt]);

  const unreadCount = useMemo(() => items.filter((i) => i.unread).length, [items]);
  const maxOccurredAt = useMemo(() => {
    if (items.length === 0) return null;
    return items.reduce((max, it) => (it.occurredAt.getTime() > max.getTime() ? it.occurredAt : max), items[0].occurredAt);
  }, [items]);

  const markItemRead = useCallback((item: NotificationItem) => {
    markRead(item.occurredAt);
  }, [markRead]);

  const markAllRead = useCallback(() => {
    if (maxOccurredAt) markRead(maxOccurredAt);
  }, [markRead, maxOccurredAt]);

  const clearAll = useCallback(() => {
    if (maxOccurredAt) markRead(maxOccurredAt);
  }, [markRead, maxOccurredAt]);

  const isLoading = auditQuery.isLoading && workflowRunsQuery.isLoading && executionsQuery.isLoading;
  const isError = auditQuery.isError && workflowRunsQuery.isError && executionsQuery.isError;

  return {
    items,
    unreadCount,
    isLoading,
    isError,
    refetch: () => {
      auditQuery.refetch();
      workflowRunsQuery.refetch();
      executionsQuery.refetch();
      projectsQuery.refetch();
      recurringQuery.refetch();
    },
    markItemRead,
    markAllRead,
    clearAll,
    maxOccurredAt,
  };
}
