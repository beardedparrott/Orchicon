import { useCallback, useEffect, useRef, useState } from "react";
import {
  keyReleasesStick,
  metricsOf,
  nextStick,
  wheelReleasesStick,
} from "@/lib/chat-scroll";

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
  // stickRef is "the operator is at the bottom of this block", so the auto-scroll knows
  // whether its own motion is wanted.
  const stickRef = useRef(true);

  // Auto-open when streaming starts.
  useEffect(() => {
    if (streaming) {
      setOpen(true);
    }
  }, [streaming]);

  // Auto-scroll the reasoning content as it grows during streaming — BUT ONLY WHILE THE
  // OPERATOR IS AT THE BOTTOM OF IT.
  //
  // The same rule the transcript's live stick follows, one level down, and it was missing
  // here: this effect re-pinned the block on every chunk, so an operator who scrolled up to
  // re-read a step was dragged back to the end of it by the next token. A block that yanks
  // itself is the same defect as a transcript that does, seen through a smaller window.
  //
  // The margin/re-acquire asymmetry the container uses applies here for the same reason: an
  // upward gesture leaves the view inside the near-bottom margin, so re-arming on position
  // would undo the gesture on the very scroll it caused.
  const onContentScroll = useCallback(() => {
    const el = contentRef.current;
    if (!el) return;
    const metrics = metricsOf(el);
    stickRef.current = nextStick(metrics, stickRef.current);
  }, []);

  // A gesture is what releases it, so a scroll-up is honoured from the first pixel rather
  // than only once it has escaped the margin.
  useEffect(() => {
    const el = contentRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (wheelReleasesStick(e.deltaY)) stickRef.current = false;
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (keyReleasesStick(e.key)) stickRef.current = false;
    };
    const onPointerDown = () => {
      stickRef.current = false;
    };
    el.addEventListener("wheel", onWheel, { passive: true });
    el.addEventListener("keydown", onKeyDown);
    el.addEventListener("mousedown", onPointerDown);
    el.addEventListener("touchstart", onPointerDown, { passive: true });
    return () => {
      el.removeEventListener("wheel", onWheel);
      el.removeEventListener("keydown", onKeyDown);
      el.removeEventListener("mousedown", onPointerDown);
      el.removeEventListener("touchstart", onPointerDown);
    };
  }, [open]);

  useEffect(() => {
    if (streaming && open && contentRef.current && stickRef.current) {
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
            onScroll={onContentScroll}
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
