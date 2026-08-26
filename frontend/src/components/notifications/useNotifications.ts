import { useMemo, useCallback, useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSession } from "@/auth/auth";
import { authClient } from "@/api/clients";
import { useNotificationCursor } from "./useNotificationCursor";
import type { AuditEvent } from "@/api/gen/orchicon/api/v1/auth_service_pb";

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
  raw: AuditEvent;
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
  // e.g. "workflow.run_started" -> "Workflow run started"
  return action.replace(/[_\.]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

function mapKind(event: AuditEvent): NotificationKind {
  const a = event.action;
  const t = event.targetType;
  // explicit workflow run events
  if (a === "workflow.run_started") return "workflow.kicked";
  if (a === "workflow.run_aborted" || a === "workflow.run_retried" || a === "workflow.run_force_progressed") return "workflow.finished";
  if (a.startsWith("workflow.run_")) return "workflow.finished";
  if (a.startsWith("workflow.")) return "workflow.finished";
  // execution terminal — infer from after status if present
  if (a === "execution.succeeded" || a === "execution.failed" || a === "execution.cancelled" || a === "execution.checkpointed") {
    if (a.includes("failed") || a.includes("cancelled")) return "execution.failed";
    return "execution.succeeded";
  }
  if (t === "execution" && a.startsWith("execution.")) {
    if (a.includes("failed") || a.includes("cancelled") || a.includes("deleted")) return "execution.failed";
    if (a.includes("succeeded") || a.includes("resumed") || a.includes("paused")) return "execution.succeeded";
    return "execution.succeeded";
  }
  if (a === "recovery.triggered" || a === "recovery.task_marked_succeeded" || a === "recovery.continuation_plan_approved") return "recovery.triggered";
  if (t === "recovery") return "recovery.triggered";
  if (a === "approval.step_approved" || a === "approval.step_rejected") return "approval.created";
  if (t === "approval") return "approval.requires_action";
  if (a.includes("schedule") || a.includes("recurring") || t.includes("schedule")) return "schedule.started";
  if (t === "work_item" && a === "work_item.started") return "schedule.started";
  return "generic";
}

function buildTitle(event: AuditEvent, kind: NotificationKind): string {
  const a = event.action;
  // Try to surface a human title from after JSON if available
  let afterName = "";
  try {
    const parsed = JSON.parse(event.after || "{}");
    afterName = parsed.name ?? parsed.title ?? parsed.display_name ?? "";
  } catch {
    // ignore
  }
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

function buildHref(event: AuditEvent, kind: NotificationKind): string {
  const tid = event.targetId;
  const ttype = event.targetType;
  switch (kind) {
    case "workflow.kicked":
    case "workflow.finished":
      if (ttype === "workflow" && tid) return `/workflows/${tid}`;
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
      return "/recurring-items";
    default:
      if (ttype === "work_item" && tid) return `/work-items/${tid}`;
      if (ttype === "project" && tid) return `/projects/${tid}`;
      if (ttype === "workflow" && tid) return `/workflows/${tid}`;
      if (ttype === "execution" && tid) return `/executions/${tid}`;
      if (ttype === "recovery" && tid) return `/recovery/${tid}`;
      if (tid) return `/${ttype}/${tid}`;
      return "/admin";
  }
}

function toDate(ts: AuditEvent["occurredAt"]): Date {
  if (!ts) return new Date(0);
  // AuditEvent.occurredAt is google.protobuf.Timestamp
  const anyTs = ts as unknown as { seconds: bigint | number; nanos: number };
  const sec = Number(anyTs.seconds ?? 0);
  const nanos = Number(anyTs.nanos ?? 0);
  return new Date(sec * 1000 + nanos / 1_000_000);
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

export function useNotifications(opts?: { enabled?: boolean }) {
  const session = useSession();
  const enabled = (opts?.enabled ?? true) && !!session.authenticated;
  const { lastReadAt, markRead } = useNotificationCursor();

  // 15s polling, visibility-aware (no background polling), stale 10s.
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

  // Ticker for relative-time labels without extra network (30s).
  const [, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((x) => x + 1), 30_000);
    return () => clearInterval(id);
  }, []);

  const items: NotificationItem[] = useMemo(() => {
    const events = auditQuery.data ?? [];
    // Already sorted newest-first by backend keyset; cap 50 and map.
    const mapped = events.slice(0, 50).map((e) => auditEventToNotification(e, lastReadAt));
    // Server already ordered; no extra sort needed. Dedupe by id.
    const seen = new Set<string>();
    const deduped: NotificationItem[] = [];
    for (const it of mapped) {
      if (seen.has(it.id)) continue;
      seen.add(it.id);
      deduped.push(it);
    }
    return deduped;
  }, [auditQuery.data, lastReadAt]);

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

  return {
    items,
    unreadCount,
    isLoading: auditQuery.isLoading,
    isError: auditQuery.isError,
    refetch: auditQuery.refetch,
    markItemRead,
    markAllRead,
    clearAll,
    maxOccurredAt,
  };
}
