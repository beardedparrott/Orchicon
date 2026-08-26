import { describe, it, expect } from "vitest";
import { formatRelativeTime, auditEventToNotification } from "./useNotifications";
import { Timestamp } from "@bufbuild/protobuf";

function ts(secAgo: number): Timestamp {
  const sec = BigInt(Math.floor(Date.now() / 1000) - secAgo);
  return new Timestamp({ seconds: sec, nanos: 0 });
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
});

describe("auditEventToNotification", () => {
  it("maps workflow.run_started to kicked kind", () => {
    const ev = {
      id: "01",
      action: "workflow.run_started",
      targetType: "workflow",
      targetId: "wf123",
      after: "{}",
      occurredAt: ts(10),
    } as unknown as Parameters<typeof auditEventToNotification>[0];
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("workflow.kicked");
    expect(n.href).toBe("/workflows/wf123");
    expect(n.unread).toBe(true);
  });
  it("marks read when occurredAt <= lastReadAt", () => {
    const ev = {
      id: "02",
      action: "execution.succeeded",
      targetType: "execution",
      targetId: "ex1",
      after: "{}",
      occurredAt: ts(100),
    } as unknown as Parameters<typeof auditEventToNotification>[0];
    const lastRead = new Date(Date.now() - 10 * 1000); // newer than event
    const n = auditEventToNotification(ev, lastRead);
    expect(n.unread).toBe(false);
  });
  it("maps recovery.triggered", () => {
    const ev = {
      id: "03",
      action: "recovery.triggered",
      targetType: "recovery",
      targetId: "rec1",
      after: "{}",
      occurredAt: ts(5),
    } as unknown as Parameters<typeof auditEventToNotification>[0];
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("recovery.triggered");
    expect(n.href).toBe("/recovery/rec1");
  });
  it("generic fallback", () => {
    const ev = {
      id: "04",
      action: "project.created",
      targetType: "project",
      targetId: "proj1",
      after: "{}",
      occurredAt: ts(20),
    } as unknown as Parameters<typeof auditEventToNotification>[0];
    const n = auditEventToNotification(ev, null);
    expect(n.kind).toBe("generic");
  });
});
