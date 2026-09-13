// AskModelChip — the composer's model control: a chip showing the model
// answering this conversation, which opens the three-tier model picker.
//
// It works in BOTH composer states on purpose:
//   - with a conversation OPEN, choosing a model retargets that conversation
//     (SetConversationModel, applied from the next message);
//   - on the hero ("Ask Orchicon Anything...", no conversation yet) choosing a
//     model records the choice the next conversation is CREATED with.
// The component does not care which: `onModelChange` is supplied by the parent,
// which owns that decision (there is no conversation row to write to on the
// hero, so the choice has to be held until the create).
//
// The panel is PORTALED to document.body and anchored ABOVE the composer. Both
// halves are load-bearing, and the first attempt got this wrong:
//
//   - The composer's `glass-input` / `glass-panel` classes use `backdrop-filter`,
//     which makes them a CONTAINING BLOCK for `position: fixed` descendants. A
//     non-portaled fixed overlay is therefore positioned relative to the composer
//     box and clipped by its `overflow-hidden` — it renders INSIDE the chat bar
//     and is effectively invisible.
//   - `document.body` escapes every ancestor containing block and every
//     overflow, and `bottom: innerHeight - rect.top` puts the panel above the
//     trigger rather than over it — the operator's "it needs to be ABOVE the
//     chat bar and not inside it".
//
// This mirrors components/ui/mode-toggle.tsx, which solves the identical problem
// for the mode menu that sits beside this chip in the same toolbar.
import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronDown } from "lucide-react";

import { ModelPicker } from "@/components/ModelPicker";

// PANEL_W is the preferred panel width; it shrinks on a narrow viewport.
const PANEL_W = 460;
const MARGIN = 8;

export function AskModelChip({
  model,
  onModelChange,
  disabled = false,
}: {
  model: string;
  onModelChange?: (ref: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [panelStyle, setPanelStyle] = useState<React.CSSProperties>({});

  // place positions the panel ABOVE the trigger, clamped to the viewport, and
  // never taller than the space available above it — so it is always fully
  // visible rather than clipped off-screen.
  const place = useCallback(() => {
    const el = triggerRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const width = Math.min(PANEL_W, vw - MARGIN * 2);
    // Align the panel's right edge with the trigger's, but keep it on screen.
    const right = Math.max(MARGIN, Math.min(vw - rect.right, vw - width - MARGIN));
    setPanelStyle({
      position: "fixed",
      bottom: `${vh - rect.top + MARGIN}px`, // the panel's BOTTOM sits above the trigger
      right: `${right}px`,
      width: `${width}px`,
      maxHeight: `${Math.max(180, rect.top - MARGIN * 2)}px`,
    });
  }, []);

  // Reposition while open: the transcript scrolls and the window resizes, and
  // the trigger moves with both. `scroll` is captured so scrolling CONTAINERS
  // count too, not just the window.
  useEffect(() => {
    if (!open) return;
    place();
    const onMove = () => place();
    window.addEventListener("resize", onMove);
    window.addEventListener("scroll", onMove, true);
    return () => {
      window.removeEventListener("resize", onMove);
      window.removeEventListener("scroll", onMove, true);
    };
  }, [open, place]);

  // Outside click and Escape close without changing anything.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (triggerRef.current?.contains(t) || panelRef.current?.contains(t)) return;
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

  // No handler = display only (the chip degrades to text rather than an inert
  // button, so it never looks clickable when it is not).
  if (!onModelChange) {
    return (
      <span
        className="truncate font-mono text-[11px] text-muted-foreground"
        title={model}
        data-testid="ask-model-chip-static"
      >
        {model}
      </span>
    );
  }

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen((v) => !v)}
        disabled={disabled}
        aria-expanded={open}
        aria-haspopup="dialog"
        title={`${model || "No model set"} — change the model${
          model ? " for this conversation" : " for the new conversation"
        }`}
        data-testid="ask-model-chip"
        className="inline-flex max-w-[18rem] shrink-0 items-center gap-1 rounded-full border border-black/10 bg-black/[0.04] px-2.5 py-1 font-mono text-[11px] text-muted-foreground transition-colors hover:bg-black/5 hover:text-cyan-700 disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:hover:bg-white/10 dark:hover:text-cyan-300"
      >
        <span className="truncate">{model || "choose model"}</span>
        <ChevronDown
          aria-hidden="true"
          className={`h-3 w-3 shrink-0 text-muted-foreground transition-transform ${
            open ? "rotate-180" : ""
          }`}
        />
      </button>

      {open &&
        createPortal(
          <div
            ref={panelRef}
            role="dialog"
            aria-label="Ask model"
            style={panelStyle}
            data-testid="ask-model-panel"
            className="z-[150] overflow-y-auto rounded-xl glass-menu p-3 shadow-xl"
          >
            <div className="mb-2 flex items-center justify-between gap-3">
              <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Ask model
              </h3>
              <button
                type="button"
                onClick={() => setOpen(false)}
                className="rounded px-2 py-0.5 text-xs text-muted-foreground hover:bg-accent hover:text-foreground"
              >
                close
              </button>
            </div>
            {/* askMode flags adapters that are registered but cannot serve Ask
                chat (ADR-0004 D1), so an unusable choice is flagged AT selection
                rather than failing on the first message. */}
            <ModelPicker
              value={model}
              askMode
              onChange={(ref) => {
                onModelChange(ref);
                setOpen(false);
              }}
            />
          </div>,
          document.body,
        )}
    </>
  );
}
