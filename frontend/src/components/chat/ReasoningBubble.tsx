import { useState } from "react";

interface ReasoningBubbleProps {
  text: string;
  className?: string;
}

export function ReasoningBubble({ text, className }: ReasoningBubbleProps) {
  const [open, setOpen] = useState(false);
  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="max-w-[92%] rounded-xl border border-violet-300/30 bg-violet-50/20 px-3 py-2 text-left text-[13px] italic leading-relaxed text-muted-foreground dark:bg-violet-950/10"
      >
        <span className="flex items-center gap-1.5 font-medium not-italic text-violet-700 dark:text-violet-300">
          <span className="inline-block h-1.5 w-1.5 rounded-full bg-violet-500" />
          reasoning {open ? "· hide" : `· ${text.length.toLocaleString()} chars`}
        </span>
        {open && (
          <span className="mt-1 block whitespace-pre-wrap break-words">
            {text}
          </span>
        )}
      </button>
    </div>
  );
}
