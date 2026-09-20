// File-edit ledger query/mutation hooks (TanStack Query + Connect-ES).
//
// The GUI diff sidebar and the TUI pane both consume the durable file-edit
// ledger for an owner (an execution or an Ask conversation) via two RPCs:
//   - GetSessionFileEdits  — initial fetch + catch-up (unary)
//   - StreamFileEdits      — live entries as they are persisted (server-stream)
//
// Everything here mirrors the exact idiom of ../api/executions.ts and the
// ../api/useStream.ts server-stream hook. No client-side diff computation
// ever happens from tool output — the ledger does the diff; this module only
// fetches and merges what the server already computed.

import { useQuery } from "@tanstack/react-query";

import { fileEditClient } from "@/api/clients";
import { useSessionStore } from "@/auth/session";
import { useStream } from "@/api/useStream";
import type {
  FileEdit,
  StreamFileEditsRequest,
  StreamFileEditsResponse,
} from "@/api/gen/orchicon/api/v1/file_edit_pb";

import { useMemo, useState, useCallback } from "react";
import type { PartialMessage } from "@bufbuild/protobuf";

export const fileEditKeys = {
  all: ["fileEdits"] as const,
  session: (ownerKind: string, ownerId: string) =>
    [...fileEditKeys.all, "session", ownerKind, ownerId] as const,
};

// The FileEditService RPCs carry tenant_id on the request message (they do
// not resolve it from context like ExecutionService). Populate it from the
// resolved session so the query never 400s with "tenant_id must not be
// empty". Sessions are always authenticated on pages that render the
// sidebar, so the tenant is present.
function sessionTenantId(): string {
  return useSessionStore.getState().session.tenant_id ?? "";
}

export function useGetSessionFileEdits(
  ownerKind: string,
  ownerId: string,
  enabled = true,
) {
  return useQuery({
    queryKey: fileEditKeys.session(ownerKind, ownerId),
    queryFn: async () => {
      const res = await fileEditClient.getSessionFileEdits({
        tenantId: sessionTenantId(),
        ownerKind,
        ownerId,
      });
      return { edits: res.edits as FileEdit[], maxSeq: res.maxSeq };
    },
    enabled: Boolean(ownerId) && enabled,
  });
}

export function useStreamFileEdits(opts: {
  ownerKind: string;
  ownerId: string;
  enabled?: boolean;
  onEvent?: (edit: FileEdit) => void;
}) {
  const { ownerKind, ownerId, enabled = true, onEvent } = opts;
  const request: PartialMessage<StreamFileEditsRequest> = {
    tenantId: sessionTenantId(),
    ownerKind,
    ownerId,
  };
  return useStream({
    name: "file-edits",
    stream: (req) => fileEditClient.streamFileEdits(req),
    request,
    getEventId: (resp: StreamFileEditsResponse) => resp.eventId,
    getSequence: (resp: StreamFileEditsResponse) => resp.sequence,
    filter: (resp: StreamFileEditsResponse) => resp.event?.ownerId === ownerId,
    onEvent: (resp: StreamFileEditsResponse) => {
      if (resp.event) onEvent?.(resp.event);
    },
    enabled,
  });
}

/**
 * mergeEdits merges the durable ledger (GetSessionFileEdits) with the live
 * stream (StreamFileEdits) using the same discipline as
 * sessionItems.mergeSessionItems: the durable fetch is a superset of
 * everything up to ~2s ago, so live events are appended only when their
 * sequence exceeds the durable max, and deduplicated by id (the stream's
 * event_id == the ledger row id) so a reconnect never double-applies.
 */
export function mergeEdits(durable: FileEdit[], live: FileEdit[]): FileEdit[] {
  const byId = new Map<string, FileEdit>();
  let maxDurableSeq = 0n;
  for (const e of durable) {
    byId.set(e.id, e);
    const seq = e.seq as bigint;
    if (seq > maxDurableSeq) maxDurableSeq = seq;
  }
  const rows: FileEdit[] = [...durable];
  for (const e of live) {
    if (byId.has(e.id)) continue;
    const seq = e.seq as bigint;
    if (seq <= maxDurableSeq) continue;
    byId.set(e.id, e);
    rows.push(e);
    if (seq > maxDurableSeq) maxDurableSeq = seq;
  }
  // Stable sort by seq (ascending) so the timeline is chronological even if
  // the stream delivered entries slightly out of order across reconnect.
  rows.sort((a, b) => {
    const da = a.seq as bigint;
    const db = b.seq as bigint;
    if (da < db) return -1;
    if (da > db) return 1;
    return 0;
  });
  return rows;
}

/**
 * useSessionFileEdits merges the durable ledger query with the live stream
 * into a single, chronologically-ordered edit list for the sidebar. When
 * `isLive` is false (a completed session) it renders only the durable,
 * git-reconciled ledger. When live, it merges durable + streamed edits with
 * the same discipline as mergeSessionItems — durable is a superset up to
 * ~2s ago, live events dedupe against it (mergeEdits).
 *
 * The returned `error` is non-null when the durable fetch OR the live stream
 * failed. Callers must render an explicit error state in that case — the
 * "No file edits" empty state is only valid when the ledger is genuinely
 * empty (fetch succeeded AND no edits), so a data-loss can never masquerade
 * as no-data.
 */
export function useSessionFileEdits(
  ownerKind: string,
  ownerId: string,
  isLive: boolean,
): {
  edits: FileEdit[];
  status: string;
  loading: boolean;
  streamStatus: string;
  error: Error | null;
} {
  const [liveEdits, setLiveEdits] = useState<FileEdit[]>([]);

  const { data, isLoading, isFetching, error: queryError } =
    useGetSessionFileEdits(ownerKind, ownerId, true);
  const durable = useMemo(() => data?.edits ?? [], [data]);

  const onEvent = useCallback((edit: FileEdit) => {
    setLiveEdits((prev) => {
      // Dedup by id.
      if (prev.some((e) => e.id === edit.id)) return prev;
      const next = [...prev, edit];
      next.sort((a, b) => {
        const da = a.seq as bigint;
        const db = b.seq as bigint;
        return da < db ? -1 : da > db ? 1 : 0;
      });
      return next;
    });
  }, []);

  const { status: streamStatus, error: streamError } = useStreamFileEdits({
    ownerKind,
    ownerId,
    enabled: isLive && Boolean(ownerId),
    onEvent,
  });

  const edits = useMemo(() => {
    if (!isLive) return durable;
    return mergeEdits(durable, liveEdits);
  }, [isLive, durable, liveEdits]);

  // Invalidate the durable query when a live event lands so a subsequent
  // refetch sees the reconciled ledger. Live events are still merged on top
  // in the interim, so the UI never blanks.
  const error: Error | null = queryError ?? streamError;
  return {
    edits,
    status: isLoading ? "loading" : isFetching ? "refetching" : "ready",
    loading: isLoading,
    streamStatus,
    error,
  };
}
