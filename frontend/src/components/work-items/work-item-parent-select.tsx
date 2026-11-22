// Searchable parent picker for work items (ADR-WIT-5).
//
// Replaces the plain <select> used for choosing a parent on the create
// page and the edit page's Parent card. Options render the kind badge
// (color-coded via kindMeta, theme-safe) followed by the title, and a
// text input inside the popover filters the already-fetched project
// items client-side (presentation filtering only — the server re-
// validates on submit, invariant #1).
//
// Built without a new dependency: the popover is a small self-contained
// implementation (open state + click-outside + Escape + arrow keys +
// Enter), consistent with the existing Radix-based MultiSelect. No
// business logic lives here — the caller decides which items are
// candidates (depth rule) and the component only filters/sorts them.
//
// THE MENU IS PORTALED, AND THAT IS A BUG FIX, NOT A STYLE CHOICE.
//
// The menu is anchored with `absolute` inside the Parent card. The card is a
// `Card`, whose class set includes `glass-panel` — and `backdrop-filter`
// establishes a STACKING CONTEXT. So the menu's `z-50` was scoped INSIDE the
// Parent card and could not outrank the sibling Workflow card below it, which
// paints later in DOM order: the option list rendered UNDERNEATH the next card
// and the operator could see only its top edge.
//
// Escaping one stacking context from inside is not possible; the element has to
// leave the subtree. `createPortal` lifts it to document.body, which this
// codebase already does for exactly this class of clipping in ProjectScopeSelect,
// mode-toggle, fullsend-toggle and AskModelChip — so the picker now behaves like
// every other menu rather than being the one that does not.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown, Search } from "lucide-react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { KindBadge } from "@/components/work-items/work-item-badges";
import { kindLabel } from "@/components/work-items/work-item-meta";
import { cn } from "@/lib/utils";

export interface WorkItemParentSelectProps {
  /** Candidate parents — already project-scoped by the caller. */
  items: WorkItem[];
  /** Kind of the child that will sit under the chosen parent (proto enum). */
  childKind: number;
  /** Currently selected parent id ("" = none). */
  value: string;
  onChange: (parentId: string) => void;
  /** Optional id to exclude (e.g. the item being edited cannot parent itself). */
  excludeId?: string;
  placeholder?: string;
  invalid?: boolean;
  error?: string;
}

// Hierarchy depth of a work item kind (epic=1 … subtask=4, matching the
// proto enum values). Unknown kinds map to 0 (never a valid parent).
export function depthForKind(kind: number): number {
  return kind >= 1 && kind <= 4 ? kind : 0;
}

// filterParentOptions returns the items that are valid parents for a
// child of `childKind`: strictly shallower in the hierarchy (epic >
// feature > task > subtask), not excluded, and matching the query
// (case-insensitive title search). Pure — exported for unit tests.
export function filterParentOptions(
  items: WorkItem[],
  childKind: number,
  excludeId: string | undefined,
  query: string,
): WorkItem[] {
  const childDepth = depthForKind(childKind);
  const q = query.trim().toLowerCase();
  return items
    .filter((i) => i.id !== excludeId && depthForKind(i.kind) < childDepth)
    .filter((i) => q === "" || i.title.toLowerCase().includes(q))
    .sort((a, b) => a.title.localeCompare(b.title));
}

/**
 * Compute a portaled menu's fixed position from the trigger's viewport rect and the viewport.
 *
 * PURE and exported so the placement rules are asserted in a test rather than only observed by
 * eye: which side it opens on, that it never exceeds the room it chose, and that the first
 * returned style is already out of flow (the bug that made the page jump was a menu painted in
 * normal flow at the end of document.body for one frame).
 *
 * `menuHeight` is optional because placement runs before the menu has a measured height on the
 * first pass; the fallback caps the menu to the viewport rather than to a fixed number that may
 * not fit.
 */
export function placeMenu(
  trigger: { top: number; bottom: number; left: number; width: number; height?: number },
  viewport: { width: number; height: number },
  menuHeight?: number,
): React.CSSProperties {
  const MARGIN = 8;
  const GAP = 4;
  const roomBelow = viewport.height - trigger.bottom - GAP - MARGIN;
  const roomAbove = trigger.top - GAP - MARGIN;
  const menuH = menuHeight ?? Math.min(300, viewport.height - 2 * MARGIN);
  // Open DOWNWARD whenever the menu fits below OR below is at least as roomy as above: a menu
  // that flips upward with room to spare below is disorienting next to a control near the top.
  const openDown = roomBelow >= menuH || roomBelow >= roomAbove;
  const style: React.CSSProperties = {
    position: "fixed",
    left: `${Math.max(MARGIN, trigger.left)}px`,
    minWidth: `${trigger.width}px`,
    // Never wider than the viewport, and anchored left-of-trigger if needed.
    maxWidth: `${Math.max(160, viewport.width - Math.max(MARGIN, trigger.left) - MARGIN)}px`,
    // THE CAP IS THE ROOM ON THE SIDE IT CHOSE, never a fixed max-height — a fixed cap is what
    // pushed a long list off the top of the screen in the sibling scope selector.
    maxHeight: `${Math.max(120, openDown ? roomBelow : roomAbove)}px`,
    visibility: "visible",
    // Out of flow from the FIRST painted frame: a menu in normal flow at the end of <body>
    // grows the document, which is what made the autofocus scroll the page.
    zIndex: 50,
  };
  if (openDown) style.top = `${trigger.bottom + GAP}px`;
  else style.bottom = `${viewport.height - trigger.top + GAP}px`;
  return style;
}

