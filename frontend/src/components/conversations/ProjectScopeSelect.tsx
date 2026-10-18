// ProjectScopeSelect — the conversations sidebar's PROJECT dropdown.
//
// The operator, correcting the first attempt at project grouping:
//
//   "I think the better alternative to what you did would be a dropdown at the top of the conversation bar that
//    allows you to pick a project, and then under that project you would only see THAT PROJECT'S Conversations
//    and Categories. Projects are WORKSPACES essentially."
//
// So this chooses a SCOPE. Selecting one narrows the list below it; the category folders are unchanged and
// simply hold fewer conversations. It is deliberately NOT a second list of conversations — the first attempt
// rendered projects and categories as rival views, which is what the operator rejected.
//
// IT IS ALSO THE PER-CONVERSATION "MOVE TO PROJECT" CONTROL, in `compact` mode. That reuse is the point rather
// than a convenience: moving a conversation between projects and choosing which project to look at are the same
// question ("which project?"), and one control means one place where the option list, the archived marker and
// the "unknown project" fallback are decided. The caller passes the options it wants — the row passes them
// without "All projects", because that is a way to LOOK at things and not a place to put a conversation.
//
// Built without a new dependency, following the codebase's existing popover precedent
// (components/work-items/work-item-parent-select.tsx): open state, click-outside, Escape. The options are real
// buttons, so keyboard activation and focus order come from the platform.

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronDown, FolderClosed } from "lucide-react";

import type { ScopeOption } from "@/lib/conversationProjects";
import { cn } from "@/lib/utils";

interface ProjectScopeSelectProps {
  options: ScopeOption[];
  /** The selected scope: a project id, "" for unassigned, or ALL_PROJECTS. */
  value: string;
  onChange: (value: string) => void;
  /** Icon-only trigger, for a conversation row's hover actions. */
  compact?: boolean;
  /** Accessible name. The two usages mean different things, so they must not share a default. */
  label?: string;
  /**
   * fullWidth stretches the trigger to its container.
   *
   * For the sidebar header, where this control IS the workspace picker and sits on its own row. The default
   * caps it at 150px so it can share a row with a title, which truncates a long project name to a few
   * characters — the worst possible outcome for the control that answers "which project am I looking at?".
   */
  fullWidth?: boolean;
}

