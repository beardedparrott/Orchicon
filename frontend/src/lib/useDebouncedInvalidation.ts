// useDebouncedInvalidation — batch per-event query invalidations behind a
// trailing debounce. A burst of N events triggers exactly ONE invalidation
// batch after the quiet period, instead of N synchronous invalidations
// (each of which kicks off a refetch and can saturate the browser's
// per-origin HTTP/1.1 connection budget during a live stream).
//
// Returns a stable `schedule` callback to pass as the stream's onEvent.
// Any pending invalidation is flushed on unmount so the final events are
// never lost.
import { useCallback, useEffect, useRef } from "react";
import { useQueryClient, type QueryKey } from "@tanstack/react-query";
import {
  createDebouncedInvalidation,
  type DebouncedInvalidation,
} from "./debouncedInvalidation";

export function useDebouncedInvalidation(
  keys: QueryKey[],
  delayMs = 500,
): () => void {
  const qc = useQueryClient();
  const keysRef = useRef(keys);
  keysRef.current = keys;

  const invalidateRef = useRef<() => void>(() => {});
  invalidateRef.current = () => {
    for (const key of keysRef.current) {
      qc.invalidateQueries({ queryKey: key });
    }
  };

  const debouncedRef = useRef<DebouncedInvalidation | null>(null);
  if (!debouncedRef.current) {
    debouncedRef.current = createDebouncedInvalidation(
      () => invalidateRef.current(),
      delayMs,
    );
  }

  useEffect(() => {
    return () => debouncedRef.current?.flush();
  }, []);

  return useCallback(() => debouncedRef.current?.schedule(), []);
}
