import { useState, useEffect, useRef } from "react";

interface ReasoningBubbleProps {
  text: string;
  /** When true, the bubble is open by default (streaming in progress). */
  streaming?: boolean;
  className?: string;
}

export function ReasoningBubble({
  text,
  streaming = false,
  className,
}: ReasoningBubbleProps) {
  const [open, setOpen] = useState(streaming);
  const prevLenRef = useRef(text.length);
  const contentRef = useRef<HTMLDivElement>(null);

  // Auto-open when streaming starts.
  useEffect(() => {
    if (streaming) {
      setOpen(true);
    }
  }, [streaming]);

  // Auto-scroll the reasoning content as it grows during streaming.
  useEffect(() => {
    if (streaming && open && contentRef.current) {
      contentRef.current.scrollTop = contentRef.current.scrollHeight;
    }
    prevLenRef.current = text.length;
  }, [text, streaming, open]);

  const charCount = text.length.toLocaleString();

  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`}>
      <div className="max-w-[92%] rounded-xl border border-violet-300/30 bg-violet-50/20 dark:bg-violet-950/10 overflow-hidden">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-1.5 px-3 py-2 text-left text-[13px] font-medium not-italic text-violet-700 dark:text-violet-300"
        >
          <span
            className={`inline-block h-1.5 w-1.5 rounded-full bg-violet-500 ${
              streaming ? "animate-pulse" : ""
            }`}
          />
          reasoning
          {streaming ? (
            <span className="text-violet-700 dark:text-violet-500/70"> · thinking…</span>
          ) : (
            <span className="text-violet-700 dark:text-violet-500/70">
              {" "}
              · {charCount} chars
            </span>
          )}
          <span className="ml-auto text-violet-700 dark:text-violet-500/60 text-xs">
            {open ? "hide" : "show"}
          </span>
        </button>
        {open && (
          <div
            ref={contentRef}
            className="max-h-48 overflow-y-auto border-t border-violet-200/30 dark:border-violet-800/30 px-3 py-2"
          >
            <span className="block whitespace-pre-wrap break-words text-[13px] italic leading-relaxed text-muted-foreground">
              {text}
              {streaming && (
                <span className="inline-block h-3 w-1.5 ml-0.5 align-middle bg-violet-400/60 animate-pulse rounded-sm" />
              )}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}
