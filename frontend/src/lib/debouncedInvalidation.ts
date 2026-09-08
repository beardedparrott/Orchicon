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
