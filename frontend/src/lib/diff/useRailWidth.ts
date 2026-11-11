// useRailWidth — observe an element's rendered width.
//
// The diff rail reads its OWN width rather than assuming it: a host-set inline
// width (the drag-resize sibling) and the default 480px must both drive the
// same side-by-side / unified decision, and the rail must re-decide when the
// operator drags it. A ResizeObserver on the rail node is the single source of
// truth; the initial read is synchronous in useLayoutEffect so the first paint
// is already correct (no unified->side-by-side flash on open).
//
// Returns 0 until the node is observable (ref null, or ResizeObserver absent
// in a non-DOM environment). Callers treat 0 as "not yet measured" — see
// sideBySideFits in ./sideBySide (0 keeps the primary side-by-side view).

import { useLayoutEffect, useState } from "react";

export function useRailWidth(ref: React.RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);

  // TWO EFFECTS, and splitting them is the point.
  //
  // The ATTACH is keyed on the NODE, not on this render. The host renders its
  // measurement target conditionally (DiffSidebar returns null while closed;
  // ActivityLine only exists while a turn is in flight), so the node APPEARS and
  // DISAPPEARS without the RefObject changing identity — which is why a deps
  // array over `[ref]` could never see it.
  //
  // It used to have no deps at all, so every render tore down and rebuilt the
  // observer for one node and forced a layout read to do it. Measured on the Ask
  // page during a streaming turn: 64 new ResizeObservers and 114
  // getBoundingClientRect calls in 20 seconds, from ONE node. At rest it was
  // zero, because the activity line is not mounted — so the cost landed exactly
  // when the pane was busiest.
  //
  // The ARRIVAL REPORT is a separate effect keyed on the observed node. The
  // attach cannot report its own reading: it runs in the commit that MOUNTED the
  // consumer, and setState there is a render-phase update React counts toward
  // its nested-update limit. Deferring it to a second effect makes a mount cost
  // two renders, once, and never again — the same reason `read()` below is
  // called only from the observer's callback after mount.
  const [node, setNode] = useState<HTMLElement | null>(null);

  useLayoutEffect(() => {
    const el = ref.current;
    setNode((prev) => (prev === el ? prev : el));
  });

  // Reports whether the REMOUNTED node has a different width, without reading
  // layout during the commit that mounted it.
  useLayoutEffect(() => {
    if (!node) return;
    const read = () => {
      const w = node.getBoundingClientRect().width;
      setWidth((prev) => (prev === w ? prev : w));
    };
    read();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(read);
    ro.observe(node);
    return () => ro.disconnect();
  }, [node]);

  return width;
}
