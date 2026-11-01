import { useEffect, useRef, useState } from "react";
import { Ellipsis, Eraser, Undo2 } from "lucide-react";

// MessageActions — the per-message menu on a USER message: undo it, or revert to it.
//
// THE OPERATOR'S TWO VERBS, and they mean different things:
//
//   Undo this message   removes EXACTLY this message. Its reply stays, so you can forget the question without
//                       losing the answer it produced.
//   Revert to here      removes this message AND everything after it, leaving the conversation ending at the
//                       previous turn.
//
// BOTH ALSO CLEAR THE CONVERSATION'S MODEL SESSION on the server, which is what makes the removal real: turns
// are continued on a stored session id, so deleting rows alone would take the message off the screen while
// leaving it in the model's context.
//
// IT IS A CONFIRM, NOT A SINGLE CLICK. Both actions destroy history and neither is undoable from the UI, so the
// menu opens a confirmation that says exactly how much goes. A menu that fires on one click is how an operator
// loses a conversation by exploring.
//
// IT IS NOT OFFERED WHILE A TURN IS RUNNING. The server refuses then (the reply is being written as it streams),
// so the trigger is disabled and says why rather than letting the click fail.

import {
  MessageAction,
  confirmationBody,
  confirmationTitle,
  type MessageActionValue,
} from "@/lib/ask-message-actions";

export { MessageAction };
export type { MessageActionValue };

export interface MessageActionsProps {
  /** Disabled with an explanation while a turn is streaming: the server refuses a delete then. */
  disabledReason?: string;
  /** Confirms and performs the action. Awaiting it keeps the confirm dialog up with a busy label. */
  onAction: (action: MessageActionValue) => Promise<void> | void;
  className?: string;
}

export function MessageActions({ disabledReason, onAction, className }: MessageActionsProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const [confirming, setConfirming] = useState<MessageActionValue | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const anchorRef = useRef<HTMLDivElement | null>(null);

  const disabled = Boolean(disabledReason);

  // Escape closes whichever layer is open, menu first. Nothing here is awaited by a turn, so closing is all it
  // means — the same rule the consent panels follow.
  useEffect(() => {
    if (!menuOpen && !confirming) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (confirming) setConfirming(null);
      else setMenuOpen(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [menuOpen, confirming]);

  // A click anywhere else closes the menu. Pointerdown rather than click so the menu is gone before the click
  // lands on whatever is underneath — otherwise dismissing the menu would also open a message under it.
  useEffect(() => {
    if (!menuOpen) return;
    const onPointerDown = (e: PointerEvent) => {
      if (anchorRef.current && !anchorRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [menuOpen]);

  const run = async (action: MessageActionValue) => {
    setBusy(true);
    setError(null);
    try {
      await onAction(action);
      setConfirming(null);
      setMenuOpen(false);
    } catch (e) {
      // SAID IN THE DIALOG, not swallowed: a failed delete that silently leaves the message reads as the button
      // not working, and the operator retries against a conversation that may have half-changed.
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={className} ref={anchorRef}>
      <div className="relative">
        <button
          type="button"
          aria-label="Message actions"
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          disabled={disabled}
          title={disabledReason}
          onClick={() => setMenuOpen((v) => !v)}
          className="rounded p-1 text-user-bubble-foreground/60 hover:bg-black/10 hover:text-user-bubble-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40 dark:hover:bg-white/10"
        >
          <Ellipsis aria-hidden="true" className="h-4 w-4" />
        </button>
        {menuOpen && !disabled && (
          <div
            role="menu"
            className="absolute right-0 z-20 mt-1 w-56 overflow-hidden rounded-md border bg-popover text-popover-foreground shadow-md"
          >
            <button
              type="button"
              role="menuitem"
              className="flex w-full items-start gap-2 px-3 py-2 text-left text-sm hover:bg-accent"
              onClick={() => {
                setMenuOpen(false);
                setConfirming(MessageAction.Undo);
              }}
            >
              <Undo2 aria-hidden="true" className="mt-0.5 h-3.5 w-3.5 shrink-0" />
              <span>
                Undo this message
                <span className="block text-xs text-muted-foreground">
                  Removes only this message. Its reply stays.
                </span>
              </span>
            </button>
            <button
              type="button"
              role="menuitem"
              className="flex w-full items-start gap-2 px-3 py-2 text-left text-sm hover:bg-accent"
              onClick={() => {
                setMenuOpen(false);
                setConfirming(MessageAction.Revert);
              }}
            >
              <Eraser aria-hidden="true" className="mt-0.5 h-3.5 w-3.5 shrink-0" />
              <span>
                Revert to here
                <span className="block text-xs text-muted-foreground">
                  Removes this message and everything after it.
                </span>
              </span>
            </button>
          </div>
        )}
      </div>

      {confirming && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" role="dialog" aria-modal="true">
          <div className="w-full max-w-md rounded-lg border bg-background p-5 shadow-lg">
            <h2 className="text-base font-semibold">
              {confirmationTitle(confirming)}
            </h2>
            <p className="mt-2 text-sm text-muted-foreground">
              {confirmationBody(confirming)}
            </p>
            <p className="mt-2 text-sm text-muted-foreground">
              This cannot be undone from the UI.
            </p>
            {error && (
              <p className="mt-3 rounded border border-destructive/50 bg-destructive/10 px-3 py-2 text-sm text-destructive [overflow-wrap:anywhere]">
                {error}
              </p>
            )}
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                className="rounded-md border px-3 py-1.5 text-sm hover:bg-accent disabled:opacity-50"
                disabled={busy}
                onClick={() => setConfirming(null)}
              >
                Cancel
              </button>
              <button
                type="button"
                className="rounded-md bg-destructive px-3 py-1.5 text-sm font-medium text-destructive-foreground hover:bg-destructive/90 disabled:opacity-50"
                disabled={busy}
                onClick={() => void run(confirming)}
              >
                {busy
                  ? "Removing…"
                  : confirming === MessageAction.Undo
                    ? "Undo message"
                    : "Revert to here"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
