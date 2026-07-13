// useStream — server-stream subscription hook (docs/10 §4 Realtime Model).
//
// Wraps Connect-ES server-stream clients with:
//   - automatic reconnect with exponential backoff,
//   - resume from last sequence number (server supports this per
//     docs/07_API_Specification.md §4),
//   - deduplication by event_id so reconnects never double-apply,
//   - graceful degradation: a closed stream shows "disconnected,
//     retrying" without losing prior state (docs/10 invariant #4).
//
// Subscriptions are scoped to the active view: navigating away
// unsubscribes. There is no global firehose.
//
// Usage:
//   const { events, status, lastSequence } = useStream({
//     name: "project-events",
//     stream: (req) => projectClient.streamProjectEvents(req),
//     request: { projectId: id },
//     getEventId: (r) => r.event?.eventId ?? "",
//     getSequence: (r) => r.sequence,
//     filter: (r) => r.event?.projectId === id,
//   });

import { useCallback, useEffect, useRef, useState } from "react";

import type { Message, PartialMessage } from "@bufbuild/protobuf";

export type StreamStatus =
  | "idle"
  | "connecting"
  | "open"
  | "reconnecting"
  | "closed"
  | "error";

export interface UseStreamOptions<
  Req extends Message<Req>,
  Resp extends Message<Resp>,
> {
  // Human-readable name for logging/debugging.
  name: string;
  // The Connect-ES server-stream method (from a generated client).
  stream: (
    request: PartialMessage<Req>,
  ) => AsyncIterable<Resp>;
  // The request message (without from_sequence — managed internally).
  request: PartialMessage<Req>;
  // Extract the event_id from a response for deduplication.
  getEventId: (resp: Resp) => string;
  // Extract the sequence number from a response for resume.
  getSequence: (resp: Resp) => bigint | number;
  // Optional filter: only responses that return true are kept.
  filter?: (resp: Resp) => boolean;
  // Optional callback for each new event (for cache invalidation).
  onEvent?: (resp: Resp) => void;
  // Disable the stream (e.g. until a resource ID is available).
  enabled?: boolean;
  // Max backoff between reconnects (default: 30s).
  maxBackoffMs?: number;
  // Max events to keep in memory (default: 200, drop-oldest).
  maxEvents?: number;
}

export interface UseStreamResult<Resp> {
  events: Resp[];
  status: StreamStatus;
  lastSequence: bigint;
  error: Error | null;
  // Manually reconnect (resets backoff).
  reconnect: () => void;
}

export function useStream<
  Req extends Message<Req>,
  Resp extends Message<Resp>,
>(opts: UseStreamOptions<Req, Resp>): UseStreamResult<Resp> {
  const {
    name,
    stream,
    request,
    getEventId,
    getSequence,
    filter,
    onEvent,
    enabled = true,
    maxBackoffMs = 30_000,
    maxEvents = 200,
  } = opts;

  const [events, setEvents] = useState<Resp[]>([]);
  const [status, setStatus] = useState<StreamStatus>("idle");
  const [error, setError] = useState<Error | null>(null);
  const [lastSequence, setLastSequence] = useState<bigint>(0n);

  // Refs for stable iteration across renders.
  const seenIds = useRef<Set<string>>(new Set());
  const lastSeqRef = useRef<bigint>(0n);
  const reconnectAttempt = useRef(0);
  const cancelledRef = useRef(false);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const pushEvent = useCallback(
    (resp: Resp) => {
      const id = getEventId(resp);
      if (id && seenIds.current.has(id)) {
        return; // dedup
      }
      if (id) {
        seenIds.current.add(id);
      }
      if (filter && !filter(resp)) {
        return;
      }
      const seq = getSequence(resp);
      if (typeof seq === "bigint") {
        lastSeqRef.current = seq;
        setLastSequence(seq);
      } else {
        lastSeqRef.current = BigInt(seq);
        setLastSequence(BigInt(seq));
      }
      setEvents((prev) => {
        const next = [...prev, resp];
        if (next.length > maxEvents) {
          return next.slice(next.length - maxEvents);
        }
        return next;
      });
      onEvent?.(resp);
    },
    [getEventId, getSequence, filter, onEvent, maxEvents],
  );

  const connect = useCallback(async () => {
    if (cancelledRef.current) return;

    setStatus(reconnectAttempt.current > 0 ? "reconnecting" : "connecting");

    try {
      const req = {
        ...request,
        fromSequence:
          lastSeqRef.current > 0n
            ? lastSeqRef.current
            : undefined,
      };

      const iter = stream(req);

      setStatus("open");
      setError(null);
      reconnectAttempt.current = 0;

      for await (const resp of iter) {
        if (cancelledRef.current) break;
        pushEvent(resp);
      }

      // Stream ended normally (server closed).
      if (!cancelledRef.current) {
        setStatus("closed");
        scheduleReconnectRef.current();
      }
    } catch (err) {
      if (cancelledRef.current) return;
      const e = err instanceof Error ? err : new Error(String(err));
      setError(e);
      setStatus("error");
      scheduleReconnectRef.current();
    }
  }, [request, stream, pushEvent]);

  // scheduleReconnect is stored in a ref to break the circular
  // dependency between connect and scheduleReconnect.
  const scheduleReconnectRef = useRef<() => void>(() => {});

  scheduleReconnectRef.current = useCallback(() => {
    if (cancelledRef.current) return;
    reconnectAttempt.current += 1;
    const attempt = reconnectAttempt.current;
    const delay = Math.min(
      1000 * Math.pow(2, attempt - 1) + Math.random() * 500,
      maxBackoffMs,
    );
    if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
    reconnectTimer.current = setTimeout(() => {
      connect();
    }, delay);
  }, [connect, maxBackoffMs]);

  const reconnect = useCallback(() => {
    reconnectAttempt.current = 0;
    if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
    connect();
  }, [connect]);

  useEffect(() => {
    if (!enabled) {
      setStatus("idle");
      return;
    }
    cancelledRef.current = false;
    seenIds.current = new Set();
    lastSeqRef.current = 0n;
    setLastSequence(0n);
    setEvents([]);
    connect();

    return () => {
      cancelledRef.current = true;
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, name]);

  return { events, status, lastSequence, error, reconnect };
}
