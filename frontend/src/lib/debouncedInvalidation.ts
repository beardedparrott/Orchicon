// Coalesce per-event query invalidations behind a trailing debounce.
//
// A live execution event stream can deliver dozens of events/sec (text
// deltas at token frequency). Invalidating four query keys synchronously
// per event keeps ~3-4 refetches permanently in flight — including the
// heavy limit-10000 session transcript fetch — which saturates the
// browser's ~6-connection HTTP/1.1 per-origin budget and hangs the whole
// UI. This helper collapses a burst of N events into exactly ONE
// invalidation batch after the quiet period.

export interface DebouncedInvalidation {
  /** Schedule an invalidation; resets the trailing debounce timer. */
  schedule: () => void;
  /** Fire any pending invalidation immediately (e.g. on unmount). */
  flush: () => void;
  /** Drop any pending invalidation without firing. */
  cancel: () => void;
}

export function createDebouncedInvalidation(
  invalidate: () => void,
  delayMs = 500,
): DebouncedInvalidation {
  let timer: ReturnType<typeof setTimeout> | null = null;
  return {
    schedule() {
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => {
        timer = null;
        invalidate();
      }, delayMs);
    },
    flush() {
      if (timer) {
        clearTimeout(timer);
        timer = null;
        invalidate();
      }
    },
    cancel() {
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }
    },
  };
}

/** Liveness gate for execution event streams: a stream is only opened for
 *  a non-terminal execution that has an id. Terminal executions
 *  (7/8/9/10) must never hold a stream connection. */
export function executionStreamEnabled(execId: string, isTerminal: boolean): boolean {
  return Boolean(execId) && !isTerminal;
}

/** The single liveness rule every stream-holding route shares: open a stream
 *  only for a record that EXISTS and is still in flight.
 *
 *  WHY ONE RULE. The plane serves no HTTP/2 (no h2c; a browser could not use it
 *  over cleartext anyway), so EVERY server-stream subscription costs one of the
 *  browser's ~6 connections to the origin. Four quiet routes across a few tabs
 *  exhaust that pool and every later navigation hangs on a white screen. A gate
 *  that is open on a finished record is a connection held for no reason.
 *
 *  (A collecte'd stream that is never aborted ALSO leaked; that half is fixed in
 *  api/useStream.ts, which now aborts on unmount. This is the other half: do not
 *  open one at all.) */
export function recordStreamEnabled(recordId: string, isTerminal: boolean): boolean {
  return Boolean(recordId) && !isTerminal;
}

/** WorkflowRunStatus terminal set (proto workflow.proto:211) — COMPLETED,
 *  FAILED, ABORTED. PAUSED (6) is NOT terminal: a paused run can be resumed, so
 *  its page must stay live. */
export function workflowRunIsTerminal(status: number | undefined): boolean {
  return status === 3 || status === 4 || status === 5;
}

/** RecoveryStatus terminal set (proto recovery.proto:141) — RESUMED, FAILED,
 *  CANCELLED. ESCALATED (4) and BLOCKED (7) are NOT terminal: both await a
 *  human, and a card that stops updating is the opposite of what they need. */
export function recoveryIsTerminal(status: number | undefined): boolean {
  return status === 3 || status === 5 || status === 6;
}
