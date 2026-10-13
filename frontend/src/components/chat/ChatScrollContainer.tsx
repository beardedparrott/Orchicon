import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowDown, ArrowUp } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  bottomScrollTop,
  isNearBottom,
  isNearTop,
  topScrollTop,
} from "@/lib/chat-scroll";

interface ChatScrollContainerProps {
  children: React.ReactNode;
  className?: string;
  /** Items that should trigger auto-scroll when they change. */
  items?: unknown[];
}

export function ChatScrollContainer({
  children,
  className,
  items,
}: ChatScrollContainerProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);
  const lastAutoScrollRef = useRef(0);
  const [showJump, setShowJump] = useState(false);
  // showTop mirrors showJump at the other end: the jump-to-top control appears
  // only once there is content ABOVE the fold (see isNearTop).
  const [showTop, setShowTop] = useState(false);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (Date.now() - lastAutoScrollRef.current < 200) return;
    const metrics = {
      scrollTop: el.scrollTop,
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
    };
    const nearBottom = isNearBottom(metrics);
    stickRef.current = nearBottom;
    setShowJump(!nearBottom);
    setShowTop(!isNearTop(metrics));
  }, []);

  useEffect(() => {
    if (stickRef.current && scrollRef.current) {
      lastAutoScrollRef.current = Date.now();
      scrollRef.current.scrollTop = bottomScrollTop(scrollRef.current);
    }
  }, [items]);

  const jumpToBottom = useCallback(() => {
    stickRef.current = true;
    setShowJump(false);
    lastAutoScrollRef.current = Date.now();
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTo({ top: bottomScrollTop(el), behavior: "smooth" });
  }, []);

  // jumpToTop scrolls to the FIRST message. It deliberately RELEASES the live
  // stick: an operator who jumped to the start of a long conversation is reading
  // history, and a streaming reply must not yank them back to the tail — the same
  // rule the wheel gesture applies.
  const jumpToTop = useCallback(() => {
    stickRef.current = false;
    setShowTop(false);
    lastAutoScrollRef.current = Date.now();
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTo({ top: topScrollTop(), behavior: "smooth" });
  }, []);

  return (
    <div className={cn("relative flex-1 min-h-0", className)}>
      <div
        ref={scrollRef}
        onScroll={onScroll}
        className="h-full overflow-y-auto"
      >
        {children}
      </div>
      {showTop && (
        <button
          type="button"
          onClick={jumpToTop}
          aria-label="Jump to top"
          className="absolute top-4 left-1/2 z-10 flex -translate-x-1/2 items-center gap-1.5 rounded-full border border-border bg-background px-3 py-1.5 text-xs shadow-md hover:bg-accent"
        >
          <ArrowUp aria-hidden="true" className="h-3 w-3" />
          jump to top
        </button>
      )}
      {showJump && (
        <button
          type="button"
          onClick={jumpToBottom}
          className="absolute bottom-4 left-1/2 z-10 flex -translate-x-1/2 items-center gap-1.5 rounded-full border border-border bg-background px-3 py-1.5 text-xs shadow-md hover:bg-accent"
        >
          <ArrowDown aria-hidden="true" className="h-3 w-3" />
          jump to latest
        </button>
      )}
    </div>
  );
}
