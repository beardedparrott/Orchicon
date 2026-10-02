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
  MIN_CHAT_WIDTH,
  RAIL_DEFAULT_WIDTH,
  RAIL_MIN_WIDTH,
  stepRailWidth,
} from "@/lib/diff/railResize";

export interface UseRailResizeOptions {
  /** the flex row the rail is the first child of (clamps against its width) */
  containerRef: React.RefObject<HTMLElement | null>;
  /**
   * the rail's OWN outer node, when it is rendered inline in `containerRef`.
   * Used to reserve the row's OTHER fixed siblings (the Ask page's 288px
   * conversations panel) and the flex gaps, so widening the rail can never
   * push them out and squeeze the chat. Optional: without it (or when the rail
   * is a detached drawer, not a descendant of the row) only MIN_CHAT_WIDTH is
   * reserved.
   */
  railRef?: React.RefObject<HTMLElement | null>;
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
  railRef,
  width,
  onWidthChange,
}: UseRailResizeOptions): UseRailResizeResult {
  const [containerWidth, setContainerWidth] = useState(0);
  // Width the row must keep for EVERYTHING that is not the rail: the chat
  // column (MIN_CHAT_WIDTH) plus the row's fixed siblings + flex gaps. Starts
  // at the bare [rail | chat] reservation; measured from the real row below.
  const [reserved, setReserved] = useState(MIN_CHAT_WIDTH);
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
  // The rail's OWN node identity: when the rail opens (inline) or closes, the
  // ref goes null <-> element and the measurement below must re-run to (re)read
  // the row's fixed siblings. setState bails on an unchanged node.
  const [railNode, setRailNode] = useState<HTMLElement | null>(null);
  useLayoutEffect(() => {
    setNode(containerRef.current ?? null);
    setRailNode(railRef?.current ?? null);
  });

  useEffect(() => {
    if (!node || typeof node.getBoundingClientRect !== "function") return;

    const measure = () => {
      const w = node.getBoundingClientRect().width;
      if (!Number.isFinite(w) || w <= 0) return;
      setContainerWidth((prev) => (Math.abs(prev - w) < 0.5 ? prev : w));

      // Reserve the row's fixed siblings + flex gaps. The rail is the row's
      // FIRST child and the chat column (flex-1 min-w-0) is its next sibling,
      // so every OTHER rendered child (the w-72 conversations panel, a <dialog>
      // overlay) plus the inter-child gaps is width the rail must never take.
      // Only measured while the rail is genuinely inside THIS row — a detached
      // drawer is not, and then MIN_CHAT_WIDTH alone is the reservation.
      const railEl = railNode ?? railRef?.current ?? null;
      if (!railEl || !node.contains(railEl)) return;
      const children = Array.from(node.children);
      const chatEl = railEl.nextElementSibling;
      let sum = 0;
      let fixed = 0;
      for (const c of children) {
        const cw = c.getBoundingClientRect().width;
        sum += cw;
        if (c === railEl || c === chatEl) continue;
        fixed += cw;
      }
      const gaps = Math.max(0, w - sum);
      const next = MIN_CHAT_WIDTH + fixed + gaps;
      setReserved((prev) => (Math.abs(prev - next) < 0.5 ? prev : next));
    };
    measure();

    // Absent in the repo's component-test harness (no jsdom) and in any host
    // that renders without layout — measuring once above is already enough.
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(node);
    // Observe every child too: toggling the Ask page's conversations panel does
    // not resize the ROW, but it does resize the flex-1 chat column — observing
    // the children makes the reservation re-read when a fixed sibling appears
    // or disappears instead of keeping a stale (too-small) reservation.
    for (const c of Array.from(node.children)) ro.observe(c);
    return () => ro.disconnect();
  }, [node, railNode, railRef]);

  const maxWidth = maxRailWidth(containerWidth, reserved);
  const effectiveWidth = Math.max(
    RAIL_MIN_WIDTH,
    Math.min(maxWidth, Number.isFinite(width) ? width : RAIL_DEFAULT_WIDTH),
  );

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
    // preventDefault stops the browser starting a text selection / native drag
    // behind the pointer; it ALSO suppresses the default focus-on-pointerdown,
    // so focus the handle explicitly — the keyboard step keys must work right
    // after a click, not only after Tab. preventScroll keeps a click on the
    // edge from nudging the page scroll.
    e.preventDefault();
    e.currentTarget.focus?.({ preventScroll: true });
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
      onWidthChangeRef.current(clampRailWidth(desired, container, reserved));
    },
    [containerRef, reserved],
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
        onWidthChangeRef.current(clampRailWidth(next, containerWidth, reserved));
      };
      switch (e.key) {
        case "ArrowLeft": // the handle moves left → rail narrows
          clampTo(stepRailWidth(current, -1, containerWidth, reserved));
          break;
        case "ArrowRight": // the handle moves right → rail widens
          clampTo(stepRailWidth(current, 1, containerWidth, reserved));
          break;
        case "Home":
          clampTo(RAIL_MIN_WIDTH);
          break;
        case "End":
          clampTo(maxRailWidth(containerWidth, reserved));
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
    [containerWidth, reserved, release],
  );

  const onDoubleClick = useCallback(() => {
    onWidthChangeRef.current(clampRailWidth(RAIL_DEFAULT_WIDTH, containerWidth, reserved));
  }, [containerWidth, reserved]);

  // Drag cursor + selection are owned by the body: the pointer is captured by
  // the handle but visually sweeps across the chat column, so both must apply
  // there too. Cleanup is unconditional → no stuck cursor/selection after any
  // ending (pointerup, pointercancel, lost capture, unmount mid-drag).
  useEffect(() => {
    if (!dragging || typeof document === "undefined") return;
    const body = document.body.style;
    const prevCursor = body.cursor;
    const prevSelect = body.userSelect;
    body.cursor = "col-resize";
    body.userSelect = "none";
    return () => {
      body.cursor = prevCursor;
      body.userSelect = prevSelect;
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
