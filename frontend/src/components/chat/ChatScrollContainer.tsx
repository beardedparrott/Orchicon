import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowDown } from "lucide-react";
import { cn } from "@/lib/utils";

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

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (Date.now() - lastAutoScrollRef.current < 200) return;
    const nearBottom =
      el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    stickRef.current = nearBottom;
    setShowJump(!nearBottom);
  }, []);

  useEffect(() => {
    if (stickRef.current && scrollRef.current) {
      lastAutoScrollRef.current = Date.now();
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [items]);

  const jumpToBottom = useCallback(() => {
    stickRef.current = true;
    setShowJump(false);
    lastAutoScrollRef.current = Date.now();
    scrollRef.current?.scrollTo({
      top: scrollRef.current.scrollHeight,
      behavior: "smooth",
    });
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
