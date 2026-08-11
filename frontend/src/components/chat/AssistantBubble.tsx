import { useState } from "react";
import { Markdown } from "@/components/markdown";
import { cn } from "@/lib/utils";
import { CopyButton } from "./CopyButton";

interface AssistantBubbleProps {
  text: string;
  at?: number;
  label?: string;
  className?: string;
}

export function AssistantBubble({
  text,
  at,
  label = "Orchicon",
  className,
}: AssistantBubbleProps) {
  const [open, setOpen] = useState(true);
  const [raw, setRaw] = useState(false);
  const long = text.length > 900;

  return (
    <div className={`flex justify-start ${className ?? ""}`}>
      <div
        className={cn(
          "max-w-[88%] overflow-hidden rounded-2xl rounded-tl-sm border shadow-sm",
          "border-sky-300/30 bg-sky-50/20 dark:border-sky-950/40 dark:bg-sky-950/10",
        )}
      >
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-4 py-2 text-left"
        >
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-sky-500" />
          <span className="truncate text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {label}
          </span>
          {at && (
            <span className="shrink-0 text-xs text-muted-foreground/60">
              {new Date(at).toLocaleTimeString()}
            </span>
          )}
          <span className="ml-auto shrink-0 text-xs text-muted-foreground/60">
            {text.length.toLocaleString()} chars
          </span>
          <span className="shrink-0 text-xs text-muted-foreground/60">
            {open ? "collapse" : "expand"}
          </span>
        </button>
        {open && (
          <div className="border-t border-border/40 px-4 py-3">
            <div className="mb-1 flex items-center justify-end gap-2">
              <button
                type="button"
                className="rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                onClick={() => setRaw((v) => !v)}
              >
                {raw ? "Render markdown" : "Raw text"}
              </button>
              <CopyButton text={text} />
            </div>
            <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
              {raw ? (
                <pre className="whitespace-pre-wrap font-mono text-[13px] leading-relaxed">
                  {text}
                </pre>
              ) : (
                <Markdown>{text}</Markdown>
              )}
            </div>
          </div>
        )}
        {long && !open && (
          <div className="px-4 pb-2 text-xs text-muted-foreground/60">
            {text.length.toLocaleString()} chars — click to expand
          </div>
        )}
      </div>
    </div>
  );
}
