import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowDown, ArrowUp } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  bottomScrollTop,
  hasArrived,
  isNearBottom,
  isNearTop,
  keyReleasesStick,
  metricsOf,
  topScrollTop,
  wheelReleasesStick,
  type ScrollEnd,
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
  // stickRef is the LIVE STICK: are we following the tail? It changes only on evidence
  // of what the OPERATOR wants — a gesture, or a scroll we did not drive. It is never
  // moved by our own scrolled positions.
  const stickRef = useRef(true);
  // driveRef is the end WE are driving the element toward, while it is still on its way
  // there (see ScrollEnd in chat-scroll.ts). It is what attributes a scroll event to an
  // INTENT rather than to a moment in time.
  const driveRef = useRef<ScrollEnd | null>(null);
  const [showJump, setShowJump] = useState(false);
  // showTop mirrors showJump at the other end: the jump-to-top control appears
  // only once there is content ABOVE the fold (see isNearTop).
  const [showTop, setShowTop] = useState(false);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const metrics = metricsOf(el);
    // A SCROLL WE ARE STILL DRIVING IS NOT THE OPERATOR'S. A smooth jump passes back
    // through the near-bottom margin on its way to the top, so its in-flight positions
    // would otherwise read as "near the bottom" and re-arm the stick the jump just
    // released — which is why a jump to the top used to be undone by the next chunk.
    if (driveRef.current !== null) {
      if (!hasArrived(metrics, driveRef.current)) return;
      driveRef.current = null; // arrived: the next scroll is the operator's
    }
    const nearBottom = isNearBottom(metrics);
    stickRef.current = nearBottom;
    setShowJump(!nearBottom);
    setShowTop(!isNearTop(metrics));
  }, []);

  // THE OPERATOR'S OWN GESTURES OUTRANK OUR SENSE OF POSITION.
  //
  // Two things happen, and both are needed:
  //
  //   - any gesture DROPS THE DRIVE, because the scroll is no longer ours to attribute.
  //     A gesture that arrives during a jump must not be ignored until the animation
  //     happens to finish.
  //   - an UPWARD gesture RELEASES THE STICK there and then, rather than waiting for a
  //     scroll event. A wheel-up can produce no event at all — the element may already
  //     be at the top, or the gesture may land in the same tick as an appended message
  //     that re-pinned it — and a release that waits for one would never happen. That is
  //     the operator's report: "when you scroll up ... the auto refresh of the live
  //     conversations brings you right back down."
  //
  // mousedown/touchstart are included because a SCROLLBAR DRAG begins with one and then
  // produces scroll events with no other gesture to explain them.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;

    const handOver = () => {
      driveRef.current = null;
    };
    const release = () => {
      driveRef.current = null;
      stickRef.current = false;
      setShowJump(true);
    };
    const onWheel = (e: WheelEvent) => {
      if (wheelReleasesStick(e.deltaY)) release();
      else handOver();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (keyReleasesStick(e.key)) release();
      else handOver();
    };

    el.addEventListener("wheel", onWheel, { passive: true });
    el.addEventListener("touchstart", handOver, { passive: true });
    el.addEventListener("mousedown", handOver);
    el.addEventListener("keydown", onKeyDown);
    return () => {
      el.removeEventListener("wheel", onWheel);
      el.removeEventListener("touchstart", handOver);
      el.removeEventListener("mousedown", handOver);
      el.removeEventListener("keydown", onKeyDown);
    };
  }, []);

  // THE PIN NEEDS NO DRIVE. Assigning the tail produces a scroll event that lands at the
  // tail, so reading it says "near the bottom" — the same answer the stick already holds.
  // Recording a drive for it would only create a target that can outlive its meaning.
  useEffect(() => {
    if (!stickRef.current) return;
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTop = bottomScrollTop(el);
  }, [items]);

  const jumpToBottom = useCallback(() => {
    stickRef.current = true;
    setShowJump(false);
    const el = scrollRef.current;
    if (!el) return;
    driveRef.current = "bottom";
    el.scrollTo({ top: bottomScrollTop(el), behavior: "smooth" });
  }, []);

  // jumpToTop scrolls to the FIRST message. It deliberately RELEASES the live
  // stick: an operator who jumped to the start of a long conversation is reading
  // history, and a streaming reply must not yank them back to the tail — the same
  // rule the wheel gesture applies. The drive is what makes the release survive the
  // journey: the animation's own in-flight positions must not be read as intent.
  const jumpToTop = useCallback(() => {
    stickRef.current = false;
    setShowTop(false);
    const el = scrollRef.current;
    if (!el) return;
    driveRef.current = "top";
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
          aria-label="Jump to latest"
          className="absolute bottom-4 left-1/2 z-10 flex -translate-x-1/2 items-center gap-1.5 rounded-full border border-border bg-background px-3 py-1.5 text-xs shadow-md hover:bg-accent"
        >
          <ArrowDown aria-hidden="true" className="h-3 w-3" />
          jump to latest
        </button>
      )}
    </div>
  );
}
