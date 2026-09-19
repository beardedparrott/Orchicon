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

  // Click-outside and Escape. Bound only while open, so a closed control adds no document listener — and the
  // listener is on `mousedown` rather than `click` so the menu closes BEFORE a click lands on whatever is
  // underneath it (a click that opened something else would otherwise be swallowed by the closing menu).
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
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

      {open && (
        <div
          role="listbox"
          aria-label={label}
          data-testid="project-scope-menu"
          className={cn(
            "absolute z-30 mt-1 min-w-[190px] max-h-72 overflow-y-auto rounded-md border border-black/10 dark:border-white/10 bg-popover shadow-lg p-1",
            // The row's control is at the right edge of a 288px panel, so it opens leftward; the header's has
            // room and opens leftward-aligned too, since the header is the panel's own width.
            compact ? "right-0" : "left-0",
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
        </div>
      )}
    </div>
  );
}
