import type { ReactNode } from "react";

import { Markdown } from "@/components/markdown";

interface UserBubbleProps {
  text: string;
  source?: string;
  className?: string;
  /** The active transcript-search query, so hits are marked where they are. */
  highlight?: string;
  /**
   * actions is an OPTIONAL slot for the per-message menu (undo / revert to here).
   *
   * It is a slot rather than a boolean because this bubble has TWO callers: the Ask conversation, where the
   * operator owns the transcript and can edit it, and the execution session pane, where these messages are the
   * record of work a worker did and removing one would falsify the audit trail. A prop that turned the menu on
   * by itself would eventually be passed from the wrong place; a slot has to be filled deliberately.
   */
  actions?: ReactNode;
}

export function UserBubble({ text, source = "you", className, highlight = "", actions }: UserBubbleProps) {
  const label =
    source === "nudge"
      ? "liveness check"
      : source === "human" || source === "follow_up"
        ? "you"
        : "goal";

  return (
    <div className={`flex justify-end ${className ?? ""}`}>
      <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-user-bubble px-4 py-2.5 text-sm text-user-bubble-foreground shadow-sm">
        <div className="mb-0.5 flex items-center justify-end gap-2">
          {/* THE MENU SITS BESIDE THE LABEL, at the bubble's top-right, which is where the operator pointed:
              "in the gui there should be an elipses button next to user messages with a drop down". */}
          {actions}
          <span className="text-[10px] font-medium uppercase tracking-wide text-user-bubble-foreground/70">
            {label}
          </span>
        </div>
        <div className="break-words [overflow-wrap:anywhere]">
          {/* preserveBreaks: this is the operator's OWN text. The line breaks they typed are the line breaks
              they meant — the operator's "it's all just a bunch of text bunched up". */}
          <Markdown preserveBreaks highlight={highlight}>{text}</Markdown>
        </div>
      </div>
    </div>
  );
}
