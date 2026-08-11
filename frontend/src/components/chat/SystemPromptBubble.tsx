import { useState } from "react";
import { Markdown } from "@/components/markdown";

interface SystemPromptBubbleProps {
  text: string;
  className?: string;
}

export function SystemPromptBubble({ text, className }: SystemPromptBubbleProps) {
  const [open, setOpen] = useState(text.length <= 600);
  const long = text.length > 600;

  return (
    <div className={`flex justify-end ${className ?? ""}`}>
      <div className="max-w-[92%] overflow-hidden rounded-2xl rounded-br-sm border border-amber-300/40 bg-amber-50/30 shadow-sm dark:border-amber-500/30 dark:bg-amber-500/10">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-4 py-2 text-left"
        >
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
          <span className="text-xs font-medium uppercase tracking-wide text-amber-700 dark:text-amber-300">
            system prompt
          </span>
          <span className="ml-auto shrink-0 text-xs text-muted-foreground/70">
            {text.length.toLocaleString()} chars
          </span>
          {long && (
            <span className="shrink-0 text-xs text-muted-foreground/70">
              {open ? "collapse" : "expand"}
            </span>
          )}
        </button>
        {open && (
          <div className="border-t border-border/40 px-4 py-3">
            <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
              <Markdown>{text}</Markdown>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
