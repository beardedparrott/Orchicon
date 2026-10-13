// chat-scroll.ts — the pure scroll-decision helpers behind the transcript's
// jump-to-top / jump-to-latest controls.
//
// WHY THESE LIVE OUTSIDE THE COMPONENT. The controls' behaviour is "am I at the
// bottom (so live content should pin me)?" and "am I at the top (so there is
// nothing above to jump to)?" — arithmetic over the element's metrics. Kept as
// pure functions, that arithmetic is unit-testable without a DOM (the repo's
// component tests render server-side, so a live scroll cannot be simulated);
// inlined in the component it would be reachable only through a browser.

export interface ScrollMetrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/** STICK_THRESHOLD_PX is how close to the bottom still counts as "at the bottom". */
export const STICK_THRESHOLD_PX = 80;

/** distanceFromBottom is the pixels of content below the viewport's bottom edge. */
export function distanceFromBottom(m: ScrollMetrics): number {
  return m.scrollHeight - m.scrollTop - m.clientHeight;
}

/**
 * isNearBottom reports whether the view is close enough to the tail that a new
 * message should keep it pinned there — the live-stick rule for a streaming
 * reply. This is the exact predicate the container used before it was extracted.
 */
export function isNearBottom(m: ScrollMetrics, threshold = STICK_THRESHOLD_PX): boolean {
  return distanceFromBottom(m) < threshold;
}

/**
 * isNearTop reports whether the view is close enough to the first message that a
 * jump-to-top control has nothing to do. Content ABOVE the fold is what makes the
 * control useful, so this is the same margin measured from the top.
 */
export function isNearTop(m: ScrollMetrics, threshold = STICK_THRESHOLD_PX): boolean {
  return m.scrollTop < threshold;
}

/** bottomScrollTop is the scrollTo target that lands on the latest message. */
export function bottomScrollTop(m: Pick<ScrollMetrics, "scrollHeight">): number {
  return m.scrollHeight;
}

/** topScrollTop is the scrollTo target that lands on the first message. */
export function topScrollTop(): number {
  return 0;
}
