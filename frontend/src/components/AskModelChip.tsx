// AskModelChip — the composer's model control: a clickable chip showing the
// model answering this conversation, which opens the three-tier model picker.
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
// The picker is rendered in a MODAL rather than as an inline dropdown because
// the composer sits at the bottom of the viewport: ModelPicker's own panel opens
// downward and would be clipped off-screen. A modal also mirrors the TUI, where
// /models opens the same picker as a modal overlay.
import { useEffect, useState } from "react";

import { ModelPicker } from "@/components/ModelPicker";

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

  // Escape closes the modal without changing anything. Bound while open only.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
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
        type="button"
        onClick={() => setOpen(true)}
        disabled={disabled}
        title={`${model || "No model set"} — click to change the model for this conversation`}
        data-testid="ask-model-chip"
        className="max-w-[18rem] shrink-0 truncate rounded-full border border-black/10 bg-black/[0.04] px-2.5 py-1 font-mono text-[11px] text-muted-foreground transition-colors hover:bg-black/5 hover:text-cyan-700 disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:hover:bg-white/10 dark:hover:text-cyan-300"
      >
        {model || "choose model"}
      </button>

      {open && (
        <div
          className="fixed inset-0 z-[200] flex items-center justify-center bg-black/40 p-4"
          onClick={() => setOpen(false)}
          data-testid="ask-model-modal"
        >
          <div
            className="glass-panel w-full max-w-xl rounded-2xl border border-black/10 p-4 dark:border-white/10"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="mb-3 flex items-center justify-between gap-3">
              <h3 className="text-sm font-medium">Ask model</h3>
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
          </div>
        </div>
      )}
    </>
  );
}
