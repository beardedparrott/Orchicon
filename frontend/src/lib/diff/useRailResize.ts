// useRailResize — the drag/keyboard interaction for the diff rail's trailing
// edge handle. One hook so both mounts (Ask page, execution page) behave
// identically and the route components only own the persisted width.
//
// The width itself is HOST-owned + persisted (usePersistentState, per-page
// key); this hook only translates pointer/keyboard input into a clamped new
// width and reports whether a drag is in flight (so the host of the slide
// transition can suspend it — an animating edge would lag behind the pointer).
//
// Contract with the sibling "wrap / measured-width / unified" item: the host
// puts `effectiveWidth` as an INLINE width on the rail's outer node — the same
// node that item's useRailWidth observes — so its measured-width decision
// re-derives automatically on every drag. No CSS variable, no second source.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import {
  clampRailWidth,
  maxRailWidth,
  RAIL_DEFAULT_WIDTH,
  RAIL_MIN_WIDTH,
  stepRailWidth,
} from "@/lib/diff/railResize";

export interface UseRailResizeOptions {
  /** the flex row the rail is the first child of (clamps against its width) */
  containerRef: React.RefObject<HTMLElement | null>;
  /** host-owned desired width (persisted) */
  width: number;
  /** host setter; called with the clamped width */
  onWidthChange: (w: number) => void;
}

export interface UseRailResizeResult {
  /** container-clamped width, for the inline style and aria-valuenow */
  effectiveWidth: number;
  /** container-clamped ceiling, for aria-valuemax */
  maxWidth: number;
  /** true while a pointer drag is in flight */
  dragging: boolean;
  /** spread onto the handle element (ARIA + tab + pointer/keyboard handlers) */
  handleProps: React.HTMLAttributes<HTMLDivElement>;
}