export function ProjectScopeSelect({
  options,
  value,
  onChange,
  compact = false,
  label = "Project",
  fullWidth = false,
}: ProjectScopeSelectProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const [menuStyle, setMenuStyle] = useState<React.CSSProperties>({});

  // THE MENU IS PORTALED, AND THAT IS THE FIX FOR "the project dropdown is being swallowed by the bubble".
  //
  // It used to be a plain absolutely-positioned child, which put it inside its ancestors' overflow. That is
  // fine in the SIDEBAR, but this control is also the composer row's project pill — and the composer sits in a
  // column whose every ancestor clips (`overflow-hidden` on the chat row, on <main>, on the page shell). The
  // menu was cut off at the bubble's edge; measured in a real browser, FOUR ancestors clipped it and it opened
  // downward off the bottom of the viewport.
  //
  // Fixing those ancestors is not an option: they clip the chat scroll region, which is what their
  // overflow-hidden is FOR. A portaled menu escapes the whole chain in one move, and it is the pattern this
  // codebase already uses for the sibling composer controls (mode-toggle, fullsend-toggle, AskModelChip) — so
  // the row's three popovers now behave identically instead of two of them floating and one being clipped.
  //
  // PLACEMENT, AND WHY IT NO LONGER OPENS UPWARD-ONLY.
  //
  // It used to be pinned ABOVE the trigger unconditionally (`bottom: innerHeight - rect.top + 4`), on the
  // reasoning that the composer sits at the bottom of its column so a menu below it would leave the viewport.
  // That is true of the COMPOSER and false of the SIDEBAR HEADER, where the operator reported the failure:
  //
  //   "When the project list is long, setting a project on the conversation rail goes off the screen at the
  //    top and you can't scroll through everything."
  //
  // The rail's header is at the TOP of the page, so an upward menu needed `menuHeight` of room above it — and
  // the menu is capped at `max-h-72` (288px) while a long project list fills that cap. Its top therefore landed
  // ABOVE y=0 and the first entries were unreachable: the list scrolls internally, but you cannot scroll to
  // what is off the viewport.
  //
  // So the menu now measures ITSELF and picks the side with room, using the whole viewport rather than the
  // trigger's own box: it opens DOWNWARD when there is at least a menu's height below the trigger, and upward
  // otherwise. The cap is also the viewport (`maxHeight`) rather than a fixed 288px, so a long list gets the
  // room that actually exists instead of a cap that guarantees overflow no matter which side it opens.
  //
  // It re-places on scroll (capture, so a scrolling ANCESTOR counts), because a fixed-position menu must track
  // a trigger that moves — and it observes the menu's own size (ResizeObserver) so the measured height is the
  // real one, not an assumption made before the options rendered.
  useEffect(() => {
    if (!open || !rootRef.current) return;
    const MARGIN = 8;
    const GAP = 4;
    const place = () => {
      const trigger = rootRef.current?.getBoundingClientRect();
      const menu = menuRef.current?.getBoundingClientRect();
      if (!trigger) return;
      const vh = window.innerHeight;
      const vw = window.innerWidth;
      // Measured when the portal has rendered; the cap is the fallback before it has.
      const menuH = menu?.height ?? Math.min(288, vh - 2 * MARGIN);
      const roomBelow = vh - trigger.bottom - GAP - MARGIN;
      const roomAbove = trigger.top - GAP - MARGIN;
      const openDown = roomBelow >= menuH || roomBelow >= roomAbove;
      const style: React.CSSProperties = {
        position: "fixed",
        right: `${Math.max(MARGIN, vw - trigger.right)}px`,
        minWidth: `${trigger.width}px`,
        // THE MENU CAN NEVER BE TALLER THAN THE ROOM ON THE SIDE IT CHOSE. This is the other half of the fix:
        // a fixed 288px cap is what put the header's menu off the top of the screen, because the space above a
        // top-of-page trigger is smaller than the cap.
        maxHeight: `${Math.max(120, (openDown ? roomBelow : roomAbove))}px`,
      };
      if (openDown) {
        style.top = `${trigger.bottom + GAP}px`;
      } else {
        style.bottom = `${vh - trigger.top + GAP}px`;
      }
      setMenuStyle(style);
    };
    place();
    // A second pass once the portal has laid out and the menu has its real height.
    const raf = requestAnimationFrame(place);
    window.addEventListener("resize", place);
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

  // Click-outside and Escape. Bound only while open, so a closed control adds no document listener — and the
  // listener is on `mousedown` rather than `click` so the menu closes BEFORE a click lands on whatever is
  // underneath it (a click that opened something else would otherwise be swallowed by the closing menu).
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      // The menu is PORTALED, so it is not a DOM descendant of this control — treating it as outside would
      // close the menu on the very click that selects an option.
      if (rootRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const current = options.find((o) => o.value === value);
  const currentLabel = current?.label ?? value;

  return (
    <div ref={rootRef} className={cn("relative", fullWidth && "w-full")}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={compact ? `${label}: ${currentLabel}` : undefined}
        title={compact ? `${label}: ${currentLabel}` : "Change project workspace"}
        data-testid="project-scope-trigger"
        className={cn(
          "flex items-center gap-1 rounded transition text-muted-foreground hover:text-foreground hover:bg-accent",
          compact
            ? "p-0.5"
            : cn(
                "border border-black/10 dark:border-white/10 px-2 py-1 text-xs",
                fullWidth ? "w-full" : "max-w-[150px]",
              ),
        )}
      >
        <FolderClosed aria-hidden="true" className={compact ? "h-3 w-3" : "h-3.5 w-3.5 shrink-0"} />
        {!compact && <span className="truncate font-medium text-foreground">{currentLabel}</span>}
        {/* The count of what the scope actually holds. On a project with none it reads 0, which is information:
            it is how you know the scope is empty BEFORE clicking into it. */}
        {!compact && current && (
          <span className="ml-auto shrink-0 tabular-nums text-[10px] text-muted-foreground">
            {current.count}
          </span>
        )}
        <ChevronDown aria-hidden="true" className={cn("shrink-0", compact ? "h-2.5 w-2.5" : "h-3 w-3")} />
      </button>

      {open &&
        createPortal(
        <div
          ref={menuRef}
          role="listbox"
          aria-label={label}
          data-testid="project-scope-menu"
          style={menuStyle}
          className={cn(
            // NO FIXED max-h-72. The height cap is set by the placement effect from the room on the side the
            // menu chose; a hard cap here reimposed the very limit that pushed the list off screen, and it also
            // fought the effect (a fixed class cannot be smaller than the inline style in one place and larger
            // in another without the two disagreeing).
            "z-50 min-w-[190px] overflow-y-auto overscroll-contain rounded-md border border-black/10 dark:border-white/10 bg-popover text-popover-foreground shadow-lg p-1",
            // The row's control is at the right edge of a 288px panel, so it opens leftward; the header's has
            // room and opens leftward-aligned too, since the header is the panel's own width.
          )}
        >
          {options.map((o) => (
            <button
              key={o.value || "__none__"}
              type="button"
              role="option"
              aria-selected={o.value === value}
              onClick={() => {
                onChange(o.value);
                setOpen(false);
              }}
              data-testid={`project-scope-option-${o.value || "none"}`}
              className={cn(
                "w-full flex items-center gap-2 rounded px-2 py-1.5 text-left text-xs transition",
                o.value === value ? "bg-accent/60 text-foreground" : "text-muted-foreground hover:bg-accent hover:text-foreground",
              )}
            >
              <span className="truncate">{o.label}</span>
              {/* AN ARCHIVED PROJECT SAYS SO. The association rule is "active or otherwise", so an archived
                  project is a valid workspace — and the one fact worth knowing before sending work into it is
                  that it is not active. */}
              {o.archived && (
                <span className="shrink-0 text-[10px] uppercase tracking-wide text-muted-foreground/80">
                  archived
                </span>
              )}
              <span className="ml-auto shrink-0 tabular-nums text-[10px] text-muted-foreground/80">{o.count}</span>
            </button>
          ))}
        </div>,
        document.body,
        )}
    </div>
  );
}
