import { useCallback, useEffect, useState } from "react";
import { useSession } from "@/auth/auth";

// Key is tenant + identity scoped (ADR-4 hybrid: localStorage cursor v1).
export function notificationCursorKey(tenantId: string, identityId: string): string {
  return `orchicon:notifications:lastReadAt:${tenantId}:${identityId}`;
}

function readRaw(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export function readCursor(tenantId: string, identityId: string): Date | null {
  if (!tenantId || !identityId) return null;
  const raw = readRaw(notificationCursorKey(tenantId, identityId));
  if (!raw) return null;
  const d = new Date(raw);
  return Number.isNaN(d.getTime()) ? null : d;
}

export function writeCursor(tenantId: string, identityId: string, at: Date): void {
  if (!tenantId || !identityId) return;
  try {
    localStorage.setItem(notificationCursorKey(tenantId, identityId), at.toISOString());
    // Notify same-tab listeners.
    window.dispatchEvent(new StorageEvent("storage", { key: notificationCursorKey(tenantId, identityId), newValue: at.toISOString() } as unknown as StorageEventInit));
  } catch {
    // ignore
  }
}

export function useNotificationCursor(): {
  lastReadAt: Date | null;
  markRead: (at: Date) => void;
  markAllRead: (maxAt: Date | null) => void;
  clear: () => void;
} {
  const session = useSession();
  const tenantId = session.tenant_id ?? "";
  const identityId = session.identity_id ?? "";

  const [lastReadAt, setLastReadAt] = useState<Date | null>(() => readCursor(tenantId, identityId));

  useEffect(() => {
    setLastReadAt(readCursor(tenantId, identityId));
  }, [tenantId, identityId]);

  useEffect(() => {
    if (!tenantId || !identityId) return;
    const key = notificationCursorKey(tenantId, identityId);
    const onStorage = (e: StorageEvent) => {
      if (e.key === key) {
        setLastReadAt(e.newValue ? new Date(e.newValue) : null);
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [tenantId, identityId]);

  const markRead = useCallback((at: Date) => {
    const cur = readCursor(tenantId, identityId);
    if (!cur || at.getTime() > cur.getTime()) {
      writeCursor(tenantId, identityId, at);
      setLastReadAt(at);
    }
  }, [tenantId, identityId]);

  const markAllRead = useCallback((maxAt: Date | null) => {
    if (maxAt) markRead(maxAt);
  }, [markRead]);

  const clear = useCallback(() => {
    // Clear means mark everything currently visible as read — caller passes maxAt instead.
    // This entry exists so panel can reset storage explicitly if needed.
    if (!tenantId || !identityId) return;
    try {
      localStorage.removeItem(notificationCursorKey(tenantId, identityId));
    } catch {
      // ignore
    }
    setLastReadAt(null);
  }, [tenantId, identityId]);

  return { lastReadAt, markRead, markAllRead, clear };
}