export function useRailResize({
  containerRef,
  width,
  onWidthChange,
}: UseRailResizeOptions): UseRailResizeResult {
  const [containerWidth, setContainerWidth] = useState(0);
  const [dragging, setDragging] = useState(false);

  // The setter is stable on both mounts (usePersistentState memoises it), but
  // the hook must not depend on that: keep it in a ref so the effects below
  // never re-subscribe because an inline lambda changed identity.
  const onWidthChangeRef = useRef(onWidthChange);
  onWidthChangeRef.current = onWidthChange;
  const widthRef = useRef(width);
  widthRef.current = width;

  // Nodes are tracked as state so the ResizeObserver effect can key on node
  // IDENTITY. A deps-less layout effect re-reads the ref every render (the
  // node exists even while the rail is closed; a stable RefObject never
  // retriggers a deps-based effect), and setState with the same node bails
  // out — so the observer is created once, not on every drag frame.
  const [node, setNode] = useState<HTMLElement | null>(null);
  useLayoutEffect(() => {
    setNode(containerRef.current ?? null);
  });

  useEffect(() => {
    if (!node || typeof node.getBoundingClientRect !== "function") return;

    const measure = () => {
      const w = node.getBoundingClientRect().width;
      if (!Number.isFinite(w) || w <= 0) return;
      setContainerWidth((prev) => (Math.abs(prev - w) < 0.5 ? prev : w));
    };
    measure();

    // Absent in the repo's component-test harness (no jsdom) and in any host
    // that renders without layout — measuring once above is already enough.
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(node);
    return () => ro.disconnect();
  }, [node]);

  const maxWidth = maxRailWidth(containerWidth);
  const effectiveWidth = Math.max(RAIL_MIN_WIDTH, Math.min(maxWidth, Number.isFinite(width) ? width : RAIL_DEFAULT_WIDTH));

  // Re-clamp on shrink: a width stored on a wide window must be persisted back
  // clamped when the container narrows, so the chat column cannot be pushed
  // out by a stale value. Converges in one pass (the write makes them equal).
  useEffect(() => {
    if (effectiveWidth !== width) onWidthChangeRef.current(effectiveWidth);
  }, [effectiveWidth, width]);

  // Drag bookkeeping. `startWidthRef` is the width at pointerdown so an Escape
  // mid-drag can restore it; `pointerIdRef` lets us ignore foreign pointers.
  const startWidthRef = useRef(width);
  const pointerIdRef = useRef<number | null>(null);
  const draggingRef = useRef(false);
  draggingRef.current = dragging;

  const release = useCallback((el: HTMLElement | null, pointerId: number | null) => {
    if (el && pointerId !== null && el.releasePointerCapture) {
      try {
        el.releasePointerCapture(pointerId);
      } catch {
        /* capture may already be gone (e.g. the element unmounted mid-drag) */
      }
    }
    pointerIdRef.current = null;
    setDragging(false);
  }, []);

  const onPointerDown = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    startWidthRef.current = widthRef.current;
    pointerIdRef.current = e.pointerId;
    // Capture on the handle: moves keep arriving even when the pointer leaves
    // the rail, and a drag that ends outside terminates via pointerup/cancel
    // rather than leaking a half-started drag.
    e.currentTarget.setPointerCapture?.(e.pointerId);
    setDragging(true);
    // NOTE: deliberately no width write here — a click that is not a drag must
    // not change the width (the width is only written on pointermove).
  }, []);

  const onPointerMove = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      if (!draggingRef.current) return;
      if (pointerIdRef.current !== null && e.pointerId !== pointerIdRef.current) return;
      const rect = containerRef.current?.getBoundingClientRect?.();
      const container = rect?.width ?? 0;
      // The rail is the row's LEFT child, so the pointer's offset from the
      // container's left edge IS the desired rail width.
      const desired = (e.clientX ?? 0) - (rect?.left ?? 0);
      onWidthChangeRef.current(clampRailWidth(desired, container));
    },
    [containerRef],
  );

  const onPointerUp = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      if (!draggingRef.current) return;
      if (pointerIdRef.current !== null && e.pointerId !== pointerIdRef.current) return;
      release(e.currentTarget, e.pointerId);
    },
    [release],
  );

  // pointercancel = the browser took the pointer away (touch/OS gesture): end
  // the drag exactly like a pointerup so nothing hangs.
  const onPointerCancel = onPointerUp;

  const onLostPointerCapture = useCallback(() => {
    pointerIdRef.current = null;
    setDragging(false);
  }, []);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const current = widthRef.current;
      const clampTo = (next: number) => {
        e.preventDefault();
        onWidthChangeRef.current(clampRailWidth(next, containerWidth));
      };
      switch (e.key) {
        case "ArrowLeft": // the handle moves left → rail narrows
          clampTo(stepRailWidth(current, -1, containerWidth));
          break;
        case "ArrowRight": // the handle moves right → rail widens
          clampTo(stepRailWidth(current, 1, containerWidth));
          break;
        case "Home":
          clampTo(RAIL_MIN_WIDTH);
          break;
        case "End":
          clampTo(maxRailWidth(containerWidth));
          break;
        case "Escape":
          if (draggingRef.current) {
            e.preventDefault();
            onWidthChangeRef.current(startWidthRef.current);
            release(e.currentTarget, pointerIdRef.current);
          }
          break;
        default:
          break;
      }
    },
    [containerWidth, release],
  );

  const onDoubleClick = useCallback(() => {
    onWidthChangeRef.current(clampRailWidth(RAIL_DEFAULT_WIDTH, containerWidth));
  }, [containerWidth]);

  // Drag cursor is owned by the body: the pointer is captured by the handle but
  // visually sweeps across the chat column, so the resize cursor must apply
  // there too. Cleanup is unconditional → no stuck cursor after any ending.
  useEffect(() => {
    if (!dragging || typeof document === "undefined") return;
    const prev = document.body.style.cursor;
    document.body.style.cursor = "col-resize";
    return () => {
      document.body.style.cursor = prev;
    };
  }, [dragging]);

  const handleProps: React.HTMLAttributes<HTMLDivElement> = {
    role: "separator",
    "aria-orientation": "vertical",
    "aria-label": "Resize diff rail",
    "aria-valuenow": Math.round(effectiveWidth),
    "aria-valuemin": RAIL_MIN_WIDTH,
    "aria-valuemax": Math.round(maxWidth),
    tabIndex: 0,
    onPointerDown,
    onPointerMove,
    onPointerUp,
    onPointerCancel,
    onLostPointerCapture,
    onKeyDown,
    onDoubleClick,
  };

  return { effectiveWidth, maxWidth, dragging, handleProps };
}
