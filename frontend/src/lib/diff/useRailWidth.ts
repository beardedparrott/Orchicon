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

  // NOTE: NO dependency array on purpose. The host renders this component while
  // the rail is closed (it returns null), so the ref node appears only when
  // `open` flips true. A stable RefObject never retriggers a deps-based effect,
  // so deps here would read a null node forever. Re-running each render is
  // correct and cheap: `observe`/`disconnect` on one node is idempotent, and
  // setWidth is guarded so an unchanged width cannot re-render.
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;

    const read = () => {
      const w = el.getBoundingClientRect().width;
      setWidth((prev) => (prev === w ? prev : w));
    };
    // Synchronous first read — before paint.
    read();

    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(read);
    ro.observe(el);
    return () => ro.disconnect();
  });

  return width;
}