export function WorkItemParentSelect({
  items,
  childKind,
  value,
  onChange,
  excludeId,
  placeholder = "— Select parent —",
  invalid = false,
  error,
}: WorkItemParentSelectProps) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [highlighted, setHighlighted] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  // THE INITIAL STYLE IS NOT EMPTY, AND THAT IS THE OTHER HALF OF THE "JUMPS TO THE BOTTOM" BUG.
  //
  // The placement effect ran AFTER paint, so for the first frame the portaled menu was appended
  // to document.body with NO inline position — i.e. `position: static`, in normal flow at the
  // very END of the body. On a page taller than the viewport that puts it below the fold, and it
  // momentarily grows the document. The autofocus then scrolled that into view: the page shot to
  // the bottom, and by the time placement made the menu `fixed` the viewport had already moved,
  // leaving the operator stranded at the bottom of the page with the menu up where they clicked.
  //
  // So the FIRST render is already `fixed` (out of flow — it cannot affect page height) and
  // invisible, and placement both measures and reveals it before the browser paints.
  const [menuStyle, setMenuStyle] = useState<React.CSSProperties>({
    position: "fixed",
    top: -9999,
    left: -9999,
    visibility: "hidden",
  });

  // Only items strictly shallower than the child are valid parents
  // (epic > feature > task > subtask). Self-exclusion is the caller's
  // concern via excludeId.
  const childDepth = depthForKind(childKind);
  const candidates = useMemo(
    () => filterParentOptions(items, childKind, excludeId, query),
    [items, childKind, excludeId, query],
  );

  // The selected item only displays when it is still a valid parent for
  // the current child kind (depth rule). After the kind changes, a parent
  // that is no longer shallower shows the placeholder instead of a stale
  // badge — the server auto-resolves such a switch via the walk-up, so
  // this is presentation only (invariant #1).
  const selected = value
    ? items.find((i) => i.id === value && depthForKind(i.kind) < childDepth)
    : undefined;

  // Close on outside click. The MENU IS PORTALED, so it is not a DOM descendant
  // of rootRef — treating "not inside the trigger" as outside would close the
  // menu on the very click that picks an option.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: MouseEvent | TouchEvent) => {
      const target = e.target as Node;
      if (rootRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      setOpen(false);
    };
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("touchstart", onPointerDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("touchstart", onPointerDown);
    };
  }, [open]);

  // PLACEMENT. A fixed-position menu must be positioned in VIEWPORT coordinates,
  // measured from the trigger's own box — and re-measured while it is open,
  // because the card it belongs to scrolls with the page and a `fixed` menu does
  // not follow it on its own. It opens DOWNWARD unless there is no room below and
  // more room above, and its height cap is the room actually available on the
  // side it chose (never a fixed max-height that guarantees overflow).
  // useLayoutEffect, NOT useEffect: this both measures the trigger and REVEALS the menu, and it
  // must happen after DOM mutation but BEFORE the browser paints. With useEffect the menu was
  // briefly painted in its pre-placement state — the frame that made it visible below the fold.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const trigger = rootRef.current?.getBoundingClientRect();
      if (!trigger) return;
      const menu = menuRef.current?.getBoundingClientRect();
      setMenuStyle(
        placeMenu(
          {
            top: trigger.top,
            bottom: trigger.bottom,
            left: trigger.left,
            width: trigger.width,
            height: menu?.height,
          },
          { width: window.innerWidth, height: window.innerHeight },
        ),
      );
    };
    place();
    // A second pass once the portal has laid out and the menu has its real height.
    const raf = requestAnimationFrame(place);
    window.addEventListener("resize", place);
    // Capture phase so a scrolling ANCESTOR counts, not just the window.
    window.addEventListener("scroll", place, true);
    let ro: ResizeObserver | undefined;
    if (typeof ResizeObserver !== "undefined") {
      ro = new ResizeObserver(place);
      if (menuRef.current) ro.observe(menuRef.current);
    }
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      ro?.disconnect();
    };
  }, [open]);

  // Keep the highlighted option in view when the selection moves — by scrolling THE LIST,
  // never the page.
  //
  // `el.scrollIntoView()` was the reported "the dropdown throws me to the bottom of the
  // screen" bug: it walks every scrollable ancestor AND the window to bring the element into
  // view, and because this menu is PORTALED and `position: fixed`, that walk ends at the document.
  // So highlighting the first option on open scrolled the page itself — the menu stayed put,
  // the form moved, and the operator had to scroll back up to use the list they had just opened.
  //
  // Adjusting the list's own scrollTop touches exactly one scroll container and cannot move
  // the document.
  useEffect(() => {
    if (!open) return;
    const list = listRef.current;
    if (!list) return;
    const el = list.querySelector<HTMLElement>("[data-highlighted='true']");
    if (!el) return;
    const top = el.offsetTop;
    const bottom = top + el.offsetHeight;
    if (top < list.scrollTop) {
      list.scrollTop = top;
    } else if (bottom > list.scrollTop + list.clientHeight) {
      list.scrollTop = bottom - list.clientHeight;
    }
  }, [open, highlighted]);

  // Reset the query whenever the popover reopens.
  useEffect(() => {
    if (open) {
      setQuery("");
      setHighlighted(0);
      // Re-hide until this open is placed; a stale style from the previous open would
      // otherwise paint at the old coordinates for a frame.
      setMenuStyle({ position: "fixed", top: -9999, left: -9999, visibility: "hidden" });
      // Focus the search box after the panel renders, WITHOUT scrolling anything.
      //
      // `inputRef.current.focus()` was the "the parent dropdown throws me to the bottom of
      // the page" bug, and it is the second half of it — the earlier scrollIntoView fix only
      // addressed the list's own highlight. focus() performs a "scroll into view" on the
      // focused element as part of focusing it, and this input lives in a menu that is
      // PORTALED to document.body with `position: fixed`. A browser brings a fixed element
      // into view by scrolling THE DOCUMENT to its position, so opening the menu scrolled the
      // page — the menu stayed where it was placed, the form moved out from under the
      // operator, and they had to scroll back up.
      //
      // `preventScroll: true` keeps the focus (so typing still filters immediately, which is
      // the reason to autofocus at all) and suppresses the scroll. This is the documented
      // purpose of the option.
      requestAnimationFrame(() => inputRef.current?.focus({ preventScroll: true }));
    }
  }, [open]);

  const choose = (id: string) => {
    onChange(id);
    setOpen(false);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setHighlighted((h) => (candidates.length === 0 ? 0 : (h + 1) % candidates.length));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setHighlighted((h) =>
        candidates.length === 0 ? 0 : (h - 1 + candidates.length) % candidates.length,
      );
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (candidates[highlighted]) choose(candidates[highlighted].id);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
    }
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-invalid={invalid || undefined}
        onClick={() => setOpen((o) => !o)}
        className={cn(
          "flex h-11 sm:h-9 min-h-[44px] w-full items-center gap-2 rounded-xl glass-menu border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:ring-2 focus-visible:ring-ring",
          open && "ring-2 ring-ring",
          invalid && "border-destructive",
        )}
      >
        {selected ? (
          <>
            <KindBadge kind={selected.kind} />
            <span className="min-w-0 flex-1 truncate text-left">{selected.title}</span>
            <span className="shrink-0 text-xs text-muted-foreground">
              {kindLabel(selected.kind)}
            </span>
          </>
        ) : (
          <span className="flex-1 truncate text-left text-muted-foreground">{placeholder}</span>
        )}
        <ChevronDown aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
      </button>

      {open &&
        createPortal(
          <div
            ref={menuRef}
            style={menuStyle}
            className="z-50 overflow-hidden rounded-xl glass-menu text-popover-foreground shadow-md"
          >
          <div className="flex items-center gap-2 border-b px-2 py-1.5">
            <Search
              aria-hidden="true"
              className="h-3.5 w-3.5 shrink-0 text-muted-foreground"
            />
            <input
              ref={inputRef}
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setHighlighted(0);
              }}
              onKeyDown={onKeyDown}
              placeholder="Search parents…"
              aria-label="Search parents"
              className="h-7 w-full bg-transparent text-sm outline-none placeholder:text-muted-foreground"
            />
          </div>
          <ul
            ref={listRef}
            role="listbox"
            aria-label="Parent work items"
            className="max-h-[220px] overflow-y-auto overscroll-contain p-1"
          >
            {candidates.length === 0 ? (
              <li className="px-2 py-3 text-center text-xs text-muted-foreground">
                No matching parents
              </li>
            ) : (
              candidates.map((item, idx) => {
                const isSelected = item.id === value;
                const isHighlighted = idx === highlighted;
                return (
                  <li key={item.id} role="option" aria-selected={isSelected}>
                    <button
                      type="button"
                      data-highlighted={isHighlighted || undefined}
                      onMouseEnter={() => setHighlighted(idx)}
                      onClick={() => choose(item.id)}
                      className={cn(
                        "flex w-full cursor-pointer select-none items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm outline-none",
                        isHighlighted && "bg-accent text-accent-foreground",
                      )}
                    >
                      <KindBadge kind={item.kind} />
                      <span className="min-w-0 flex-1 truncate">{item.title}</span>
                      {isSelected && <Check aria-hidden="true" className="h-3.5 w-3.5 shrink-0" />}
                    </button>
                  </li>
                );
              })
            )}
          </ul>
          </div>,
          document.body,
        )}

      {error && (
        <p role="alert" className="mt-1 text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
