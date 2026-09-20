// usePersistentState — localStorage-backed React state hook.
//
// Mirrors the existing panelCollapsed idiom (ask-orchicon.tsx:240-255): read
// once in the useState initializer, write inside an effect. No store/context
// needed — each page owns its sidebar state under a namespaced key.
//
// Closed is the default for first-time users (no localStorage entry = the
// `initial` default, which the host components set to `false` for `open`).

import { useCallback, useEffect, useState } from "react";

export function usePersistentState<T>(
  key: string,
  initial: T,
): [T, (value: T | ((prev: T) => T)) => void] {
  const [state, setState] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key);
      if (raw === null) return initial;
      return JSON.parse(raw) as T;
    } catch {
      return initial;
    }
  });

  useEffect(() => {
    try {
      localStorage.setItem(key, JSON.stringify(state));
    } catch {
      /* storage may be disabled (private mode) — state is best-effort */
    }
  }, [key, state]);

  const set = useCallback(
    (value: T | ((prev: T) => T)) => {
      setState((prev) =>
        typeof value === "function" ? (value as (p: T) => T)(prev) : value,
      );
    },
    [],
  );

  return [state, set];
}
